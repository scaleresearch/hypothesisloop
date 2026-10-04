package environments

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/moby/buildkit/client/llb"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/opencontainers/go-digest"
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

// Each error is built as the daemon builds it and crosses gRPC as it does, so the test reads what
// the client is handed.
func TestOnlyAnInstructionOfTheFileFailingIsTheFiles(t *testing.T) {
	ctx := context.Background()
	line := &errdefs.Source{Info: &pb.SourceInfo{Filename: "Dockerfile", Data: []byte("ARG BASE\nFROM ${BASE}\nRUN false\n")},
		Ranges: []*pb.Range{{Start: &pb.Position{Line: 3}, End: &pb.Position{Line: 3}}}}
	vertex := digest.FromString("the vertex")
	for name, c := range map[string]struct {
		daemon error
		file   bool
	}{
		"a RUN exiting non-zero":   {errdefs.WithSource(errdefs.WrapVertex(&gatewaypb.ExitError{ExitCode: 100, Err: errors.New(`process "/bin/sh -c apt-get install -y nonexistent" did not complete successfully: exit code: 100`)}, vertex), line), true},
		"a parse error":            {errdefs.WithSource(errors.New("dockerfile parse error on line 3: unknown instruction: RUNN"), line), true},
		"a FROM not found":         {errdefs.WithSource(errors.New("docker.io/library/nonexistent:1: not found"), line), true},
		"a RUN that never started": {errdefs.WithSource(errdefs.WrapVertex(&gatewaypb.ExitError{ExitCode: gatewaypb.UnknownExitStatus, Err: errors.New("failed to create shim task")}, vertex), line), false},
		"a pull of a layer":        {errdefs.WithSource(errdefs.WrapVertex(errors.New("failed to copy: httpReadSeeker: failed open: unexpected EOF"), vertex), line), false},
		"the empty context":        {status.Error(codes.Unknown, "failed to get frontend inputs: invalid nil input definition to definition op"), false},
		"the push":                 {errors.New("failed to push registry.test/env:k: failed to authorize: context deadline exceeded"), false},
		"the daemon unreachable":   {status.Error(codes.Unavailable, "connection error"), false},
	} {
		handed := fmt.Errorf("failed to solve: %w", grpcerrors.FromGRPC(grpcerrors.ToGRPC(ctx, c.daemon)))
		if got := fileFault(handed); got != c.file {
			t.Errorf("%s: the file's fault %v, want %v", name, got, c.file)
		}
	}
}
