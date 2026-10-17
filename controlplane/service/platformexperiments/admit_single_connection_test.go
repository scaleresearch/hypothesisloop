package platformexperiments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// inTxStore runs observe the way the real transactions do and fails any pool read made while one
// is open: that read needs a second connection the transaction's own request is already holding.
type inTxStore struct {
	fakeStore
	inTx bool
}

func (f *inTxStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	if f.inTx {
		return nil, errors.New("pool read inside a quota transaction")
	}
	return testPE, nil
}

func (f *inTxStore) AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observe func(context.Context) (*domain.AgentQuota, error), _ db.SubmissionRateLimit, _ func(context.Context) error) (db.AdmitDecision, error) {
	f.inTx = true
	defer func() { f.inTx = false }()
	if _, err := observe(ctx); err != nil {
		return db.AdmitRejected, err
	}
	return db.AdmitInserted, nil
}

func (f *inTxStore) ReserveAdmittedFlavorTx(ctx context.Context, _ string, _ domain.AcceleratorType, _ float64, _, _ int, observe func(context.Context, string, string, time.Time) (*domain.AgentQuota, error)) (string, error) {
	f.inTx = true
	defer func() { f.inTx = false }()
	_, err := observe(ctx, "agent-1", testPE.ID, testPE.CreatedAt)
	return "", err
}

func TestAdmissionObservesUsageWithoutASecondPoolConnection(t *testing.T) {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer metrics.Close()
	svc := NewPlatformExperimentsService(&inTxStore{}, domain.QuotaConfig{}, zap.NewNop(), metricsdb.New(metrics.URL), 3*time.Minute)
	ctx := context.Background()

	exp := &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: testPE.ID}
	decision, err := svc.AdmitExperiment(ctx, exp, db.SubmissionRateLimit{}, func(context.Context) error { return nil })
	if err != nil || decision != db.AdmitInserted {
		t.Fatalf("AdmitExperiment: got = (%v, %v), want = (%v, nil)", decision, err, db.AdmitInserted)
	}
	if err := svc.ReserveAdmittedFlavor(ctx, exp.ID, "h100", 1, 1); err != nil {
		t.Fatalf("ReserveAdmittedFlavor: got = %v, want = nil", err)
	}
}
