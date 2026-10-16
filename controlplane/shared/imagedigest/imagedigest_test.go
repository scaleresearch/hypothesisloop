package imagedigest

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const sum = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func asking(answers map[string]string) (*Resolver, *[]string) {
	var asked []string
	r := New("10.0.0.2:5000")
	r.resolve = func(_ context.Context, ref string, plain bool) (string, error) {
		key := ref
		if plain {
			key = "http " + ref
		}
		asked = append(asked, key)
		if digest, ok := answers[key]; ok {
			return digest, nil
		}
		return "", errors.New("no answer")
	}
	return r, &asked
}

// A tag is pinned to the digest its registry answers, asked once while it is fresh, and named by
// repository and digest alone: the same image under another tag is the same pin.
func TestATagIsPinnedToTheDigestItsRegistryAnswers(t *testing.T) {
	r, asked := asking(map[string]string{"registry.test/job-base:abc": sum, "registry.test/job-base:def": sum})
	for range 2 {
		got, err := r.Pinned(context.Background(), "registry.test/job-base:abc")
		if err != nil || got != "registry.test/job-base@"+sum {
			t.Fatalf("pinned %q, %v", got, err)
		}
	}
	other, err := r.Pinned(context.Background(), "registry.test/job-base:def")
	if err != nil || other != "registry.test/job-base@"+sum {
		t.Fatalf("another tag: %q, %v", other, err)
	}
	if !slices.Equal(*asked, []string{"registry.test/job-base:abc", "registry.test/job-base:def"}) {
		t.Errorf("asked %v, want each tag once", *asked)
	}
}

func TestADigestIsKeptAndNothingIsAsked(t *testing.T) {
	r, asked := asking(nil)
	got, err := r.Pinned(context.Background(), "registry.test/env:7@"+sum)
	if err != nil || got != "registry.test/env@"+sum || len(*asked) != 0 {
		t.Errorf("pinned %q, %v after asking %v, want the digest kept with no question", got, err, *asked)
	}
}

// A registry on a port is a local one that may serve plain HTTP; any other is asked over TLS alone.
func TestOnlyARegistryOnAPortIsAskedOverPlainHTTP(t *testing.T) {
	r, asked := asking(map[string]string{"http 10.0.0.2:5000/hypothesisloop-job-base:abc": sum})
	if got, err := r.Pinned(context.Background(), "10.0.0.2:5000/hypothesisloop-job-base:abc"); err != nil || got != "10.0.0.2:5000/hypothesisloop-job-base@"+sum {
		t.Errorf("local registry: %q, %v", got, err)
	}
	if _, err := r.Pinned(context.Background(), "registry.test/job:1"); err == nil {
		t.Error("an unanswered registry pinned an image")
	}
	if !slices.Equal(*asked, []string{"10.0.0.2:5000/hypothesisloop-job-base:abc", "http 10.0.0.2:5000/hypothesisloop-job-base:abc", "registry.test/job:1"}) {
		t.Errorf("asked %v", *asked)
	}
}
