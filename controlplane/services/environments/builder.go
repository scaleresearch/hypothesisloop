package environments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Builder builds a Dockerfile with BASE set to base and baseUserArg to the user of base's image, on
// a context holding only the file, and pushes the result as image. A *BuildFailed is the file failing to build. Any other error is the builder's own, and
// the build is tried again.
type Builder interface {
	Build(ctx context.Context, dockerfile, base, image string) error
}

// BuildFailed is a build the file failed, with the end of its output.
type BuildFailed struct{ Log string }

func (e *BuildFailed) Error() string { return "the build failed" }

const (
	// passInterval is how soon a new environment starts building.
	passInterval = 5 * time.Second
	// retryAfter is how long a build the builder failed waits before it is tried again.
	retryAfter = time.Minute
	// buildLimit bounds one build. One that runs longer failed.
	buildLimit = 30 * time.Minute
	// maxParallel is how many builds run at once.
	maxParallel = 4
)

// Loop builds every environment that is building, on the leader alone.
type Loop struct {
	store   Store
	builder Builder
	logger  *zap.Logger

	mu       sync.Mutex
	inFlight map[string]bool
	// notBefore holds back a build the builder failed, by key.
	notBefore map[string]time.Time
}

func NewLoop(store Store, builder Builder, logger *zap.Logger) *Loop {
	if store == nil || builder == nil || logger == nil {
		panic("environments.NewLoop: the store, the builder and a logger are required")
	}
	return &Loop{store: store, builder: builder, logger: logger, inFlight: map[string]bool{}, notBefore: map[string]time.Time{}}
}

// Start runs passes until ctx ends and waits for the builds it started.
func (l *Loop) Start(ctx context.Context) {
	l.logger.Info("environments: building on this replica, the leader")
	var builds sync.WaitGroup
	defer builds.Wait()
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

func (l *Loop) pass(ctx context.Context, builds *sync.WaitGroup) {
	building, err := l.store.ListBuildingEnvironments(ctx)
	if err != nil {
		l.logger.Error("environments: list building", zap.Error(err))
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, env := range building {
		if len(l.inFlight) == maxParallel {
			return
		}
		if l.inFlight[env.Key] || now.Before(l.notBefore[env.Key]) {
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
	buildCtx, cancel := context.WithTimeout(ctx, buildLimit)
	err := l.builder.Build(buildCtx, asRoot(env.Dockerfile), env.Base, env.Image)
	overrun := errors.Is(buildCtx.Err(), context.DeadlineExceeded)
	cancel()
	var failed *BuildFailed
	state, tail := domain.EnvironmentReady, ""
	switch {
	case err == nil:
	case errors.As(err, &failed):
		state, tail = domain.EnvironmentFailed, LastBytes(failed.Log, domain.MaxEnvironmentLogBytes)
	case overrun:
		state, tail = domain.EnvironmentFailed, fmt.Sprintf("the build ran longer than %s, the most one may take", buildLimit)
	case ctx.Err() != nil:
		return
	default:
		log.Warn("environments: the builder failed a build, it is tried again", zap.Error(err))
		l.mu.Lock()
		l.notBefore[env.Key] = time.Now().Add(retryAfter)
		l.mu.Unlock()
		return
	}
	if err := l.store.FinishEnvironment(ctx, env.Key, state, tail); err != nil {
		log.Error("environments: record the build", zap.Error(err))
		return
	}
	l.mu.Lock()
	delete(l.notBefore, env.Key)
	l.mu.Unlock()
	log.Info("environments: built", zap.String("state", string(state)))
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
