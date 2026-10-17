package hosting

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
)

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
// reconcile answers with the same one until one of them changes.
//
// The machine is handed where the platform is and the agent's token, and nothing else: the token
// says which agent of which platform experiment it is, and the agent reads its agent type, model
// and hyperparameters from its instructions, again whenever they change. None of them goes into
// the machine's name: the agent applies an edit of its type or model on the machine it has.
func instanceFor(agent domain.AgentSignup, ended int, image, apiURL, signingSecret, launchDigest string) (Instance, error) {
	startedAs := map[string]string{
		"API_URL":                apiURL,
		"AGENT_ID":               agent.AgentID,
		"PLATFORM_EXPERIMENT_ID": agent.PlatformExperimentID,
	}
	name, err := instanceName(agent.PlatformExperimentID, agent.AgentID, "", image, startedAs, launchDigest, ended)
	if err != nil {
		return Instance{}, err
	}
	token, err := auth.Mint(signingSecret, auth.AgentScope(agent.PlatformExperimentID, agent.AgentID))
	if err != nil {
		return Instance{}, err
	}
	return Instance{Name: name, Image: image, Env: map[string]string{"API_URL": apiURL, "HL_TOKEN": token}}, nil
}

// coordinatorName stands where an agent's id does in the name of a coordinator's machine.
const coordinatorName = "coordinator"

// coordinatorInstanceFor is the machine a platform experiment's hosted coordinator runs in: the
// agents' image started with the platform experiment's coordinator token, which is what makes it
// a coordinator. It reads its model from its instructions, so a change of it keeps the machine.
func coordinatorInstanceFor(platformExperimentID string, ended int, image, apiURL, signingSecret, launchDigest string) (Instance, error) {
	startedAs := map[string]string{
		"API_URL":                apiURL,
		"PLATFORM_EXPERIMENT_ID": platformExperimentID,
	}
	name, err := instanceName(platformExperimentID, coordinatorName, coordinatorName, image, startedAs, launchDigest, ended)
	if err != nil {
		return Instance{}, err
	}
	token, err := auth.Mint(signingSecret, auth.CoordinatorScope(platformExperimentID))
	if err != nil {
		return Instance{}, err
	}
	return Instance{Name: name, Image: image, Env: map[string]string{"API_URL": apiURL, "HL_TOKEN": token}}, nil
}

// instanceName names one machine of one agent, or of a coordinator when role says so: an agent
// whose id is the coordinator's name gets another digest. The digest covers everything the machine is
// started with: the image, the environment, the launcher's own launch inputs (its digest of
// them) and how many machines of this agent ended before it. An agent for whom any of that
// changes is asked for under a new name: the launcher sees a machine that is no longer wanted
// and one that is missing, and replaces it without being told to. startedAs is what the agent
// runs as, without its token, which has no place in a name. The result is lowercase letters, digits and dashes, at
// most MaxInstanceNameLength long.
func instanceName(platformExperimentID, agentID, role, image string, startedAs map[string]string, launchDigest string, ended int) (string, error) {
	identity, err := json.Marshal(struct {
		PlatformExperimentID string            `json:"platform_experiment_id"`
		AgentID              string            `json:"agent_id"`
		Role                 string            `json:"role,omitempty"`
		Image                string            `json:"image"`
		Env                  map[string]string `json:"env"`
		LaunchDigest         string            `json:"launch_digest"`
		Restarts             int               `json:"restarts"`
	}{platformExperimentID, agentID, role, image, startedAs, launchDigest, ended})
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
