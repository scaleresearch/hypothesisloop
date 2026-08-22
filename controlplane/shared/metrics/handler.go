package metrics

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// A push may be buffered and retried through a network outage, so it is legitimately backdated;
// it can never legitimately be from the future beyond ordinary clock jitter.
const (
	maxPushBacklog = 24 * time.Hour
	maxPushSkew    = 5 * time.Minute
)

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

// PushRequest is the fresh observation payload sent by hypothesisloop-node-agent.
type PushRequest struct {
	Node      string      `json:"node"`
	Timestamp time.Time   `json:"timestamp"`
	Pods      []PodSample `json:"pods"`
}

// PodSample is a pod execution observation tagged with the experiment it belongs to
// (from the pod's hypothesisloop.io/experiment-id label — resolved by the node-agent, not us).
type PodSample struct {
	PodUID       string `json:"pod_uid"`
	ExperimentID string `json:"experiment_id,omitempty"`
}

// NewPushHandler returns an HTTP handler that accepts node-agent push payloads and writes every
// tagged sample to GreptimeDB as an observed-alive heartbeat (metricsdb.RecordObservations) in one
// remote-write request per push — a node reporting N pods sends one push, and that becomes one
// write to GreptimeDB, not N — using the agent's own collection timestamp (req.Timestamp), not
// receipt time, so a payload delayed or buffered through a network blip and flushed later still
// lands at the moment it actually happened, not the moment it happened to arrive.
//
// Deliberately stateless: this handler holds nothing in memory between requests. Every reader
// (silence detection, quota accounting, Cancel) recomputes straight from GreptimeDB, so there is
// no cache here to fall out of sync with it, drop on restart, or diverge between processes.
func NewPushHandler(dbURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req PushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		observations := make([]metricsdb.Observation, 0, len(req.Pods))
		for _, p := range req.Pods {
			if p.ExperimentID == "" {
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
