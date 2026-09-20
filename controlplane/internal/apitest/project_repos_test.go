package apitest

import "context"

// noProjectRepos is a deployment in which no project has a repository attached.
type noProjectRepos struct{}

func (noProjectRepos) CloneURL(context.Context, string) (string, error) { return "", nil }

func (noProjectRepos) ExperimentBase(context.Context, string) ([]byte, error) { return nil, nil }
