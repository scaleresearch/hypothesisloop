package db

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SchedulerLockKey is the advisory-lock key control-service's leader holds. Changing a key lets
// two replicas both lead across a rolling deploy.
const SchedulerLockKey = 72901

// ControllerLockKey is the advisory-lock key metrics-service's leader holds.
const ControllerLockKey = 72902

// RunAsLeader blocks until ctx is cancelled, alternating between standby, trying the advisory lock key
// every interval, and leading: holding the lock on one dedicated connection and running every
// loop in a context cancelled the moment leadership is lost. Each loop must return once its
// context is cancelled. The lock is released only after every loop has returned, so no two
// replicas ever run them at once. A ping of the lock's connection that does not answer within
// interval is leadership lost: Postgres may have dropped the session, and its lock with it.
func RunAsLeader(ctx context.Context, pool *Pool, key int64, interval time.Duration, logger *zap.Logger, loops ...func(context.Context)) {
	logger = logger.With(zap.Int64("lockKey", key))
	for ctx.Err() == nil {
		lock, err := pool.TryAdvisoryLock(ctx, key)
		if err != nil {
			logger.Warn("leaderelection: try advisory lock", zap.Error(err))
		}
		if lock == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			continue
		}

		logger.Info("leaderelection: acquired leadership")
		leaderCtx, cancel := context.WithCancel(ctx)
		var running sync.WaitGroup
		for _, loop := range loops {
			running.Go(func() { loop(leaderCtx) })
		}
		holdUntilLost(leaderCtx, lock, interval, logger)
		cancel()
		running.Wait()
		lock.Release()
		logger.Warn("leaderelection: lost leadership")
	}
}

func holdUntilLost(ctx context.Context, lock *AdvisoryLock, interval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := lock.Ping(pingCtx)
			cancel()
			if err != nil {
				logger.Warn("leaderelection: leader connection lost", zap.Error(err))
				return
			}
		}
	}
}
