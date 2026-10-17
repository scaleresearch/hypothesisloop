package domain

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// enumSchema names the string type T in the OpenAPI document as a schema of its own taking one of
// values, and refers to it, so each field of the type reads as the type in every client.
func enumSchema[T ~string](r huma.Registry, values []T) *huma.Schema {
	name := reflect.TypeFor[T]().Name()
	if r.Map()[name] == nil {
		s := &huma.Schema{Type: huma.TypeString}
		for _, v := range values {
			s.Enum = append(s.Enum, string(v))
		}
		s.PrecomputeMessages()
		r.Map()[name] = s
	}
	return &huma.Schema{Ref: "#/components/schemas/" + name}
}

func (ExperimentStatus) Schema(r huma.Registry) *huma.Schema {
	return enumSchema(r, ExperimentStatuses)
}
func (PlatformExperimentStatus) Schema(r huma.Registry) *huma.Schema {
	return enumSchema(r, PlatformExperimentStatuses)
}
func (HypothesisStatus) Schema(r huma.Registry) *huma.Schema {
	return enumSchema(r, HypothesisStatuses)
}
func (LinkType) Schema(r huma.Registry) *huma.Schema        { return enumSchema(r, LinkTypes) }
func (CloseReason) Schema(r huma.Registry) *huma.Schema     { return enumSchema(r, AnsweredCloseReasons) }
func (Waiter) Schema(r huma.Registry) *huma.Schema          { return enumSchema(r, Waiters) }
func (SignupRole) Schema(r huma.Registry) *huma.Schema      { return enumSchema(r, SignupRoles) }
func (AgentRole) Schema(r huma.Registry) *huma.Schema       { return enumSchema(r, AgentRoles) }
func (AgentTypeAction) Schema(r huma.Registry) *huma.Schema { return enumSchema(r, AgentTypeActions) }
func (OrgRole) Schema(r huma.Registry) *huma.Schema         { return enumSchema(r, OrgRoles) }
func (ActorKind) Schema(r huma.Registry) *huma.Schema       { return enumSchema(r, ActorKinds) }
func (CapacityTier) Schema(r huma.Registry) *huma.Schema    { return enumSchema(r, CapacityTiers) }
func (TypeKind) Schema(r huma.Registry) *huma.Schema        { return enumSchema(r, TypeKinds) }
