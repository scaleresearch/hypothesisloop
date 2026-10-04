package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// reconcileBuilds starts every listed build not running at its attempt and cancels every running
// one no longer listed at it. A listed build that ended with its report not taken is not run
// again: its report is posted again, so a file that failed, whose next run fails the same way,
// runs once however long the control plane is out of reach. An agent that restarts starts its
// builds again, and the daemon's cache makes that cheap.
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
		if running, ok := a.builds[b.Key]; ok {
			if running.unsent != nil && !running.posting {
				running.posting = true
				a.building.Add(1)
				go func() {
					defer a.building.Done()
					a.postReport(ctx, running, b)
				}()
			}
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
// did not take is kept, and the next pass that still lists b posts it again (postReport). A
// reported build is cancelled too once the list drops it, which stops nothing, so the cancel is
// told here.
func (a *Agent) runBuild(ctx context.Context, running *runningBuild, b domain.EnvironmentBuild) {
	defer a.building.Done()
	defer running.cancel()
	a.Log("build %s attempt %d: started on %s", b.Key, b.Attempt, b.Base)
	steps := make(chan string, 1)
	posted := make(chan struct{})
	go a.postProgress(ctx, b, steps, posted)
	// The loaded report goes out while the push goes on, and before the outcome, which a control
	// plane that took it no longer fences on the build.
	var loadedPosted sync.WaitGroup
	loaded := func(node, digest string) {
		loadedPosted.Add(1)
		go func() {
			defer loadedPosted.Done()
			err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/loaded", domain.EnvironmentBuildLoaded{Attempt: b.Attempt, Node: node, Digest: digest}, nil)
			if err != nil && ctx.Err() == nil {
				a.Log("build %s attempt %d: loaded on %s: %v", b.Key, b.Attempt, node, err)
			}
		}()
	}
	report := a.Builder.Run(ctx, b, func(ctx context.Context) (string, string, error) {
		var login domain.RegistryLogin
		err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/credential", domain.EnvironmentBuildCredentialRequest{Attempt: b.Attempt}, &login)
		return login.Username, login.Password, err
	}, func(ctx context.Context) (domain.EnvironmentGitCredential, error) {
		var credential domain.EnvironmentGitCredential
		err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key)+"/git-credential", domain.EnvironmentBuildCredentialRequest{Attempt: b.Attempt}, &credential)
		return credential, err
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
	}, loaded)
	close(steps)
	<-posted
	loadedPosted.Wait()
	if ctx.Err() != nil {
		a.Log("build %s attempt %d: no longer this cluster's, cancelled", b.Key, b.Attempt)
		return
	}
	report.Attempt = b.Attempt
	a.buildsMu.Lock()
	running.unsent, running.posting = &report, true
	a.buildsMu.Unlock()
	a.postReport(ctx, running, b)
}

// postReport posts the unsent report of b. Taken, it is sent; not taken, it waits for the next
// pass that still lists b at its attempt, which posts it again.
func (a *Agent) postReport(ctx context.Context, running *runningBuild, b domain.EnvironmentBuild) {
	a.buildsMu.Lock()
	report := *running.unsent
	a.buildsMu.Unlock()
	err := a.postJSON(ctx, "/builds/"+url.PathEscape(b.Key), report, nil)
	a.buildsMu.Lock()
	running.posting = false
	if err == nil {
		running.unsent = nil
	}
	a.buildsMu.Unlock()
	if err == nil {
		detail := report.Digest
		if report.Outcome != domain.EnvironmentBuildPushed {
			lines := strings.Split(strings.TrimSpace(report.LogTail), "\n")
			detail = lines[len(lines)-1]
		}
		a.Log("build %s attempt %d: %s %s", b.Key, b.Attempt, report.Outcome, detail)
		return
	}
	a.Log("build %s attempt %d: report %s: %v; posted again while it is listed", b.Key, b.Attempt, report.Outcome, err)
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
