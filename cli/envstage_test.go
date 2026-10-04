package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type entry struct {
	name, body, link string
	kind             byte
	mode             int64
}

func layerOf(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// pushedImage is an image of two layers in a registry of its own, by digest: the upper one
// replaces a file, removes another and links a third.
func pushedImage(t *testing.T) string {
	t.Helper()
	lower := layerOf(t,
		entry{name: "usr/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "usr/bin/", kind: tar.TypeDir, mode: 0o555},
		entry{name: "usr/bin/tool", kind: tar.TypeReg, mode: 0o755, body: "old"},
		entry{name: "usr/bin/gone", kind: tar.TypeReg, mode: 0o644, body: "removed above"},
		entry{name: "dev/null", kind: tar.TypeChar, mode: 0o666},
	)
	upper := layerOf(t,
		entry{name: "usr/bin/tool", kind: tar.TypeReg, mode: 0o755, body: "new"},
		entry{name: "usr/bin/.wh.gone", kind: tar.TypeReg, mode: 0o644},
		entry{name: "usr/bin/same", kind: tar.TypeLink, link: "usr/bin/tool"},
		entry{name: "lib", kind: tar.TypeSymlink, link: "/usr/lib"},
	)
	img, err := mutate.AppendLayers(empty.Image, lower, upper)
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.Config(img, v1.Config{Env: []string{"PATH=/opt/tools/bin:/usr/bin", "CUDA_HOME=/usr/local/cuda"}}); err != nil {
		t.Fatal(err)
	}
	return pushed(t, img)
}

// pushed is img in a registry of its own, by digest.
func pushed(t *testing.T, img v1.Image) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/environment:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return ref.Context().Digest(digest.String()).String()
}

// With no job named, the baseline job's image is staged: its layers flattened into one root, a
// removed file absent, devices left out, and its environment kept for hl env run. Staging it again
// pulls nothing.
func TestEnvStageExtractsTheBaselineJobsImage(t *testing.T) {
	image := pushedImage(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	var asked []string
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/platform-experiments/pe-1":
			_ = json.NewEncoder(w).Encode(map[string]string{"baseline_job_id": "job-b"})
		case "/experiments/job-b/image-credentials":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"image": image, "environment_key": "k1"})
		default:
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "stage", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{"GET /platform-experiments/pe-1", "POST /experiments/job-b/image-credentials"}
	if !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	dir, err := stageDir()
	if err != nil {
		t.Fatal(err)
	}
	s, err := readStaged(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := s.Root
	if body, err := os.ReadFile(filepath.Join(root, "usr/bin/same")); err != nil || string(body) != "new" {
		t.Errorf("usr/bin/same holds %q, %v, want the upper tool through its link", body, err)
	}
	if info, err := os.Stat(filepath.Join(root, "usr/bin/tool")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("usr/bin/tool: %v, %v, want mode 0755", info, err)
	}
	if link, err := os.Readlink(filepath.Join(root, "lib")); err != nil || link != "/usr/lib" {
		t.Errorf("lib links to %q, %v, want /usr/lib", link, err)
	}
	for _, absent := range []string{"usr/bin/gone", "usr/bin/.wh.gone", "dev/null"} {
		if _, err := os.Lstat(filepath.Join(root, absent)); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want it absent", absent, err)
		}
	}
	if s.Image != image || s.Job != "job-b" || !slices.Contains(s.Env, "CUDA_HOME=/usr/local/cuda") {
		t.Errorf("staged %+v, want the image of job-b with its environment", s)
	}
	if !strings.Contains(stdout.String(), `"image":"`+image+`"`) {
		t.Errorf("stdout %q, want what it staged", stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"env", "stage", "job-b"}, &stdout, &stderr); code != 0 || strings.Count(stderr.String(), "staged in") != 1 {
		t.Errorf("exit %d, stderr %q, want the staged image kept as it is", code, stderr.String())
	}
}

// A session staged while its environment was still being pushed, was refused once and failed
// silently: staging waits for the environment as long as it is being built or pushed.
func TestEnvStageWaitsForTheEnvironmentToBeReady(t *testing.T) {
	image := pushedImage(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	defer func(wait time.Duration) { stageWait = wait }(stageWait)
	stageWait = time.Millisecond
	asks := 0
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if asks++; asks < 3 {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": "environment_not_ready", "message": "job job-b runs in environment k1, which is loaded: pushed"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"image": image, "environment_key": "k1"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "stage", "job-b"}, &stdout, &stderr); code != 0 || asks != 3 || strings.Count(stderr.String(), "waiting until the environment is ready") != 1 {
		t.Errorf("exit %d after %d asks, stderr %q, want it staged once the environment is ready, said once", code, asks, stderr.String())
	}
}

// A failed environment fails the stage at once, and hl env run says that is why nothing is staged.
func TestEnvStageFailsAtOnceForAFailedEnvironmentAndEnvRunSaysSo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	asks := 0
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		asks++
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "environment_failed", "message": "the file failed: pip: no such package"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "stage", "job-b"}, &stdout, &stderr); code == 0 || asks != 1 || !strings.Contains(stderr.String(), "no such package") {
		t.Errorf("exit %d after %d asks, stderr %q, want it failed at once with the build's output", code, asks, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"env", "run", "--", "make"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "the last hl env stage failed") || !strings.Contains(stderr.String(), "no such package") {
		t.Errorf("exit %d, stderr %q, want it told why nothing is staged", code, stderr.String())
	}
}

func TestEnvStageWithoutABaselineSaysToNameAJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "pe-1"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "stage", "--platform-experiment", "pe-1"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "no baseline job yet") {
		t.Errorf("exit %d, stderr %q, want it told there is no baseline job", code, stderr.String())
	}
}

func TestEnvRunWithNothingStagedSaysHowToStage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HL_TOKEN", "test-token")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "run", "--", "make"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "hl env stage") {
		t.Errorf("exit %d, stderr %q, want it told to stage", code, stderr.String())
	}
}

// The command runs as root of the image, seen through its working tree's view, with the image's
// variables and the caller's home, in the current directory, which must be one the image shares
// with the machine.
func TestEnvRunSharesHomeAndTmpAndKeepsTheImagesVariables(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "workspace")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	s := &staged{Root: "/stage/root", Env: []string{"PATH=/opt/conda/bin:/usr/bin"}}
	wd, err := sharedWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	args, err := sandboxArgs(s, "/stage/view", wd)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{"--uid 0", "--bind /stage/view /", "--bind /tmp /tmp", "--bind " + home + " " + home, "--setenv PATH /opt/conda/bin:/usr/bin", "--setenv HOME " + home, "--clearenv"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q, want %q", line, want)
		}
	}
	if !strings.Contains(line, "--chdir "+wd) {
		t.Errorf("args %q, want the current directory", line)
	}

	t.Chdir("/")
	if _, err := sharedWorkingDirectory(); err == nil {
		t.Errorf("a directory outside home and /tmp was accepted")
	}
}

// Parallel work in two worktrees built into the same path of the image and one measured the
// other's binary: each working tree has a view of the image of its own, the same from anywhere
// in it, and a new root has new views.
func TestEnvRunGivesEachWorkingTreeAViewOfItsOwn(t *testing.T) {
	home := t.TempDir()
	repo, linked, loose := filepath.Join(home, "repo"), filepath.Join(home, "wt", "idea"), filepath.Join(home, "scratch")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "src", "deep"), linked, loose} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+repo+"/.git/worktrees/idea\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for wd, want := range map[string]string{repo: repo, filepath.Join(repo, "src", "deep"): repo, linked: linked, loose: loose} {
		if got := worktreeOf(wd); got != want {
			t.Errorf("worktreeOf(%s) = %s, want %s", wd, got, want)
		}
	}
	dir := "/cache/hl/env"
	views := map[string]bool{}
	for _, root := range []string{"/cache/hl/env/roots/a", "/cache/hl/env/roots/b"} {
		for _, tree := range []string{repo, linked, loose} {
			views[overlayOf(dir, root, tree)] = true
		}
	}
	if len(views) != 6 {
		t.Errorf("views %v, want one for each root and working tree", views)
	}
}

// Layers merge as the job's overlay engine merges them: an opaque directory hides what the layers
// below put in it but keeps what its own layer wrote there, whatever their order, a whiteout
// removes a file of a layer below, and a path under a symlink of a layer below makes a directory
// that hides the symlink.
func TestEnvStageAppliesLayersAsTheJobsEngineDoes(t *testing.T) {
	lower := layerOf(t,
		entry{name: "opt/a/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "opt/a/old", kind: tar.TypeReg, mode: 0o644, body: "lower"},
		entry{name: "opt/a/sub/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "opt/a/sub/deep", kind: tar.TypeReg, mode: 0o644, body: "lower"},
		entry{name: "usr/lib/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "usr/lib/lower.so", kind: tar.TypeReg, mode: 0o644, body: "lower"},
		entry{name: "usr/bin/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "usr/bin/python3", kind: tar.TypeReg, mode: 0o755, body: "py"},
		entry{name: "lib", kind: tar.TypeSymlink, link: "/usr/lib"},
		entry{name: "data/x", kind: tar.TypeReg, mode: 0o644, body: "lower"},
	)
	middle := layerOf(t,
		entry{name: "opt/a/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "opt/a/new", kind: tar.TypeReg, mode: 0o644, body: "middle"},
		entry{name: "opt/a/sub/", kind: tar.TypeDir, mode: 0o755},
		entry{name: "opt/a/sub/fresh", kind: tar.TypeReg, mode: 0o644, body: "middle"},
		entry{name: "opt/a/.wh..wh..opq", kind: tar.TypeReg},
		entry{name: "lib/upper.so", kind: tar.TypeReg, mode: 0o644, body: "beside the link"},
		entry{name: "usr/bin/python3.12", kind: tar.TypeReg, mode: 0o755, body: "py"},
		entry{name: "usr/bin/python", kind: tar.TypeLink, link: "/usr/bin/python3.12"},
		entry{name: "data/.wh.x", kind: tar.TypeReg},
	)
	upper := layerOf(t, entry{name: "opt/a/.wh.new", kind: tar.TypeReg})
	img, err := mutate.AppendLayers(empty.Image, lower, middle, upper)
	if err != nil {
		t.Fatal(err)
	}
	s, err := stage(t.Context(), t.TempDir(), pushed(t, img), authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"opt/a/sub/fresh": "middle", "lib/upper.so": "beside the link", "usr/lib/lower.so": "lower", "usr/bin/python": "py"} {
		if body, err := os.ReadFile(filepath.Join(s.Root, file)); err != nil || string(body) != want {
			t.Errorf("%s holds %q, %v, want %q", file, body, err, want)
		}
	}
	for _, absent := range []string{"opt/a/old", "opt/a/sub/deep", "opt/a/new", "data/x", "usr/lib/upper.so"} {
		if _, err := os.Lstat(filepath.Join(s.Root, absent)); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want it absent", absent, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(s.Root, "lib")); err != nil || !info.IsDir() {
		t.Errorf("lib: %v, %v, want a directory in place of the link", info, err)
	}
}

// A new stage removes the roots before it, except one a command still runs in, which goes with
// the stage after that command ended.
func TestEnvStageKeepsARootACommandRunsIn(t *testing.T) {
	image := pushedImage(t)
	dir := t.TempDir()
	stageOnce := func() *staged {
		t.Helper()
		s, err := stage(t.Context(), dir, image, authn.Anonymous)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeStaged(dir, s); err != nil {
			t.Fatal(err)
		}
		if err := removeUnused(filepath.Dir(s.Root), s.Root); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := stageOnce()
	held, fd, err := holdStaged(dir)
	if err != nil || held.Root != first.Root {
		t.Fatalf("held %+v, %v, want the first root", held, err)
	}
	second := stageOnce()
	if _, err := os.Stat(filepath.Join(first.Root, "usr/bin/tool")); err != nil {
		t.Errorf("the held root was removed: %v", err)
	}
	syscall.Close(fd)
	third := stageOnce()
	entries, err := os.ReadDir(filepath.Dir(third.Root))
	if err != nil || len(entries) != 1 || filepath.Join(filepath.Dir(third.Root), entries[0].Name()) != third.Root {
		t.Errorf("roots %v, %v, want only %s once %s and %s are unused", entries, err, third.Root, first.Root, second.Root)
	}
}
