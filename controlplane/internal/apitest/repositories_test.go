package apitest

import (
	"context"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// worldRepositories is a git host that has every repository asked of it under one root.
type worldRepositories struct{}

const worldGitRoot = "https://git.test/"

func (worldRepositories) Reachable(_ context.Context, _, repository string) (string, error) {
	return repository, nil
}

func (worldRepositories) URL(repository string) string { return worldGitRoot + repository }

func (worldRepositories) File(context.Context, string, string, string) ([]byte, error) {
	panic("the handler read a file from the git host")
}

func (worldRepositories) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	panic("the handler asked for a git credential")
}

func (worldRepositories) Serves(repository, protocol, host, path string) bool {
	return protocol+"://"+host+"/"+strings.Trim(path, "/") == worldGitRoot+repository
}

func (worldRepositories) CreatesOn(context.Context, string, string) (string, error) {
	return "world", nil
}

func (worldRepositories) Create(_ context.Context, _, _, name, _ string, _ map[string]string) (string, error) {
	return name, nil
}
