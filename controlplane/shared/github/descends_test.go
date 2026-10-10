package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub compares base...head from their merge base: with the commit as the base and the
// ancestor as the head, a descendant is behind it by nothing the answer lists.
func TestDescendsComparesTheAncestorAgainstTheCommit(t *testing.T) {
	const ancestor, child, other = "aaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		basehead := strings.TrimPrefix(r.URL.Path, "/repos/Acme/Model/compare/")
		base, head, _ := strings.Cut(basehead, "...")
		switch {
		case head != ancestor:
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(child, base):
			json.NewEncoder(w).Encode(map[string]any{"status": "behind", "base_commit": map[string]string{"sha": child}, "commits": []any{}, "files": []any{}}) //nolint:errcheck
		case base == ancestor:
			json.NewEncoder(w).Encode(map[string]any{"status": "identical", "base_commit": map[string]string{"sha": ancestor + strings.Repeat("0", 36)}}) //nolint:errcheck
		case base == other:
			json.NewEncoder(w).Encode(map[string]any{"status": "diverged", "base_commit": map[string]string{"sha": other}}) //nolint:errcheck
		case base == "huge":
			w.Write([]byte(`{"status":"diverged","files":"` + strings.Repeat("x", maxResponseBytes) + `"}`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := &Client{apiURL: server.URL, http: server.Client()}
	token := &Token{Value: "ghs_read", Repository: "Acme/Model"}
	ctx := context.Background()

	if sha, err := client.Descends(ctx, token, ancestor, child[:7]); err != nil || sha != child {
		t.Errorf("a descendant by its abbreviation = %q, %v, want its full sha", sha, err)
	}
	if !strings.HasSuffix(asked[0], "?per_page=1") {
		t.Errorf("asked %s, want one commit a page", asked[0])
	}
	if sha, err := client.Descends(ctx, token, ancestor, ancestor); err != nil || sha != ancestor+strings.Repeat("0", 36) {
		t.Errorf("the ancestor itself = %q, %v", sha, err)
	}
	for commit, want := range map[string]error{other: ErrNotDescendant, "huge": ErrNotDescendant, "dddddddd": ErrCommitNotFound} {
		if sha, err := client.Descends(ctx, token, ancestor, commit); !errors.Is(err, want) {
			t.Errorf("%s = %q, %v, want %v", commit, sha, err, want)
		}
	}
}
