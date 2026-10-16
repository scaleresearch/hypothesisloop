package metricsingest

import (
	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// A push may be buffered and retried through a network outage, so it is legitimately backdated;
// it can never legitimately be from the future beyond ordinary clock jitter.
const (
	maxPushBacklog = 24 * time.Hour
	maxPushSkew    = 5 * time.Minute
)

// maxPushBytes bounds one node's push, a few hundred bytes per pod it runs.
const maxPushBytes = 4 << 20

func validPushTime(at, now time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("push has no timestamp")
	}
	if at.After(now.Add(maxPushSkew)) {
		return fmt.Errorf("push timestamp %s is in the future", at)
	}
	if at.Before(now.Add(-maxPushBacklog)) {
		return fmt.Errorf("push timestamp %s is older than %s", at, maxPushBacklog)
	}
	return nil
}

// PushRequest is the fresh observation payload sent by hypothesisloop-node-agent, and PodSample
// one pod of it, tagged by the node-agent with the experiment it belongs to.
type (
	PushRequest = clusterapi.NodeMetricsPush
	PodSample   = clusterapi.PodSample
)

// Placements resolves where each named job is placed. Implemented by db.ExperimentsStore.
type Placements interface {
	PlacementByID(ctx context.Context, ids []string) (map[string]domain.Placement, error)
}

// NewPushHandler returns an HTTP handler that accepts node-agent push payloads and writes every
// tagged sample to GreptimeDB as an observed-alive heartbeat (metricsdb.RecordObservations) in one
// remote-write request per push, using the agent's own collection timestamp (req.Timestamp), so a
// payload buffered through a network blip still lands at the moment it happened.
//
// It must sit behind auth.Gate and auth.RequireRuntime: a sample counts only for a job placed on the cluster
// the token names, since a heartbeat keeps a job alive and is what its runtime is billed from.
//
// Stateless: every reader (silence detection, quota accounting, Cancel) recomputes from
// GreptimeDB, so there is no cache here to fall out of sync with it.
func NewPushHandler(dbURL string, placements Placements) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		caller, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var req PushRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPushBytes)).Decode(&req); err != nil {
			if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// The timestamp is the agent's own collection time, which is exactly why it must be
		// bounded: it is written straight into the series billing is computed from, so a pod on a
		// node with a skewed clock (or a payload with no timestamp at all, which unmarshals to
		// year 1) corrupts elapsed time for every job on that node.
		if err := validPushTime(req.Timestamp, time.Now().UTC()); err != nil {
			log.Printf("metrics: rejecting push from node %s: %v", req.Node, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ids := make([]string, 0, len(req.Pods))
		for _, p := range req.Pods {
			if p.ExperimentID != "" {
				ids = append(ids, p.ExperimentID)
			}
		}
		placed, err := placements.PlacementByID(r.Context(), ids)
		if err != nil {
			log.Printf("metrics: resolve placements for node %s: %v", req.Node, err)
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		observations := make([]metricsdb.Observation, 0, len(req.Pods))
		for _, p := range req.Pods {
			if p.ExperimentID == "" {
				continue
			}
			// As with a status report, a sample for a job this cluster does not hold is dropped
			// and the rest of the push stands: mid-reschedule that is the ordinary state.
			if !ownSample(caller, placed, p.ExperimentID) {
				log.Printf("metrics: dropping a sample for job %s, which is not placed on cluster %s", p.ExperimentID, caller.ClusterName)
				continue
			}
			observations = append(observations, metricsdb.Observation{
				ExperimentID: p.ExperimentID,
				At:           req.Timestamp,
				ExtraLabels:  map[string]string{"pod_uid": p.PodUID, "node": req.Node},
			})
		}
		if len(observations) > 0 {
			if err := metricsdb.RecordObservations(r.Context(), dbURL, observations); err != nil {
				log.Printf("metrics: record observations for node %s: %v", req.Node, err)
				// Non-2xx tells node-agent's retry queue to keep this payload and retry later —
				// acking a write GreptimeDB never received would let silence-eviction mistake a
				// storage outage for genuinely dead jobs.
				http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func ownSample(caller auth.Principal, placed map[string]domain.Placement, experimentID string) bool {
	if caller.Kind == auth.KindStaff {
		return true
	}
	placement, found := placed[experimentID]
	return found && caller.ClusterName != "" && placement.ClusterName == caller.ClusterName
}
