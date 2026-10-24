package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// registerAgentRoles registers the list of agent roles on the shared API document.
func registerAgentRoles(doc *Doc) {
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-agent-roles", Method: "GET", Path: "/agent-roles", Tags: []string{"agent-types"},
		Summary: "List the roles an agent plays",
		Description: "Every role of an agent type, with what its agents do, the built-in type an agent of it runs as when it names none, the signup role it is added with, whether a platform experiment's agents list names it and whether an org makes types of it. " +
			"A team is the agents list of a platform experiment: an entry's agent type gives its role, researcher for the built-in researcher and executor for any type of role executor.",
	}, func(context.Context, *struct{}) (*struct{ Body []domain.AgentRoleInfo }, error) {
		return &struct{ Body []domain.AgentRoleInfo }{Body: domain.AgentRoleInfos}, nil
	})
}
