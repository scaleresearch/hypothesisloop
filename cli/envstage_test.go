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
	"testing"

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
	root := filepath.Join(dir, "root")
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
	s, err := readStaged(dir)
	if err != nil || s.Image != image || s.Job != "job-b" || !slices.Contains(s.Env, "CUDA_HOME=/usr/local/cuda") {
		t.Errorf("staged %+v, %v, want the image of job-b with its environment", s, err)
	}
	if !strings.Contains(stdout.String(), `"image":"`+image+`"`) {
		t.Errorf("stdout %q, want what it staged", stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"env", "stage", "job-b"}, &stdout, &stderr); code != 0 || strings.Count(stderr.String(), "staged in") != 1 {
		t.Errorf("exit %d, stderr %q, want the staged image kept as it is", code, stderr.String())
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

// The command runs as root of the image, with the image's variables and the caller's home, in the
// current directory, which must be one the image shares with the machine.
func TestEnvRunSharesHomeAndTmpAndKeepsTheImagesVariables(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "workspace")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	s := &staged{Root: "/stage/root", Env: []string{"PATH=/opt/conda/bin:/usr/bin"}}
	args, err := sandboxArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{"--uid 0", "--bind /stage/root /", "--bind /tmp /tmp", "--bind " + home + " " + home, "--setenv PATH /opt/conda/bin:/usr/bin", "--setenv HOME " + home, "--clearenv"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q, want %q", line, want)
		}
	}
	if wd, _ := os.Getwd(); !strings.Contains(line, "--chdir "+wd) {
		t.Errorf("args %q, want the current directory", line)
	}

	t.Chdir("/")
	if _, err := sandboxArgs(s); err == nil {
		t.Errorf("a directory outside home and /tmp was accepted")
	}
}
