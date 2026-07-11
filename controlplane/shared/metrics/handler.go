package metrics

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// PushRequest is the payload sent by openresearch-node-agent every 2 seconds.
type PushRequest struct {
	Node      string      `json:"node"`
	Timestamp time.Time   `json:"timestamp"`
	Pods      []PodSample `json:"pods"`
}

// PodSample is a single pod's CPU utilization reading, tagged with the experiment it belongs to
// (from the pod's openresearch.io/experiment-id label — resolved by the node-agent, not us).
type PodSample struct {
	PodUID       string  `json:"pod_uid"`
	ExperimentID string  `json:"experiment_id,omitempty"`
	CPUUtilPct   float64 `json:"cpu_util_pct"`
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
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
