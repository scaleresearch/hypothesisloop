package quota

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// SessionStore keeps one opaque snapshot per agent and per coordinator. *objectstore.Client is
// the one in use.
type SessionStore interface {
	PutSession(ctx context.Context, platformExperimentID, agentID string, snapshot []byte) error
	GetSession(ctx context.Context, platformExperimentID, agentID string, maxBytes int64) ([]byte, error)
}

type storedSession struct {
	SizeBytes int `json:"size_bytes"`
}

type sessionBytes struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

const sessionDescription = "A session is what the harness needs to go on where it stopped: the model's conversation so far, packed by the harness and opaque to the platform. " +
	"The harness stores it after a turn and reads it before its first turn on a new machine, so a machine that was replaced costs the work of one turn at most. " +
	"Only its owner's own token reads or writes it. At most 32 MiB (413 above). Writing answers 409 once the platform experiment has been closed for longer than the wind-down."

// RegisterSessionsHuma registers where a hosted agent and a hosted coordinator keep their model
// session, so that it outlives the machine they run on.
func RegisterSessionsHuma(doc *apidocs.Doc, sessions SessionStore, logger *zap.Logger) {
	access := doc.Access
	put := func(ctx context.Context, platformExperimentID, agentID string, snapshot []byte) (*struct{ Body storedSession }, error) {
		if err := access.OwnSession(ctx, platformExperimentID, agentID, true); err != nil {
			return nil, err
		}
		if len(snapshot) == 0 {
			return nil, huma.Error400BadRequest("the body is the session and must not be empty")
		}
		if err := sessions.PutSession(ctx, platformExperimentID, agentID, snapshot); err != nil {
			logger.Error("store session failed", zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID), zap.Error(err))
			return nil, huma.Error500InternalServerError(auth.InternalError)
		}
		return &struct{ Body storedSession }{Body: storedSession{SizeBytes: len(snapshot)}}, nil
	}
	get := func(ctx context.Context, platformExperimentID, agentID string) (*sessionBytes, error) {
		if err := access.OwnSession(ctx, platformExperimentID, agentID, false); err != nil {
			return nil, err
		}
		snapshot, err := sessions.GetSession(ctx, platformExperimentID, agentID, domain.MaxAgentSessionBytes)
		if errors.Is(err, objectstore.ErrNoSession) {
			return nil, huma.Error404NotFound("session not found")
		}
		if err != nil {
			logger.Error("read session failed", zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID), zap.Error(err))
			return nil, huma.Error500InternalServerError(auth.InternalError)
		}
		return &sessionBytes{ContentType: "application/octet-stream", Body: snapshot}, nil
	}

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-agent-session", Method: "PUT", Path: "/platform-experiments/{id}/agents/{agent_id}/session",
		Summary: "Store an agent's model session", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxAgentSessionBytes, Description: sessionDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
		RawBody []byte `contentType:"application/octet-stream"`
	}) (*struct{ Body storedSession }, error) {
		return put(ctx, in.ID, in.AgentID, in.RawBody)
	})
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-agent-session", Method: "GET", Path: "/platform-experiments/{id}/agents/{agent_id}/session",
		Summary: "Read an agent's stored model session", Tags: []string{"platform-experiments"}, Description: sessionDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*sessionBytes, error) {
		return get(ctx, in.ID, in.AgentID)
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "put-coordinator-session", Method: "PUT", Path: "/platform-experiments/{id}/coordinator/session",
		Summary: "Store the hosted coordinator's model session", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxAgentSessionBytes, Description: sessionDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		RawBody []byte `contentType:"application/octet-stream"`
	}) (*struct{ Body storedSession }, error) {
		return put(ctx, in.ID, "", in.RawBody)
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-coordinator-session", Method: "GET", Path: "/platform-experiments/{id}/coordinator/session",
		Summary: "Read the hosted coordinator's stored model session", Tags: []string{"platform-experiments"}, Description: sessionDescription,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*sessionBytes, error) {
		return get(ctx, in.ID, "")
	})
}
