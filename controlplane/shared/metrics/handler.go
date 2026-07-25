package metrics

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

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
