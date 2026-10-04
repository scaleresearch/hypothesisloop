package environments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Builder builds a Dockerfile with BASE set to base and baseUserArg to the user of base's image, on
// a context holding only the file, and pushes the result as image. A *BuildFailed is an
// instruction of the file failing. Any other error is the platform's, and the build is tried
// again. Pull fetches image into the builder's cache, so a build on it does not wait for it.
type Builder interface {
	Build(ctx context.Context, dockerfile, base, image string) error
	Pull(ctx context.Context, image string) error
}

// BuildFailed is a build the file failed, with the end of its output.
type BuildFailed struct{ Log string }

func (e *BuildFailed) Error() string { return "the build failed" }

const (
	// passInterval is how soon a new environment starts building.
	passInterval = 5 * time.Second
	// retryAfter is how long a build the platform failed waits before it is tried again.
	retryAfter = time.Minute
	// maxAttempts is how many builds of one environment the platform fails before it abandons it.
	maxAttempts = 3
	// buildLimit bounds one build, and one pull into the cache.
	buildLimit = 30 * time.Minute
	// maxParallel is how many builds run at once.
	maxParallel = 4
)

// Loop builds every environment that is building, on the leader alone.
type Loop struct {
	store   Store
	builder Builder
	// bases are the images environments are built on, pulled into the builder's cache as the loop
	// starts.
	bases  []string
	logger *zap.Logger

	mu       sync.Mutex
	inFlight map[string]bool
}

func NewLoop(store Store, builder Builder, bases []string, logger *zap.Logger) *Loop {
	if store == nil || builder == nil || len(bases) == 0 || logger == nil {
		panic("environments.NewLoop: the store, the builder, the bases and a logger are required")
	}
	return &Loop{store: store, builder: builder, bases: bases, logger: logger, inFlight: map[string]bool{}}
}

// Start warms the builder's cache, runs passes until ctx ends and waits for what it started.
func (l *Loop) Start(ctx context.Context) {
	l.logger.Info("environments: building on this replica, the leader")
	var builds sync.WaitGroup
	defer builds.Wait()
	builds.Add(1)
	go func() {
		defer builds.Done()
		l.warm(ctx)
	}()
	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	for {
		l.pass(ctx, &builds)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// warm pulls every base into the builder's cache, one after another.
func (l *Loop) warm(ctx context.Context) {
	l.logger.Info("environments: warming the builder's cache", zap.Strings("bases", l.bases))
	began := time.Now()
	for _, base := range l.bases {
		start := time.Now()
		pullCtx, cancel := context.WithTimeout(ctx, buildLimit)
		err := l.builder.Pull(pullCtx, base)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			l.logger.Warn("environments: warm the builder's cache", zap.String("base", base), zap.Duration("took", time.Since(start)), zap.Error(err))
			continue
		}
		l.logger.Info("environments: pulled into the builder's cache", zap.String("base", base), zap.Duration("took", time.Since(start)))
	}
	l.logger.Info("environments: warmed the builder's cache", zap.Duration("took", time.Since(began)))
}

func (l *Loop) pass(ctx context.Context, builds *sync.WaitGroup) {
	building, err := l.store.ListBuildingEnvironments(ctx, retryAfter)
	if err != nil {
		l.logger.Error("environments: list building", zap.Error(err))
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, env := range building {
		if len(l.inFlight) == maxParallel {
			return
		}
		if l.inFlight[env.Key] {
			continue
		}
		l.inFlight[env.Key] = true
		builds.Add(1)
		go func() {
			defer builds.Done()
			l.build(ctx, env)
			l.mu.Lock()
			delete(l.inFlight, env.Key)
			l.mu.Unlock()
		}()
	}
}

func (l *Loop) build(ctx context.Context, env *domain.Environment) {
	log := l.logger.With(zap.String("environment", env.Key), zap.String("base", env.Base))
	start := time.Now()
	buildCtx, cancel := context.WithTimeout(ctx, buildLimit)
	err := l.builder.Build(buildCtx, asRoot(env.Dockerfile), env.Base, env.Image)
	cancel()
	var failed *BuildFailed
	state, tail := domain.EnvironmentReady, ""
	switch {
	case err == nil:
	case errors.As(err, &failed):
		state, tail = domain.EnvironmentFailed, LastBytes(failed.Log, domain.MaxEnvironmentLogBytes)
	case ctx.Err() != nil:
		return
	default:
		abandoned, recordErr := l.store.FailEnvironmentAttempt(ctx, env.Key, maxAttempts)
		if recordErr != nil {
			log.Error("environments: record a build the platform failed", zap.Error(recordErr))
			return
		}
		log.Warn("environments: the platform failed a build", zap.Int("attempt", env.Attempts+1), zap.Bool("abandoned", abandoned), zap.Duration("took", time.Since(start)), zap.Error(err))
		return
	}
	if err := l.store.FinishEnvironment(ctx, env.Key, state, tail); err != nil {
		log.Error("environments: record the build", zap.Error(err))
		return
	}
	log.Info("environments: built", zap.String("state", string(state)), zap.Duration("took", time.Since(start)))
}

// LastBytes keeps the end of text, from the start of a line where one starts inside the limit.
func LastBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline < len(text)-1 {
		text = text[newline+1:]
	}
	return strings.ToValidUTF8(text, "")
}
