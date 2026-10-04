package envbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

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
		"a RUN exiting non-zero": {errdefs.WithSource(errdefs.WrapVertex(&gatewaypb.ExitError{ExitCode: 100, Err: errors.New(`process "/bin/sh -c apt-get install -y nonexistent" did not complete successfully: exit code: 100`)}, vertex), line), true},
		"a parse error":          {errdefs.WithSource(errors.New("dockerfile parse error on line 3: unknown instruction: RUNN"), line), true},
		"a FROM not found":       {errdefs.WithSource(errors.New("docker.io/library/nonexistent:1: not found"), line), true},
		"a COPY of a path not there": {errdefs.WithSource(errdefs.WithSolveError(errdefs.WrapVertex(errors.New(`failed to compute cache key: failed to calculate checksum of ref x::y: "/vendor/codec": not found`), vertex),
			&errdefs.Solve_Cache{Cache: &errdefs.ContentCache{Index: 0}}, nil, nil), line), true},
		"a COPY --from of a path not there": {errdefs.WithSource(errdefs.WithSolveError(errdefs.WrapVertex(errors.New(`lstat /var/lib/buildkit/x/opt/missing: no such file or directory`), vertex),
			&errdefs.Solve_File{File: &errdefs.FileAction{Index: 0}}, nil, nil), line), true},
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
	image  []byte
	builds []client.SolveOpt
}

func (d *fakeDaemon) Solve(_ context.Context, def *llb.Definition, opt client.SolveOpt, statuses chan *client.SolveStatus) (*client.SolveResponse, error) {
	defer close(statuses)
	if def != nil {
		return &client.SolveResponse{}, d.pull
	}
	d.builds = append(d.builds, opt)
	statuses <- &client.SolveStatus{Logs: []*client.VertexLog{{Data: []byte(d.output)}}}
	if d.build != nil {
		return nil, d.build
	}
	for _, export := range opt.Exports {
		if export.Output == nil {
			continue
		}
		w, err := export.Output(nil)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(d.image); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	return &client.SolveResponse{ExporterResponse: map[string]string{exptypes.ExporterImageDigestKey: d.digest}}, nil
}

func TestABuildIsReadyWithTheDigestItPushedUntagged(t *testing.T) {
	daemon := &fakeDaemon{digest: "sha256:abc"}
	got := (&Daemon{solver: daemon}).Run(context.Background(), domain.EnvironmentBuild{Base: "registry.test/hl/job:1", Dockerfile: "ARG BASE\nFROM ${BASE}\n", Repository: "registry.test/envs/env"}, nil, ignoreProgress)
	if got.Outcome != domain.EnvironmentBuildPushed || got.Digest != "sha256:abc" {
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
	copyMissing := errdefs.WithSource(errdefs.WithSolveError(errdefs.WrapVertex(errors.New(`failed to compute cache key: "/vendor/codec": not found`), digest.FromString("c")),
		&errdefs.Solve_Cache{Cache: &errdefs.ContentCache{}}, nil, nil), line)
	for name, c := range map[string]struct {
		daemon *fakeDaemon
		fault  domain.EnvironmentBuildOutcomeKind
		log    string
	}{
		"the base does not pull":   {&fakeDaemon{pull: errors.New("registry.test/hl/job:1: 503 Service Unavailable")}, domain.EnvironmentBuildPlatformFault, "503 Service Unavailable"},
		"a RUN of the file fails":  {&fakeDaemon{build: runFailed, output: "installing nonexistent\n"}, domain.EnvironmentBuildFileFault, "installing nonexistent"},
		"a COPY source is missing": {&fakeDaemon{build: copyMissing, output: "[4/6] COPY vendor/codec /opt/codec\n"}, domain.EnvironmentBuildFileFault, `"/vendor/codec": not found`},
		"the push fails":           {&fakeDaemon{build: errors.New("failed to push: failed to authorize")}, domain.EnvironmentBuildPlatformFault, "failed to authorize"},
		"no digest is named":       {&fakeDaemon{}, domain.EnvironmentBuildPlatformFault, "no digest"},
	} {
		got := (&Daemon{solver: c.daemon}).Run(context.Background(), domain.EnvironmentBuild{Base: "registry.test/hl/job:1", Repository: "registry.test/envs/env"}, nil, ignoreProgress)
		if got.Outcome != c.fault || got.Digest != "" || !strings.Contains(got.LogTail, c.log) {
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
	got := (&Daemon{solver: &fakeDaemon{build: killed}}).Run(ctx, domain.EnvironmentBuild{Repository: "registry.test/envs/env"}, nil, ignoreProgress)
	if got.Outcome == domain.EnvironmentBuildFileFault {
		t.Errorf("a cancelled build is %+v, the file's fault", got)
	}
}

func ignoreProgress(string, ...any) {}

// fakeLoader reads read bytes of the image, then answers digest and err.
type fakeLoader struct {
	read       int
	digest     string
	err        error
	repository string
	got        []byte
}

func (l *fakeLoader) Load(_ context.Context, repository string, image io.Reader) (string, error) {
	l.repository = repository
	l.got, _ = io.ReadAll(io.LimitReader(image, int64(l.read)))
	return l.digest, l.err
}

// The image is loaded from the same solve that pushes it, both as OCI media types, and the push
// is the last export so its digest is the one reported.
func TestABuildThatLoadsSendsTheImageToTheLoaderAndPushesItLast(t *testing.T) {
	image := bytes.Repeat([]byte("layer"), 1<<20)
	daemon := &fakeDaemon{digest: "sha256:abc", image: image}
	loader := &fakeLoader{read: len(image), digest: "sha256:abc"}
	var told []string
	d := &Daemon{solver: daemon}
	d.LoadInto(loader)
	got := d.Run(context.Background(), domain.EnvironmentBuild{Base: "registry.test/hl/job:1", Repository: "registry.test/envs/env"}, nil,
		func(format string, args ...any) { told = append(told, fmt.Sprintf(format, args...)) })
	if got.Outcome != domain.EnvironmentBuildPushed || got.Digest != "sha256:abc" {
		t.Fatalf("result %+v, want pushed with the digest", got)
	}
	exports := daemon.builds[0].Exports
	if len(exports) != 2 || exports[0].Type != client.ExporterOCI || exports[1].Type != client.ExporterImage || exports[1].Attrs["push"] != "true" {
		t.Fatalf("exports %+v, want the OCI tar then the push", exports)
	}
	for _, export := range exports {
		if export.Attrs["oci-mediatypes"] != "true" {
			t.Errorf("export %s attrs %v, want OCI media types set", export.Type, export.Attrs)
		}
	}
	if loader.repository != "registry.test/envs/env" || !bytes.Equal(loader.got, image) {
		t.Errorf("the loader got %d bytes for %q, want the image for the repository", len(loader.got), loader.repository)
	}
	if !slices.Contains(told, "loading into the node: done, its jobs start without a pull") {
		t.Errorf("told %q, want the load reported", told)
	}
}

// A loader that fails, stops reading, or holds another image costs the job a pull, never the build.
func TestALoadThatGoesWrongNeverFailsTheBuild(t *testing.T) {
	for name, loader := range map[string]*fakeLoader{
		"the loader fails before reading": {err: errors.New("dial unix /run/containerd/containerd.sock: connect: permission denied")},
		"the loader stops halfway":        {read: 1 << 10, err: errors.New("unexpected EOF")},
		"the loaded image is another":     {read: 5 << 20, digest: "sha256:other"},
	} {
		d := &Daemon{solver: &fakeDaemon{digest: "sha256:abc", image: bytes.Repeat([]byte("layer"), 1<<20)}}
		d.LoadInto(loader)
		var told []string
		got := d.Run(context.Background(), domain.EnvironmentBuild{Base: "registry.test/hl/job:1", Repository: "registry.test/envs/env"}, nil,
			func(format string, args ...any) { told = append(told, fmt.Sprintf(format, args...)) })
		if got.Outcome != domain.EnvironmentBuildPushed || got.Digest != "sha256:abc" {
			t.Errorf("%s: %+v, want pushed with the digest", name, got)
		}
		if last := told[len(told)-1]; !strings.HasSuffix(last, "its jobs pull it") {
			t.Errorf("%s: told %q last, want the pull it costs", name, last)
		}
	}
}

func TestABuildThatFailsBeforeItsExportLoadsNothing(t *testing.T) {
	loader := &fakeLoader{}
	d := &Daemon{solver: &fakeDaemon{build: errors.New("failed to push: failed to authorize")}}
	d.LoadInto(loader)
	got := d.Run(context.Background(), domain.EnvironmentBuild{Base: "registry.test/hl/job:1", Repository: "registry.test/envs/env"}, nil, ignoreProgress)
	if got.Outcome != domain.EnvironmentBuildPlatformFault || loader.repository != "" {
		t.Errorf("%+v, loader asked for %q, want the platform's fault and nothing loaded", got, loader.repository)
	}
}

// A build is followed in the agent's log step by step: each step once as it starts and once as it
// ends, and its transfers no more often than every transferInterval however fast they stream.
func TestABuildTellsEachStepOnceAsItStartsAndEndsAndItsTransfersSparingly(t *testing.T) {
	var told []string
	steps := newSteps(func(format string, args ...any) { told = append(told, fmt.Sprintf(format, args...)) })
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	later := start.Add(90 * time.Second)
	run, push, cached, failing := digest.FromString("run"), digest.FromString("push"), digest.FromString("cached"), digest.FromString("failing")
	for _, status := range []*client.SolveStatus{
		{Vertexes: []*client.Vertex{{Digest: cached, Name: "[1/3] FROM base", Started: &start, Completed: &start, Cached: true}}},
		{Vertexes: []*client.Vertex{{Digest: run, Name: "[2/3] RUN make", Started: &start}}},
		{Vertexes: []*client.Vertex{{Digest: run, Name: "[2/3] RUN make", Started: &start}}},
		{Vertexes: []*client.Vertex{{Digest: run, Name: "[2/3] RUN make", Started: &start, Completed: &later}}},
		{Vertexes: []*client.Vertex{{Digest: push, Name: "pushing layers", Started: &later}}},
		{Statuses: []*client.VertexStatus{{Vertex: push, ID: "sha256:aaa", Current: 100e6, Total: 1200e6}}},
		{Statuses: []*client.VertexStatus{{Vertex: push, ID: "sha256:aaa", Current: 200e6, Total: 1200e6}}},
		{Vertexes: []*client.Vertex{{Digest: push, Name: "pushing layers", Started: &later, Completed: &later}}},
		{Vertexes: []*client.Vertex{{Digest: failing, Name: "[3/3] RUN false", Started: &later, Completed: &later, Error: "exit code: 1"}}},
	} {
		steps.follow(status)
	}
	want := []string{
		"[1/3] FROM base: cached",
		"[2/3] RUN make: started",
		"[2/3] RUN make: done in 1m30s",
		"pushing layers: started",
		"pushing layers: sha256:aaa 100/1200 MB",
		"pushing layers: done in 0s",
		"[3/3] RUN false: failed",
	}
	if !slices.Equal(told, want) {
		t.Errorf("told\n%s\nwant\n%s", strings.Join(told, "\n"), strings.Join(want, "\n"))
	}
}
