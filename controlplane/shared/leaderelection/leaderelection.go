// Package leaderelection makes exactly one control-service replica active for work that is only
// safe single-threaded across the whole deployment: the scheduler's admission tick (reads
// capacity, decides, writes — not a CAS) and the JobWatcher scan. Every other replica stays in
// standby.
//
// The lock is a Postgres session-level advisory lock: it lives on one dedicated connection and is
// released automatically by Postgres the instant that connection closes, including on a hard
// process crash — no explicit unlock, heartbeat table, or TTL bookkeeping needed.
package leaderelection

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// SchedulerLockKey is the fixed advisory-lock key the scheduler leader holds. Arbitrary but
// stable — changing it would let two replicas both think they're leader across a rolling deploy.
const SchedulerLockKey = 72901 // arbitrary fixed key, do not change once deployed

// Run blocks until ctx is cancelled, alternating between standby (retrying
// pg_try_advisory_lock every retryInterval) and leader (holding one dedicated connection and
// running onLeader in a context that's cancelled the moment leadership is lost). onLeader must
// return once its context is cancelled; Run waits for it before releasing the connection and
// returning to standby.
func Run(ctx context.Context, pool *db.Pool, key int64, retryInterval time.Duration, logger *zap.Logger, onLeader func(ctx context.Context)) {
	for ctx.Err() == nil {
		lock, err := pool.TryAdvisoryLock(ctx, key)
		if err != nil {
			logger.Warn("leaderelection: try advisory lock", zap.Error(err))
		}
		if lock == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
			continue
		}

		logger.Info("leaderelection: acquired scheduler leadership")
		leaderCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			onLeader(leaderCtx)
		}()

		holdUntilLost(leaderCtx, lock, retryInterval, logger)
		cancel()
		<-done
		lock.Release()
		logger.Warn("leaderelection: lost scheduler leadership")
	}
}

// holdUntilLost blocks until the leader connection is no longer healthy or ctx is cancelled. A
// dead connection is exactly what releases the advisory lock (Postgres does this automatically
// on connection close), so probing liveness here is what lets another replica detect and take
// over leadership within one retryInterval of this process dying.
func holdUntilLost(ctx context.Context, lock *db.AdvisoryLock, retryInterval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := lock.Ping(ctx); err != nil {
				logger.Warn("leaderelection: leader connection lost", zap.Error(err))
				return
			}
		}
	}
}
