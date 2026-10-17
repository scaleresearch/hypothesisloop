package vast

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

const showPath = "/api/v0/instances/9001/"

func TestGetReadsTheInstanceItselfAsListWouldShowIt(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, showPath, answer(200, `{"instances":`+instanceJSON(9001, "hl-exp1-a1", "running")+`}`))
	p := newTestProvider(t, api, Settings{})

	got, exists, err := p.Get(context.Background(), "9001")
	want := provider.Instance{ID: "9001", Name: "hl-exp1-a1", State: provider.StateRunning, StartedAt: time.Unix(4102444800, 0)}
	if err != nil || !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, %v, %v, want %+v", got, exists, err, want)
	}
	if lists := len(api.sent(http.MethodGet, listPath)); lists != 0 {
		t.Errorf("lists = %d, want none", lists)
	}
}

// The API answers an id it does not have with no instance and 200, as it did on 2026-10-09.
func TestGetSaysAnInstanceIsGoneOnlyWhenTheVendorDoes(t *testing.T) {
	for name, read := range map[string]http.HandlerFunc{
		"no instance":      answer(200, `{"instances": null}`),
		"no such instance": answer(404, gone),
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, showPath, read)
			if _, exists, err := newTestProvider(t, api, Settings{}).Get(context.Background(), "9001"); err != nil || exists {
				t.Fatalf("Get = %v, %v, want gone", exists, err)
			}
		})
	}
	api := newFakeAPI(t)
	api.on(http.MethodGet, showPath, answer(500, `oops`))
	if _, exists, err := newTestProvider(t, api, Settings{}).Get(context.Background(), "9001"); err == nil || exists {
		t.Fatalf("Get = %v, %v, want an error that says neither", exists, err)
	}
}
