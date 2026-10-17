package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every other failure still is one. Swallowing them would turn an unreachable or broken metrics
// store into "this job consumed nothing", which silently refunds work that really was done.
func TestQuerySQLRowsStillFailsOnAnyOtherError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":3000,"error":"Internal error: storage is unavailable"}`))
	}))
	defer server.Close()

	if _, err := querySQLRows(context.Background(), server.URL, "SELECT 1"); err == nil {
		t.Fatal("querySQLRows swallowed a storage failure — that bills a real run as zero hours")
	}
}
