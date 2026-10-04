package envbuild

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/opencontainers/go-digest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestTheDaemonGetsTheCredentialOfTheRegistryAloneAndFetchesItsTokenItself(t *testing.T) {
	asked := 0
	a := &registryAuth{host: "registry.test", credential: func(context.Context) (string, string, error) {
		asked++
		return "oauth2accesstoken", "token", nil
	}}
	ctx := context.Background()
	got, err := a.Credentials(ctx, &auth.CredentialsRequest{Host: "registry.test"})
	if err != nil || got.Username != "oauth2accesstoken" || got.Secret != "token" {
		t.Fatalf("the registry's credential: %v %v", got, err)
	}
	other, err := a.Credentials(ctx, &auth.CredentialsRequest{Host: "docker.io"})
	if err != nil || other.Username != "" || other.Secret != "" || asked != 1 {
		t.Fatalf("another host is handed %v %v, the credential asked %d times", other, err, asked)
	}
	// The daemon reads Unimplemented as no token authority and asks for the credential instead.
	if _, err := a.GetTokenAuthority(ctx, &auth.GetTokenAuthorityRequest{Host: "registry.test"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetTokenAuthority: %v", err)
	}
}

func TestTheDockerfileInputIsADefinitionTheDaemonAccepts(t *testing.T) {
	def, err := dockerfileState("ARG BASE\nFROM ${BASE}\n").Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llb.NewDefinitionOp(def.ToPB()); err != nil {
		t.Errorf("the input: %v", err)
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

func TestOnlyTheEndOfALongBuildOutputIsKept(t *testing.T) {
	log := strings.Repeat("compiling a crate\n", 2000) + "error[E0425]: cannot find value\n"
	tail := lastBytes(log, domain.MaxEnvironmentLogBytes)
	if len(tail) > domain.MaxEnvironmentLogBytes || !strings.HasSuffix(tail, "error[E0425]: cannot find value\n") || !strings.HasPrefix(tail, "compiling") {
		t.Errorf("tail of %d bytes, want at most %d starting on a line and ending with the error", len(tail), domain.MaxEnvironmentLogBytes)
	}
}

// fakeDaemon answers the base pull with pull and the build with build, after streaming output.
type fakeDaemon struct {
	pull   error
	build  error
	output string
	digest string
	builds []client.SolveOpt
}

func (d *fakeDaemon) Solve(_ context.Context, def *llb.Definition, opt client.SolveOpt, statuses chan *client.SolveStatus) (*client.SolveResponse, error) {
	if statuses != nil {
		defer close(statuses)
	}
	if def != nil {
		return &client.SolveResponse{}, d.pull
	}
	d.builds = append(d.builds, opt)
	statuses <- &client.SolveStatus{Logs: []*client.VertexLog{{Data: []byte(d.output)}}}
	if d.build != nil {
		return nil, d.build
	}
	return &client.SolveResponse{ExporterResponse: map[string]string{exptypes.ExporterImageDigestKey: d.digest}}, nil
}

func TestABuildIsReadyWithTheDigestItPushedUntagged(t *testing.T) {
	daemon := &fakeDaemon{digest: "sha256:abc"}
	got := (&Daemon{solver: daemon}).Run(context.Background(), Build{Base: "registry.test/hl/job:1", Dockerfile: "ARG BASE\nFROM ${BASE}\n", Repository: "registry.test/envs/env"}, nil)
	if got.Fault != "" || got.Digest != "sha256:abc" {
		t.Fatalf("result %+v, want ready with the digest", got)
	}
	attrs := daemon.builds[0].Exports[0].Attrs
	if attrs["name"] != "registry.test/envs/env" || attrs["push"] != "true" || attrs["push-by-digest"] != "true" {
		t.Errorf("export %v, want the repository pushed by digest with no tag", attrs)
	}
	if daemon.builds[0].FrontendAttrs["build-arg:BASE"] != "registry.test/hl/job:1" {
		t.Errorf("frontend %v, want BASE the base", daemon.builds[0].FrontendAttrs)
	}
}

func TestWhoseFaultABuildThatPushedNothingIs(t *testing.T) {
	line := &errdefs.Source{Info: &pb.SourceInfo{Filename: "Dockerfile", Data: []byte("ARG BASE\nFROM ${BASE}\nRUN false\n")},
		Ranges: []*pb.Range{{Start: &pb.Position{Line: 3}, End: &pb.Position{Line: 3}}}}
	runFailed := errdefs.WithSource(errdefs.WrapVertex(&gatewaypb.ExitError{ExitCode: 1, Err: errors.New("exit code: 1")}, digest.FromString("v")), line)
	for name, c := range map[string]struct {
		daemon *fakeDaemon
		fault  domain.EnvironmentFault
		log    string
	}{
		"the base does not pull":  {&fakeDaemon{pull: errors.New("registry.test/hl/job:1: 503 Service Unavailable")}, domain.EnvironmentFaultPlatform, "503 Service Unavailable"},
		"a RUN of the file fails": {&fakeDaemon{build: runFailed, output: "installing nonexistent\n"}, domain.EnvironmentFaultFile, "installing nonexistent"},
		"the push fails":          {&fakeDaemon{build: errors.New("failed to push: failed to authorize")}, domain.EnvironmentFaultPlatform, "failed to authorize"},
		"no digest is named":      {&fakeDaemon{}, domain.EnvironmentFaultPlatform, "no digest"},
	} {
		got := (&Daemon{solver: c.daemon}).Run(context.Background(), Build{Base: "registry.test/hl/job:1", Repository: "registry.test/envs/env"}, nil)
		if got.Fault != c.fault || got.Digest != "" || !strings.Contains(got.LogTail, c.log) {
			t.Errorf("%s: %+v, want fault %q with %q in its log", name, got, c.fault, c.log)
		}
		if c.daemon.pull != nil && len(c.daemon.builds) != 0 {
			t.Errorf("%s: built on a base that did not pull", name)
		}
	}
}

func TestARunACancelStoppedIsNotTheFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	killed := &gatewaypb.ExitError{ExitCode: 137, Err: errors.New("exit code: 137")}
	got := (&Daemon{solver: &fakeDaemon{build: killed}}).Run(ctx, Build{Repository: "registry.test/envs/env"}, nil)
	if got.Fault == domain.EnvironmentFaultFile {
		t.Errorf("a cancelled build is %+v, the file's fault", got)
	}
}
