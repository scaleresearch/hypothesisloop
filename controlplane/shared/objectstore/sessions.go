package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// sessionsPrefix lies outside every platform experiment's prefix, which each agent's and each
// job's key reads whole: a session holds everything its agent said and saw, and is for the
// platform to hand back to that agent alone.
const sessionsPrefix = "sessions/"

// ErrNoSession is returned for an agent or coordinator that has stored no session.
var ErrNoSession = errors.New("objectstore: no session is stored")

// sessionKey is where the session of an agent, or with no agentID of the coordinator, is kept.
// Both ids become one path segment each, so neither may be anything but an identifier.
func sessionKey(platformExperimentID, agentID string) (string, error) {
	who := "coordinator"
	if agentID != "" {
		if err := domain.ValidateIdentifier("agent_id", agentID); err != nil {
			return "", err
		}
		who = "agents/" + agentID
	}
	if err := domain.ValidateIdentifier("platform_experiment_id", platformExperimentID); err != nil {
		return "", err
	}
	return sessionsPrefix + platformExperimentID + "/" + who + "/session", nil
}

// PutSession replaces the stored session of an agent, or with no agentID of the coordinator.
func (c *Client) PutSession(ctx context.Context, platformExperimentID, agentID string, snapshot []byte) error {
	key, err := sessionKey(platformExperimentID, agentID)
	if err != nil {
		return err
	}
	if len(snapshot) == 0 {
		return fmt.Errorf("objectstore: put %s: the session is empty", key)
	}
	req, err := c.signedRequest(ctx, http.MethodPut, "/"+c.Bucket+"/"+key, nil, snapshot)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("objectstore: put %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// GetSession returns the stored session, or ErrNoSession. One larger than maxBytes is an error:
// nothing this package stored is.
func (c *Client) GetSession(ctx context.Context, platformExperimentID, agentID string, maxBytes int64) ([]byte, error) {
	key, err := sessionKey(platformExperimentID, agentID)
	if err != nil {
		return nil, err
	}
	req, err := c.signedRequest(ctx, http.MethodGet, "/"+c.Bucket+"/"+key, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: read body: %w", key, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNoSession
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("objectstore: get %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 4<<10)])))
	case int64(len(body)) > maxBytes:
		return nil, fmt.Errorf("objectstore: get %s: larger than %d bytes", key, maxBytes)
	}
	return body, nil
}
