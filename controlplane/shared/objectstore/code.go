package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrNoCode is returned for a commit an agent has stored no code under.
var ErrNoCode = errors.New("objectstore: no code is stored for that commit")

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CommitShaped reports whether sha is a full commit id as git prints it.
func CommitShaped(sha string) bool { return commitSHA.MatchString(sha) }

// codeKey is where the code an agent pushed at one commit is kept: under the agent's own prefix,
// so the per-agent byte ceiling counts it and it is deleted with the experiment's data. The
// agent's data key can write there too. That gives it nothing: whoever fetches the commit from
// the bundle is given objects git names by their content.
func codeKey(platformExperimentID, agentID, sha string) (string, error) {
	if err := domain.ValidateIdentifier("platform_experiment_id", platformExperimentID); err != nil {
		return "", err
	}
	if err := domain.ValidateIdentifier("agent_id", agentID); err != nil {
		return "", err
	}
	if !CommitShaped(sha) {
		return "", fmt.Errorf("objectstore: %q is not a commit id", sha)
	}
	return AgentPrefix(platformExperimentID, agentID) + "code/" + sha + ".bundle", nil
}

// PutCode stores the git bundle that holds commit sha of an agent's work.
func (c *Client) PutCode(ctx context.Context, platformExperimentID, agentID, sha string, bundle []byte) error {
	key, err := codeKey(platformExperimentID, agentID, sha)
	if err != nil {
		return err
	}
	return c.put(ctx, key, bundle)
}

// GetCode returns the stored bundle, or ErrNoCode.
func (c *Client) GetCode(ctx context.Context, platformExperimentID, agentID, sha string, maxBytes int64) ([]byte, error) {
	key, err := codeKey(platformExperimentID, agentID, sha)
	if err != nil {
		return nil, err
	}
	body, found, err := c.get(ctx, key, maxBytes)
	if err == nil && !found {
		return nil, ErrNoCode
	}
	return body, err
}

func (c *Client) put(ctx context.Context, key string, body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("objectstore: put %s: nothing to store", key)
	}
	req, err := c.signedRequest(ctx, http.MethodPut, "/"+c.Bucket+"/"+key, nil, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("objectstore: put %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

// get reads one object. One larger than maxBytes is an error: nothing this package stored is.
func (c *Client) get(ctx context.Context, key string, maxBytes int64) ([]byte, bool, error) {
	req, err := c.signedRequest(ctx, http.MethodGet, "/"+c.Bucket+"/"+key, nil, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("objectstore: get %s: read body: %w", key, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("objectstore: get %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 4<<10)])))
	case int64(len(body)) > maxBytes:
		return nil, false, fmt.Errorf("objectstore: get %s: larger than %d bytes", key, maxBytes)
	}
	return body, true, nil
}
