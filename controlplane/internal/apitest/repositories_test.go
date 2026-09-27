package apitest

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// noRepositories is a deployment in which no org connected GitHub.
type noRepositories struct{}

func (noRepositories) Reachable(context.Context, string, string) (string, error) { return "", nil }

func (noRepositories) URL(string) string { return "" }

func (noRepositories) File(context.Context, string, string, string) ([]byte, error) {
	panic("the handler reached GitHub")
}

func (noRepositories) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	panic("the handler reached GitHub")
}

func (noRepositories) Serves(string, string, string, string) bool { return false }

func (noRepositories) Create(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
