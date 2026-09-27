package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// CodeStore keeps the code an agent pushed, one git bundle per commit. *objectstore.Client is
// the one in use.
type CodeStore interface {
	PutCode(ctx context.Context, platformExperimentID, agentID, sha string, bundle []byte) error
	GetCode(ctx context.Context, platformExperimentID, agentID, sha string, maxBytes int64) ([]byte, error)
}

// CodeRoster says who may still push code. *PlatformExperimentsService is the one in use.
type CodeRoster interface {
	Joined(ctx context.Context, platformExpID, agentID string) (bool, error)
	// Repository returns the repository the platform experiment names, empty when it names none.
	Repository(ctx context.Context, platformExpID string) (string, error)
	RequireActive(ctx context.Context, platformExpID, agentID string) error
}

// Repository returns the repository the platform experiment names, empty when it names none.
func (s *PlatformExperimentsService) Repository(ctx context.Context, platformExpID string) (string, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.Repository: %w", err)
	}
	if pe == nil {
		return "", ErrPlatformExperimentNotFound
	}
	return pe.Repository, nil
}

// Joined reports whether the agent is in the platform experiment.
func (s *PlatformExperimentsService) Joined(ctx context.Context, platformExpID, agentID string) (bool, error) {
	return s.store.IsSignedUp(ctx, platformExpID, agentID)
}

type storedCode struct {
	SHA       string `json:"sha"`
	SizeBytes int    `json:"size_bytes"`
}

const codeDescription = "A platform experiment with no repository, which only a deployment with no GitHub App has, keeps its agents' code here: one git bundle per commit, made by `git bundle create` and pushed by `hl code push`. " +
	"Writing answers 409 code_in_repository for a platform experiment that names a repository: its code is pushed there with git. " +
	"A job's code_ref is this address with @sha in place of /sha, and `hl-clone` fetches it from the API it was given with the job's own token, so the code a job ran stays readable as long as the experiment's data. " +
	"An agent writes only its own; everyone the platform experiment admits reads all of it, its jobs included. At most 32 MiB (413 above). " +
	"Writing answers 409 once the platform experiment is closed or the agent was cut, and 422 not_a_bundle for a body that is no bundle holding that commit."

// bundleHolds reports whether body is a git bundle whose header lists sha as a tip. The header
// is "# v2 git bundle" (or v3 with @capability lines), then one "<sha> <ref>" line per tip and
// "-<sha>" per prerequisite, then an empty line and the pack.
func bundleHolds(body []byte, sha string) bool {
	header, _, found := bytes.Cut(body, []byte("\n\n"))
	if !found {
		return false
	}
	lines := bytes.Split(header, []byte("\n"))
	if signature := string(lines[0]); signature != "# v2 git bundle" && signature != "# v3 git bundle" {
		return false
	}
	for _, line := range lines[1:] {
		if bytes.HasPrefix(line, []byte(sha+" ")) {
			return true
		}
	}
	return false
}

// RegisterCodeHuma registers where the agents of a platform experiment without a repository
// keep the code their jobs run.
func RegisterCodeHuma(doc *apidocs.Doc, code CodeStore, roster CodeRoster, logger *zap.Logger) {
	access := doc.Access
	type address struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
		SHA     string `path:"sha" doc:"The full 40-digit commit id."`
	}
	// The same refusal for a commit id that could name nothing as for one nothing is stored under.
	named := func(in address) error {
		if !domain.IdentifierShaped(in.AgentID) || !objectstore.CommitShaped(in.SHA) {
			return huma.Error404NotFound("code not found")
		}
		return nil
	}

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-agent-code", Method: "PUT", Path: "/platform-experiments/{id}/agents/{agent_id}/code/{sha}",
		Summary: "Store an agent's code at one commit", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxAgentCodeBytes, Description: codeDescription,
	}, func(ctx context.Context, put *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
		SHA     string `path:"sha" doc:"The full 40-digit commit id."`
		RawBody []byte `contentType:"application/octet-stream"`
	}) (*struct{ Body storedCode }, error) {
		in := struct {
			address
			RawBody []byte
		}{address{put.ID, put.AgentID, put.SHA}, put.RawBody}
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := access.ActAs(ctx, in.AgentID); err != nil {
			return nil, err
		}
		if err := named(in.address); err != nil {
			return nil, err
		}
		joined, err := roster.Joined(ctx, in.ID, in.AgentID)
		if err != nil {
			return nil, auth.Internal(logger, "store code failed", err, zap.String("platformExpID", in.ID), zap.String("agentID", in.AgentID))
		}
		if !joined {
			return nil, huma.Error404NotFound("agent " + in.AgentID + " is not in platform experiment " + in.ID)
		}
		if err := roster.RequireActive(ctx, in.ID, in.AgentID); err != nil {
			return nil, err
		}
		repository, err := roster.Repository(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(logger, "store code failed", err, zap.String("platformExpID", in.ID), zap.String("agentID", in.AgentID))
		}
		if repository != "" {
			return nil, api.Refuse(http.StatusConflict, "code_in_repository", "platform experiment "+in.ID+" keeps its code in "+domain.RepositoryURL(repository)+": commit on your branch and `git push` it there. The platform stores code only for a platform experiment that names no repository")
		}
		if !bundleHolds(in.RawBody, in.SHA) {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "not_a_bundle", "the body is not a git bundle whose tip is "+in.SHA+": make it with `git bundle create <file> HEAD`, or push with `hl code push`")
		}
		if err := code.PutCode(ctx, in.ID, in.AgentID, in.SHA, in.RawBody); err != nil {
			return nil, auth.Internal(logger, "store code failed", err, zap.String("platformExpID", in.ID), zap.String("agentID", in.AgentID))
		}
		return &struct{ Body storedCode }{Body: storedCode{SHA: in.SHA, SizeBytes: len(in.RawBody)}}, nil
	})

	read := func(ctx context.Context, in *address) (*sessionBytes, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := named(*in); err != nil {
			return nil, err
		}
		bundle, err := code.GetCode(ctx, in.ID, in.AgentID, in.SHA, domain.MaxAgentCodeBytes)
		if errors.Is(err, objectstore.ErrNoCode) {
			return nil, huma.Error404NotFound("code not found")
		}
		if err != nil {
			return nil, auth.Internal(logger, "read code failed", err, zap.String("platformExpID", in.ID), zap.String("agentID", in.AgentID))
		}
		return &sessionBytes{ContentType: "application/octet-stream", Body: bundle}, nil
	}
	apidocs.Register(doc, apidocs.AudienceJob, huma.Operation{
		OperationID: "get-agent-code", Method: "GET", Path: "/platform-experiments/{id}/agents/{agent_id}/code/{sha}",
		Summary: "Read an agent's code at one commit", Tags: []string{"platform-experiments"}, Description: codeDescription,
	}, read)
}
