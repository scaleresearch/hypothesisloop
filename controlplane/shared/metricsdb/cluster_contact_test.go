package metricsdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
)

// The heartbeat carries the version the agent said it runs, and the last contact of each cluster
// is read back with it, however long ago it was within the lookback.
func TestTheHeartbeatCarriesTheAgentVersionReadBackWithTheLastContact(t *testing.T) {
	var heartbeat map[string]string
	writer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Error(err)
			return
		}
		var req rwWriteRequest
		if err := proto.Unmarshal(decoded, &req); err != nil {
			t.Error(err)
			return
		}
		for _, ts := range req.Timeseries {
			labels := map[string]string{}
			for _, l := range ts.Labels {
				labels[l.Name] = l.Value
			}
			if labels["__name__"] == clusterHeartbeatMetric {
				heartbeat = labels
			}
		}
	}))
	defer writer.Close()
	if err := RecordClusterCapacitySnapshot(context.Background(), writer.URL, ClusterCapacitySnapshot{ClusterName: "c", ClusterID: "uid-c", AgentVersion: "abc1234", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if heartbeat["agent_version"] != "abc1234" || heartbeat["cluster_id"] != "uid-c" {
		t.Fatalf("heartbeat labels = %v, want the agent's version beside its identity", heartbeat)
	}

	var asked string
	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("sql")
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"cluster_name"},{"name":"cluster_id"},{"name":"agent_version"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},`+
			`"rows":[["tt-box","uid-tt","old1234",1760000000000,1.0],["from-before","uid-old",null,1760000000000,1.0]]}}]}`)
	}))
	defer reader.Close()
	contacts, err := ClusterLastContacts(context.Background(), reader.URL, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ClusterContact{At: time.UnixMilli(1760000000000).UTC(), AgentVersion: "old1234"}); contacts["tt-box"] != want {
		t.Errorf("tt-box = %+v, want %+v", contacts["tt-box"], want)
	}
	if got := contacts["from-before"]; got.AgentVersion != "" || got.At.IsZero() {
		t.Errorf("a cluster whose agent says no version = %+v, want its contact with no version", got)
	}
	if !strings.Contains(asked, "INTERVAL '2592000 seconds'") {
		t.Errorf("query = %s, want the last contact looked for over the whole lookback", asked)
	}
}
