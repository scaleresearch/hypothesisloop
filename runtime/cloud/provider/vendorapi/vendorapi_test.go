package vendorapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func get(t *testing.T, handler http.HandlerFunc, maxBody int64) ([]byte, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/things?secret=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := Do(server.Client(), req, "example", maxBody)
	return raw, err
}

func TestDoReturnsTheBodyOfASuccess(t *testing.T) {
	raw, err := get(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "12345678") }, 8)
	if err != nil || string(raw) != "12345678" {
		t.Fatalf("Do = %q, %v, want a body of exactly the bound", raw, err)
	}
}

// An answer can carry what runs inside an instance: a file it wrote, a line it logged.
func TestDoRefusesABodyLongerThanItsBoundWithoutReadingItAll(t *testing.T) {
	var sent atomic.Int64
	raw, err := get(t, func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 64<<10)
		for sent.Load() < 256<<20 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			sent.Add(int64(len(chunk)))
		}
	}, 1024)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "more than 1024 bytes") || raw != nil {
		t.Fatalf("Do = %d bytes, %v, want a refusal naming the bound", len(raw), err)
	}
	if sent.Load() >= 256<<20 {
		t.Errorf("the whole %d bytes were taken before the answer was refused", sent.Load())
	}
}

func TestDoTurnsARefusalIntoAStatusErrorWithTheStartOfItsBody(t *testing.T) {
	_, err := get(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, strings.Repeat("slow down ", 1000))
	}, 1024)
	var failure *StatusError
	if !errors.As(err, &failure) || failure.Status != 429 || failure.Vendor != "example" || failure.Path != "/v1/things" || len(failure.Body) != maxErrorBody {
		t.Fatalf("err = %#v, want a 429 of example with %d bytes of body", err, maxErrorBody)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error carries the query: %v", err)
	}
}

func TestGiveBackDestroysOnItsOwnTimeAndSaysWhatBecameOfTheInstance(t *testing.T) {
	cause := errors.New("could not be confirmed")
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	err := GiveBack(ended, func(ctx context.Context) error { return ctx.Err() }, time.Second, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "it was destroyed") {
		t.Errorf("err = %v, want the cause and that the instance was destroyed, though the caller's context had ended", err)
	}
	err = GiveBack(ended, func(context.Context) error { return errors.New("HTTP 503") }, time.Second, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "may still be rented") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("err = %v, want the cause, the failed destroy and that the instance may still be rented", err)
	}
}
