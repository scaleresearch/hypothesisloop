package launcherapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Instance is one machine a launcher keeps: what it boots and the environment its agent starts
// with. Name is its identity, and the launcher puts its own prefix in front of it.
type Instance struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
}

const (
	// A name is <platform experiment>-<agent>-<digest>. The first two parts are there to be read
	// by a person and are cut to these lengths; the digest alone tells two instances apart.
	namePlatformExperimentLength = 20
	nameAgentLength              = 24
	nameDigestLength             = 12
)

// MaxInstanceNameLength is the longest name instanceName returns.
const MaxInstanceNameLength = namePlatformExperimentLength + 1 + nameAgentLength + 1 + nameDigestLength

// instanceFor is the machine a hosted agent runs in. Everything in it follows from the agent's
// signup, the platform's config and what the launcher says it starts machines with, so every
// reconcile answers with the same one until one of them changes. This is the one place the
// agent's environment is put together: a value an agent needs at startup is added here.
func instanceFor(agent domain.AgentSignup, image, apiURL, signingSecret, launchDigest string) (Instance, error) {
	if agent.Flavor == "" || agent.Model == "" {
		return Instance{}, fmt.Errorf("signup of %s has no flavor or no model", agent.AgentID)
	}
	hyperparameters := []byte("{}")
	var err error
	if agent.Hyperparameters != nil {
		hyperparameters, err = json.Marshal(agent.Hyperparameters)
	}
	if err != nil {
		return Instance{}, fmt.Errorf("encode hyperparameters of %s: %w", agent.AgentID, err)
	}
	env := map[string]string{
		"API_URL":                apiURL,
		"AGENT_ID":               agent.AgentID,
		"PLATFORM_EXPERIMENT_ID": agent.PlatformExperimentID,
		"AGENT_FLAVOR":           agent.Flavor,
		"AGENT_MODEL":            agent.Model,
		"AGENT_HYPERPARAMETERS":  string(hyperparameters),
	}
	restarts := 0
	if agent.Instance != nil {
		restarts = agent.Instance.Restarts
	}
	name, err := instanceName(agent.PlatformExperimentID, agent.AgentID, image, env, launchDigest, restarts)
	if err != nil {
		return Instance{}, err
	}
	token, err := auth.Mint(signingSecret, auth.AgentScope(agent.PlatformExperimentID, agent.AgentID))
	if err != nil {
		return Instance{}, err
	}
	env["HL_TOKEN"] = token
	return Instance{Name: name, Image: image, Env: env}, nil
}

// instanceName names one machine of one agent. The digest covers everything the machine is
// started with: the image, the environment, the launcher's own launch inputs (its digest of
// them) and how many machines of this agent exited before it. An agent for whom any of that
// changes is asked for under a new name: the launcher sees a machine that is no longer wanted
// and one that is missing, and replaces it without being told to. env must not hold the agent's
// token, which has no place in a name. The result is lowercase letters, digits and dashes, at
// most MaxInstanceNameLength long.
func instanceName(platformExperimentID, agentID, image string, env map[string]string, launchDigest string, restarts int) (string, error) {
	identity, err := json.Marshal(struct {
		PlatformExperimentID string            `json:"platform_experiment_id"`
		AgentID              string            `json:"agent_id"`
		Image                string            `json:"image"`
		Env                  map[string]string `json:"env"`
		LaunchDigest         string            `json:"launch_digest"`
		Restarts             int               `json:"restarts"`
	}{platformExperimentID, agentID, image, env, launchDigest, restarts})
	if err != nil {
		return "", fmt.Errorf("name instance of %s: %w", agentID, err)
	}
	digest := sha256.Sum256(identity)
	return strings.Join([]string{
		readable(platformExperimentID, namePlatformExperimentLength),
		readable(agentID, nameAgentLength),
		hex.EncodeToString(digest[:])[:nameDigestLength],
	}, "-"), nil
}

// readable keeps the lowercase letters and digits of id, up to length of them. An id with none
// reads as "x" so that the name keeps its three parts.
func readable(id string, length int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if b.Len() == length {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
