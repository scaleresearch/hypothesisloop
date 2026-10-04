package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

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
	steps := make(chan string, 1)
	posted := make(chan struct{})
	go a.postProgress(ctx, b, steps, posted)
	report := a.Builder.Run(ctx, b, func(ctx context.Context) (string, string, error) {
		var login domain.RegistryLogin
		err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/credential", domain.EnvironmentBuildCredentialRequest{Attempt: b.Attempt}, &login)
		return login.Username, login.Password, err
	}, func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		a.Log("build %s attempt %d: %s", b.Key, b.Attempt, line)
		step := progressStep(line)
		if step == "" {
			return
		}
		// The build tells its steps from one goroutine, so after the drain the send never blocks.
		select {
		case <-steps:
		default:
		}
		steps <- step
	})
	close(steps)
	<-posted
	if ctx.Err() != nil {
		a.Log("build %s attempt %d: no longer this cluster's, cancelled", b.Key, b.Attempt)
		return
	}
	report.Attempt = b.Attempt
	err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key), report, nil)
	if err == nil {
		detail := report.Digest
		if report.Outcome != domain.EnvironmentBuildPushed {
			lines := strings.Split(strings.TrimSpace(report.LogTail), "\n")
			detail = lines[len(lines)-1]
		}
		a.Log("build %s attempt %d: %s %s", b.Key, b.Attempt, report.Outcome, detail)
		return
	}
	a.Log("build %s attempt %d: report %s: %v", b.Key, b.Attempt, report.Outcome, err)
	a.buildsMu.Lock()
	defer a.buildsMu.Unlock()
	if a.builds[b.Key] == running {
		delete(a.builds, b.Key)
	}
}

// postProgress posts each step it receives as the step b is at, until steps closes, then closes
// posted. A step told while one is posted replaces the one waiting, so a slow control plane holds
// no build back, and a post that fails is logged and costs the build nothing.
func (a *Agent) postProgress(ctx context.Context, b domain.EnvironmentBuild, steps <-chan string, posted chan<- struct{}) {
	defer close(posted)
	for step := range steps {
		err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/progress", domain.EnvironmentBuildProgress{Attempt: b.Attempt, Step: step}, nil)
		if err != nil && ctx.Err() == nil {
			a.Log("build %s attempt %d: progress: %v", b.Key, b.Attempt, err)
		}
	}
}

// progressStep is the first line of line, cut to domain.MaxEnvironmentProgressBytes on a rune
// boundary.
func progressStep(line string) string {
	line, _, _ = strings.Cut(strings.ReplaceAll(line, "\r", "\n"), "\n")
	line = strings.TrimSpace(line)
	if len(line) <= domain.MaxEnvironmentProgressBytes {
		return line
	}
	cut := domain.MaxEnvironmentProgressBytes
	for !utf8.RuneStart(line[cut]) {
		cut--
	}
	return strings.TrimSpace(line[:cut])
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
