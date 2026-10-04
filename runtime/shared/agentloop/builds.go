package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// reconcileBuilds starts every listed build not running at its attempt and cancels every running
// one no longer listed at it. An agent that restarts starts its builds again, and the daemon's
// cache makes that cheap.
func (a *Agent) reconcileBuilds(ctx context.Context, listed []domain.EnvironmentBuild) {
	if a.Builder == nil {
		return
	}
	a.buildsMu.Lock()
	defer a.buildsMu.Unlock()
	if a.builds == nil {
		a.builds = map[string]*runningBuild{}
	}
	attempts := make(map[string]int, len(listed))
	for _, b := range listed {
		attempts[b.Key] = b.Attempt
	}
	for key, running := range a.builds {
		if attempt, ok := attempts[key]; !ok || attempt != running.attempt {
			running.cancel()
			delete(a.builds, key)
		}
	}
	for _, b := range listed {
		if _, running := a.builds[b.Key]; running {
			continue
		}
		buildCtx, cancel := context.WithCancel(ctx)
		running := &runningBuild{attempt: b.Attempt, cancel: cancel}
		a.builds[b.Key] = running
		a.building.Add(1)
		go a.runBuild(buildCtx, running, b)
	}
}

// runBuild runs b and reports how it ended, unless it was cancelled. A report the control plane
// did not take forgets the build, so the next pass that still lists it runs it again. A reported
// build is cancelled too once the list drops it, which stops nothing, so the cancel is told here.
func (a *Agent) runBuild(ctx context.Context, running *runningBuild, b domain.EnvironmentBuild) {
	defer a.building.Done()
	defer running.cancel()
	a.Log("build %s attempt %d: started on %s", b.Key, b.Attempt, b.Base)
	report := a.Builder.Run(ctx, b, func(ctx context.Context) (string, string, error) {
		var login domain.RegistryLogin
		err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/credential", domain.EnvironmentBuildCredentialRequest{Attempt: b.Attempt}, &login)
		return login.Username, login.Password, err
	}, func(format string, args ...any) {
		a.Log("build %s attempt %d: %s", b.Key, b.Attempt, fmt.Sprintf(format, args...))
	})
	if ctx.Err() != nil {
		a.Log("build %s attempt %d: no longer this cluster's, cancelled", b.Key, b.Attempt)
		return
	}
	report.Attempt = b.Attempt
	err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key), report, nil)
	if err == nil {
		a.Log("build %s attempt %d: %s %s", b.Key, b.Attempt, report.Outcome, report.Digest)
		return
	}
	a.Log("build %s attempt %d: report %s: %v", b.Key, b.Attempt, report.Outcome, err)
	a.buildsMu.Lock()
	defer a.buildsMu.Unlock()
	if a.builds[b.Key] == running {
		delete(a.builds, b.Key)
	}
}

// postJSON posts body to path under this cluster's API and decodes a 2xx answer into out, when
// out is not nil.
func (a *Agent) postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/internal/clusters/%s%s", a.APIURL, a.ClusterName, path), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return refusal(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
