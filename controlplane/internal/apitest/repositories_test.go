package apitest

import "context"

// noRepositories is a deployment in which no org connected GitHub.
type noRepositories struct{}

func (noRepositories) Reachable(context.Context, string, string) (string, error) { return "", nil }

func (noRepositories) ExperimentBase(context.Context, string, string) ([]byte, error) {
	panic("the handler reached GitHub")
}

func (noRepositories) Create(context.Context, string, string, string) (string, error) { return "", nil }
