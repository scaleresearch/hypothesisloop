package objectstore

import (
	"context"
	"errors"

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
	return c.put(ctx, key, snapshot)
}

// GetSession returns the stored session, or ErrNoSession.
func (c *Client) GetSession(ctx context.Context, platformExperimentID, agentID string, maxBytes int64) ([]byte, error) {
	key, err := sessionKey(platformExperimentID, agentID)
	if err != nil {
		return nil, err
	}
	body, found, err := c.get(ctx, key, maxBytes)
	if err == nil && !found {
		return nil, ErrNoSession
	}
	return body, err
}
