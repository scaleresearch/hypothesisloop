package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARequestCarriesTheTokenAndItsBodyAndAnAnswerIsDecoded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/internal/clusters/edge-7/reconcile" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok" ||
			r.Header.Get("Content-Type") != "application/json" || string(body) != `{"asked":1}` {
			t.Errorf("request %s %s %q %q %s", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body)
		}
		_, _ = io.WriteString(w, `{"answered":2}`)
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL + "/internal/clusters/edge-7", Token: "tok", HTTP: server.Client()}
	var answer struct {
		Answered int `json:"answered"`
	}
	if err := c.Do(context.Background(), http.MethodPost, "/reconcile", map[string]int{"asked": 1}, &answer); err != nil || answer.Answered != 2 {
		t.Fatalf("answer = %+v, %v", answer, err)
	}
}

func TestARefusalIsTheControlPlanesOwnWords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("a request without a body says it is %s", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "unauthenticated: cluster edge-7 is not registered", "reason": "cluster_not_registered"})
	}))
	defer server.Close()
	err := (&Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client()}).Do(context.Background(), http.MethodDelete, "", nil, nil)
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized || refused.Reason != "cluster_not_registered" ||
		refused.Message != "unauthenticated: cluster edge-7 is not registered" || !strings.Contains(err.Error(), "control plane answered 401: {") {
		t.Fatalf("refusal = %+v, %v", refused, err)
	}
}

func TestAnAnswerLargerThanTheBoundIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"machines":[`+strings.Repeat(" ", 64)+`]}`)
	}))
	defer server.Close()
	var answer any
	err := (&Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client(), MaxAnswerBytes: 32}).Do(context.Background(), http.MethodGet, "", nil, &answer)
	if err == nil || !strings.Contains(err.Error(), "larger than 32 bytes") {
		t.Fatalf("err = %v", err)
	}
}
