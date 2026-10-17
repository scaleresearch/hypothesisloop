package platformexperiments

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// DataKeyStore is what the data key sweep reads and writes (db.Store).
type DataKeyStore interface {
	EndedDataKeys(ctx context.Context, grace db.DataKeyGrace) ([]db.EndedDataKey, error)
	DropDataKey(ctx context.Context, key db.EndedDataKey, grace db.DataKeyGrace, deleteKey func(ctx context.Context, accessKeyID string) error) error
}

// DataKeySweep takes the object-store key of every job and agent whose access ended out of the
// store. It is the one path a key leaves by once its holder ends, so a deletion that failed or a
// restart in between leaves the key to the next pass.
type DataKeySweep struct {
	store     DataKeyStore
	deleteKey func(ctx context.Context, accessKeyID string) error
	grace     db.DataKeyGrace
	logger    *zap.Logger
}

func NewDataKeySweep(store DataKeyStore, deleteKey func(ctx context.Context, accessKeyID string) error, grace db.DataKeyGrace, logger *zap.Logger) *DataKeySweep {
	return &DataKeySweep{store: store, deleteKey: deleteKey, grace: grace, logger: logger}
}

// Start runs Sweep now and every interval until ctx ends.
func (d *DataKeySweep) Start(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		if err := d.Sweep(passCtx); err != nil {
			d.logger.Error("data_key_sweep: scan", zap.Error(err))
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep drops every ended key once. A key that fails is logged and left to the next pass.
func (d *DataKeySweep) Sweep(ctx context.Context) error {
	keys, err := d.store.EndedDataKeys(ctx, d.grace)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := d.store.DropDataKey(ctx, key, d.grace, d.deleteKey); err != nil {
			d.logger.Warn("data_key_sweep: drop key", zap.String("experimentID", key.ExperimentID),
				zap.String("platformExpID", key.PlatformExperimentID), zap.String("agentID", key.AgentID), zap.Error(err))
			continue
		}
		d.logger.Info("data_key_sweep: dropped key", zap.String("experimentID", key.ExperimentID),
			zap.String("platformExpID", key.PlatformExperimentID), zap.String("agentID", key.AgentID))
	}
	return nil
}
