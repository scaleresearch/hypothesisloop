package httpapi

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// schemaAlias has the document describe type t as alias does.
type schemaAlias struct{ t, alias reflect.Type }

// schemaAliases are the types the document describes otherwise than their Go shape: each string
// enum as a schema of its own its fields refer to, every error as the one error body, and a job
// whose cpu a request may write as a number.
var schemaAliases = []schemaAlias{
	enum(domain.ExperimentStatuses),
	enum(domain.ExperimentActions),
	enum(domain.PlatformExperimentStatuses),
	enum(domain.PlatformExperimentActions),
	enum(domain.HypothesisStatuses),
	enum(domain.HypothesisActions),
	enum(domain.LinkTypes),
	enum(domain.AnsweredCloseReasons),
	enum(domain.Waiters),
	enum(domain.SignupRoles),
	enum(domain.SignupActions),
	enum(domain.AgentRoles),
	enum(domain.AgentTypeChanges),
	enum(domain.AgentTypeActions),
	enum(domain.OrgRoles),
	enum(domain.OrgActions),
	enum(domain.OrgMemberActions),
	enum(domain.APIKeyActions),
	enum(domain.ClusterActions),
	enum(domain.ActorKinds),
	enum(domain.CapacityTiers),
	enum(domain.TypeKinds),
	{reflect.TypeFor[refusal.Refusal](), reflect.TypeFor[platformapi.ErrorModel]()},
	{reflect.TypeFor[domain.JobSpec](), reflect.TypeFor[jobSpec]()},
	{reflect.TypeFor[domain.JobGroup](), reflect.TypeFor[jobGroup]()},
}

func aliasSchemas(r huma.Registry) {
	for _, a := range schemaAliases {
		r.RegisterTypeAlias(a.t, a.alias)
	}
}

// enumValues is the values of each enum in schemaAliases, by its type.
var enumValues = map[reflect.Type][]any{}

func enum[T ~string](values []T) schemaAlias {
	t := reflect.TypeFor[T]()
	for _, v := range values {
		enumValues[t] = append(enumValues[t], string(v))
	}
	return schemaAlias{t, reflect.TypeFor[enumOf[T]]()}
}

// enumOf names T in the document as a schema of its own, added the first time a field refers to it.
type enumOf[T ~string] string

func (enumOf[T]) Schema(r huma.Registry) *huma.Schema {
	t := reflect.TypeFor[T]()
	if r.Map()[t.Name()] == nil {
		s := &huma.Schema{Type: huma.TypeString, Enum: enumValues[t]}
		s.PrecomputeMessages()
		r.Map()[t.Name()] = s
	}
	return &huma.Schema{Ref: "#/components/schemas/" + t.Name()}
}

// jobSpec and jobGroup are named for the document as the types they describe.
type (
	jobSpec  domain.JobSpec
	jobGroup domain.JobGroup
)

func (jobSpec) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema { return cpuEitherForm(s) }
func (jobGroup) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return cpuEitherForm(s)
}

func cpuEitherForm(s *huma.Schema) *huma.Schema {
	if s.Properties["cpu"] != nil {
		s.Properties["cpu"] = &huma.Schema{
			Description: `CPU for each node: a number of cores (4) or a quantity ("500m"). Always stated. Answered as a string.`,
			OneOf:       []*huma.Schema{{Type: huma.TypeString}, {Type: huma.TypeNumber, Minimum: new(float64)}},
		}
	}
	return s
}
