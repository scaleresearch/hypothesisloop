package environments

import (
	"context"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTheDaemonGetsTheCredentialOfTheRegistryAloneAndFetchesItsTokenItself(t *testing.T) {
	a := &registryAuth{host: "registry.test", username: "oauth2accesstoken", password: "token"}
	ctx := context.Background()
	got, err := a.Credentials(ctx, &auth.CredentialsRequest{Host: "registry.test"})
	if err != nil || got.Username != "oauth2accesstoken" || got.Secret != "token" {
		t.Fatalf("the registry's credential: %v %v", got, err)
	}
	other, err := a.Credentials(ctx, &auth.CredentialsRequest{Host: "docker.io"})
	if err != nil || other.Username != "" || other.Secret != "" {
		t.Fatalf("another host is handed %v %v", other, err)
	}
	// The daemon reads Unimplemented as no token authority and asks for the credential instead.
	if _, err := a.GetTokenAuthority(ctx, &auth.GetTokenAuthorityRequest{Host: "registry.test"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetTokenAuthority: %v", err)
	}
}

func TestEveryFrontendInputIsADefinitionTheDaemonAccepts(t *testing.T) {
	inputs, err := frontendInputs(context.Background(), "ARG BASE\nFROM ${BASE}\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dockerfile", "context"} {
		def, ok := inputs[name]
		if !ok {
			t.Fatalf("no %s input", name)
		}
		if _, err := llb.NewDefinitionOp(def); err != nil {
			t.Errorf("the %s input: %v", name, err)
		}
	}
}
