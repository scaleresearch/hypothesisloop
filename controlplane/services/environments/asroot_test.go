package environments

import (
	"context"
	"fmt"
	"testing"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// baseImages answers the configuration of each base by its user.
type baseImages map[string]string

func (b baseImages) ResolveImageConfig(_ context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	config := fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{"User":%q},"rootfs":{"type":"layers"}}`, b[ref])
	return ref, digest.FromString(config), []byte(config), nil
}

// convert builds the plan of dockerfile on base the way the builder asks for it, and returns the
// user each RUN runs as and the user of the image.
func convert(t *testing.T, dockerfile, base string, bases baseImages) (runs []string, user string) {
	t.Helper()
	platform := platforms.MustParse(buildPlatform)
	st, img, _, _, err := dockerfile2llb.Dockerfile2LLB(context.Background(), []byte(asRoot(dockerfile)), dockerfile2llb.ConvertOpt{
		Config:         dockerui.Config{BuildArgs: map[string]string{"BASE": base, baseUserArg: bases[base]}},
		TargetPlatform: &platform,
		MetaResolver:   bases,
	})
	if err != nil {
		t.Fatalf("convert on %s: %v", base, err)
	}
	def, err := st.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, dt := range def.Def {
		var op pb.Op
		if err := op.UnmarshalVT(dt); err != nil {
			t.Fatal(err)
		}
		if exec := op.GetExec(); exec != nil {
			runs = append(runs, exec.Meta.User)
		}
	}
	return runs, img.Config.User
}

func TestOneFileInstallsAsRootOnBothBasesAndKeepsEachBaseUser(t *testing.T) {
	bases := baseImages{"registry.test/job:1": "", "registry.test/agent:1": "agent"}
	file := "# the toolchain\nARG BASE\nFROM ${BASE}\nRUN apt-get update && apt-get install -y build-essential\nUSER nobody\nRUN true"
	for base, want := range bases {
		if err := domain.ValidateEnvironmentFile(file); err != nil {
			t.Fatal(err)
		}
		runs, user := convert(t, file, base, bases)
		if len(runs) != 2 || runs[0] != "root" || runs[1] != "nobody" || user != want {
			t.Errorf("on %s: runs as %q and the image runs as %q, want the install as root and the image as %q", base, runs, user, want)
		}
	}
}

func TestTheKeyCoversWhatIsBuilt(t *testing.T) {
	svc := NewService(&memoryStore{envs: map[string]*domain.Environment{}}, &fileHost{}, "registry.test/env")
	env, err := svc.Ensure(context.Background(), rust, "base:1")
	if err != nil || env.Key != domain.EnvironmentKey(asRoot(rust), "base:1") || env.Dockerfile != rust {
		t.Errorf("%+v, %v, want the repository's file kept under the key of what is built from it", env, err)
	}
}
