package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func agentsSession(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents session", `usage: hl platform-experiments agents session [--platform-experiment PE] AGENT|coordinator|planner > session.tar.gz

Writes the model session last stored to standard output, byte for byte: what its owner said and
saw, as its harness packed it. AGENT is an agent's id, coordinator the sandboxed coordinator and
planner the agent that planned the draft. The platform's own harness packs a gzipped tar, with
the model's transcript as JSON lines under model-home/:

  hl platform-experiments agents session --platform-experiment pe-123 ag-1 | tar -xzf - -C ag-1-session

Read by the agent itself and by the org that owns the platform experiment, while it runs and
after it has closed. One that stored none yet is not found.`, stderr)
	named := command.ExperimentFlag(fs)
	who, err := command.ParseOne(fs, args, "AGENT, coordinator or planner", "", "agent")
	if err != nil {
		return command.ParseExit(err)
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	path := apiclient.AgentsPath(pe) + "/" + url.PathEscape(who) + "/session"
	switch who {
	case "coordinator":
		path = "/platform-experiments/" + url.PathEscape(pe) + "/coordinator/session"
	case "planner":
		roles, err := rolesOf(c, pe)
		if err != nil {
			return command.Fail(stderr, err)
		}
		planner := ""
		for id, role := range roles {
			if role == platformapi.SignupRolePlanner {
				planner = id
			}
		}
		if planner == "" {
			return command.Fail(stderr, fmt.Errorf("platform experiment %s has no planner: only one that began as a draft has", pe))
		}
		path = apiclient.AgentsPath(pe) + "/" + url.PathEscape(planner) + "/session"
	}
	if err := copyBytes(c, path, stdout); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

// copyBytes streams an answer that is no JSON to out.
func copyBytes(c *apiclient.Client, path string, out io.Writer) error {
	if c.BaseURL == "" {
		return errors.New("no API URL given (set API_URL)")
	}
	if c.Token == "" {
		return errors.New("no token given (set HL_TOKEN)")
	}
	req, err := http.NewRequest("GET", strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		refusal, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &apiclient.HTTPError{Method: "GET", Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(refusal))}
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("GET %s: reading response: %w", path, err)
	}
	return nil
}
