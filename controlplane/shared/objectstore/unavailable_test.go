package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAStoreThatDoesNotAnswerIsUnavailable(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(530)
	}))
	defer gateway.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	for name, endpoint := range map[string]string{"gateway error": gateway.URL, "unreachable": closed.URL} {
		client, err := New(endpoint, endpoint, "garage", "bucket", "key-id", "secret")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.TotalBytes(context.Background(), "pe-1/ag-1/")
		var down *Unavailable
		if !errors.As(err, &down) {
			t.Fatalf("%s: got %v, want Unavailable", name, err)
		}
		if reason, _ := down.Unavailable(); reason != "object_store_unavailable" {
			t.Fatalf("%s: reason %q", name, reason)
		}
	}
}

func TestARefusalOfTheStoreIsNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	client, err := New(srv.URL, srv.URL, "garage", "bucket", "key-id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TotalBytes(context.Background(), "pe-1/ag-1/")
	var down *Unavailable
	if err == nil || errors.As(err, &down) {
		t.Fatalf("got %v, want an error that is not Unavailable", err)
	}
}
