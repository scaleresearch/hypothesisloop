package environments

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Builder builds a Dockerfile with BASE set to base, on a context holding only the file, and
// pushes the result as image. A *BuildFailed is an instruction of the file failing; any other
// error is the platform's, and the build is tried again. Pull fetches image into the builder's
// cache, so a build on it does not wait for it.
type Builder interface {
	Build(ctx context.Context, dockerfile, base, image string) error
	Pull(ctx context.Context, image string) error
}

// BuildFailed is a build the file failed, with the end of its output, at most
// domain.MaxEnvironmentLogBytes.
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

// LiveTypes is the accelerator types the builder's own cluster reports now.
type LiveTypes func(ctx context.Context) ([]string, error)

// Loop builds every environment that is building, on the leader alone.
type Loop struct {
	store   Store
	builder Builder
	// jobImages and liveTypes say which bases the loop pulls into the builder's cache once as it
	// starts: the job images that suit the hardware of the builder's own cluster. Every other base
	// is pulled by the first build on it.
	jobImages []domain.JobImage
	liveTypes LiveTypes
	logger    *zap.Logger

	mu       sync.Mutex
	inFlight map[string]bool
}

func NewLoop(store Store, builder Builder, jobImages []domain.JobImage, liveTypes LiveTypes, logger *zap.Logger) *Loop {
	if store == nil || builder == nil || liveTypes == nil || logger == nil {
		panic("environments.NewLoop: the store, the builder, the live types and a logger are required")
	}
	return &Loop{store: store, builder: builder, jobImages: jobImages, liveTypes: liveTypes, logger: logger, inFlight: map[string]bool{}}
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

// warm pulls the job images that suit the hardware of the builder's cluster into its cache, one
// after another, each once and within buildLimit. A base's address names its tag, and the tag
// changes only with a restart.
func (l *Loop) warm(ctx context.Context) {
	types, err := l.liveTypes(ctx)
	if err != nil {
		l.logger.Warn("environments: the hardware to warm the builder's cache for", zap.Error(err))
		return
	}
	bases := suitedImages(l.jobImages, types)
	l.logger.Info("environments: warming the builder's cache", zap.Strings("bases", bases))
	began := time.Now()
	for _, base := range bases {
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

// suitedImages is the addresses of the images that suit one of types.
func suitedImages(images []domain.JobImage, types []string) []string {
	var out []string
	for _, image := range images {
		if slices.ContainsFunc(types, image.SuitsType) {
			out = append(out, image.Image)
		}
	}
	return out
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
	err := l.builder.Build(buildCtx, env.Dockerfile, env.Base, env.Image)
	cancel()
	var failed *BuildFailed
	state, tail := domain.EnvironmentReady, ""
	switch {
	case err == nil:
	case errors.As(err, &failed):
		state, tail = domain.EnvironmentFailed, failed.Log
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

// lastBytes keeps the end of text, from the start of a line where one starts inside the limit.
func lastBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline < len(text)-1 {
		text = text[newline+1:]
	}
	return strings.ToValidUTF8(text, "")
}
