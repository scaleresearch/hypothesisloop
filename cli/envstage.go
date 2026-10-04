package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// staged is what hl env stage leaves beside the root it extracted, and hl env run reads.
type staged struct {
	Job            string    `json:"job"`
	Image          string    `json:"image"`
	EnvironmentKey string    `json:"environment_key,omitempty"`
	Env            []string  `json:"env"`
	Root           string    `json:"root"`
	Bytes          int64     `json:"bytes"`
	StagedAt       time.Time `json:"staged_at"`
}

// stageDir holds the one staged image of this machine: root/, staged.json and stage.lock. It is
// under the user's cache, outside every repository.
func stageDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "hl", "env"), nil
}

func envStage(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env stage", `usage: hl env stage [JOB] [--platform-experiment PE]

Pulls the image job JOB runs in, its environment once built, and extracts it on this machine,
where hl env run runs commands in it. JOB defaults to the platform experiment's baseline job, so
with no JOB this is the image every job built on the baseline runs in; the harness stages it in
the background when a session starts. Stage a job of yours after its commit changed the
environment, once the environment is built. The machine holds one staged image: a new one
replaces it once it is whole, and until then hl env run uses the old one. Staging an image
already staged does nothing. Prints what it staged as one JSON line.

Refusals: environment_not_ready (the job's environment is building or failed); run_over.`, stderr)
	pe := experimentFlag(fs)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one JOB, got %d", len(operands)))
	}
	job := ""
	if len(operands) == 1 {
		job = operands[0]
	} else {
		var err error
		if job, err = baselineJob(c, experimentOf(c, *pe)); err != nil {
			return fail(stderr, fmt.Errorf("env stage: %w", err))
		}
	}
	dir, err := stageDir()
	if err != nil {
		return fail(stderr, fmt.Errorf("env stage: %w", err))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(stderr, fmt.Errorf("env stage: %w", err))
	}
	unlock, err := lockStage(dir)
	if err != nil {
		return fail(stderr, fmt.Errorf("env stage: %w", err))
	}
	defer unlock()

	var credential struct {
		Image          string `json:"image"`
		EnvironmentKey string `json:"environment_key"`
		Login          *struct {
			Registry string `json:"registry"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"login"`
	}
	if err := c.Do("POST", "/experiments/"+url.PathEscape(job)+"/image-credentials", nil, &credential); err != nil {
		return fail(stderr, err)
	}
	if current, err := readStaged(dir); err == nil && current.Image == credential.Image {
		return printStaged(stdout, current)
	}
	auth := authn.Anonymous
	if credential.Login != nil {
		auth = &authn.Basic{Username: credential.Login.Username, Password: credential.Login.Password}
	}
	started := time.Now()
	out, err := stage(context.Background(), dir, credential.Image, auth)
	if err != nil {
		return fail(stderr, fmt.Errorf("env stage: %s: %w", credential.Image, err))
	}
	out.Job, out.EnvironmentKey = job, credential.EnvironmentKey
	if err := writeStaged(dir, out); err != nil {
		return fail(stderr, fmt.Errorf("env stage: %w", err))
	}
	fmt.Fprintf(stderr, "hl: env stage: %s staged in %s\n", credential.Image, time.Since(started).Round(time.Second))
	return printStaged(stdout, out)
}

// baselineJob is the job the platform experiment declared its baseline.
func baselineJob(c *apiclient.Client, pe string) (string, error) {
	if pe == "" {
		return "", errors.New("no platform experiment is named, so there is no baseline job: name a JOB or pass --platform-experiment PE")
	}
	var out struct {
		BaselineJobID string `json:"baseline_job_id"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(pe), nil, &out); err != nil {
		return "", err
	}
	if out.BaselineJobID == "" {
		return "", fmt.Errorf("platform experiment %s has no baseline job yet: name a JOB of yours whose environment is built", pe)
	}
	return out.BaselineJobID, nil
}

// lockStage holds the stage of dir for this process, refusing while another holds it.
func lockStage(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "stage.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another hl env stage is staging now: hl env run uses what it stages once it ends")
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func stagingNow(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "stage.lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	return false
}

// stage extracts image into dir/root, in place of what was staged there once the new root is
// whole.
func stage(ctx context.Context, dir, image string, auth authn.Authenticator) (*staged, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(ref, remote.WithAuth(auth), remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return nil, err
	}
	config, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	next := filepath.Join(dir, "root.next")
	if err := os.RemoveAll(next); err != nil {
		return nil, err
	}
	if err := os.Mkdir(next, 0o755); err != nil {
		return nil, err
	}
	flat := mutate.Extract(img)
	bytes, err := extractTar(flat, next)
	flat.Close()
	if err != nil {
		os.RemoveAll(next)
		return nil, err
	}
	root := filepath.Join(dir, "root")
	old := filepath.Join(dir, "root.old")
	if err := os.RemoveAll(old); err != nil {
		return nil, err
	}
	if err := os.Rename(root, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(next, root); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(old); err != nil {
		return nil, err
	}
	return &staged{Image: image, Env: config.Config.Env, Root: root, Bytes: bytes, StagedAt: time.Now().UTC()}, nil
}

// extractTar writes the flattened filesystem r into dir and returns the bytes of its files. Owners
// are not kept: every file is the caller's, which hl env run maps to root. Device files are left
// out, as hl env run gives the command a /dev of its own.
func extractTar(r io.Reader, dir string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var total int64
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "/"))
		if name == "." {
			continue
		}
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return total, err
		}
		mode := h.FileInfo().Mode().Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			// Owner-writable, so that the root can be replaced and removed.
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return total, err
			}
			err = root.Chmod(name, mode|0o700)
		case tar.TypeReg:
			var f *os.File
			if f, err = root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil {
				return total, err
			}
			var n int64
			n, err = io.Copy(f, tr)
			total += n
			if err == nil {
				err = f.Chmod(mode)
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		case tar.TypeSymlink:
			err = root.Symlink(h.Linkname, name)
		case tar.TypeLink:
			err = root.Link(path.Clean(strings.TrimPrefix(h.Linkname, "/")), name)
		}
		if err != nil {
			return total, fmt.Errorf("%s: %w", name, err)
		}
	}
}

func readStaged(dir string) (*staged, error) {
	data, err := os.ReadFile(filepath.Join(dir, "staged.json"))
	if err != nil {
		return nil, err
	}
	var s staged
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeStaged(dir string, s *staged) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	next := filepath.Join(dir, "staged.json.next")
	if err := os.WriteFile(next, data, 0o644); err != nil {
		return err
	}
	return os.Rename(next, filepath.Join(dir, "staged.json"))
}

func printStaged(stdout io.Writer, s *staged) int {
	line, _ := json.Marshal(map[string]any{"job": s.Job, "image": s.Image, "environment_key": s.EnvironmentKey, "root": s.Root, "bytes": s.Bytes, "staged_at": s.StagedAt})
	fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

func envRun(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("env run", `usage: hl env run -- COMMAND [ARG...]

Runs COMMAND on this machine inside the image hl env stage staged, the image a job runs in, as
root of that image, with its environment variables. Your home and /tmp are the machine's own,
so a working tree and what it builds are shared, and the command starts in the current
directory, which must be under one of them. What it installs into the image stays there until
the next stage replaces it. The machine has none of a job's accelerators: code builds and
imports, and what needs the device fails. Exits with COMMAND's status.`, stderr)
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}
	command := flags.Args()
	if len(command) == 0 {
		return parseExit(misuse(flags, "no COMMAND: hl env run -- COMMAND"))
	}
	dir, err := stageDir()
	if err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	s, err := readStaged(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if stagingNow(dir) {
			return fail(stderr, errors.New("env run: the image is being staged now and nothing was staged before it: run this again once hl env stage ends"))
		}
		return fail(stderr, errors.New("env run: nothing is staged on this machine: hl env stage stages the image a job runs in, and says why it cannot"))
	}
	if err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fail(stderr, errors.New("env run: needs bubblewrap (bwrap) on PATH, on Linux"))
	}
	argv, err := sandboxArgs(s)
	if err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	if err := syscall.Exec(bwrap, append(append([]string{"bwrap"}, argv...), command...), nil); err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	return 0
}

// sandboxArgs are bwrap's arguments that run a command in the staged root as its root user, in
// namespaces of its own, with the caller's home, /tmp and network.
func sandboxArgs(s *staged) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if !within(wd, home) && !within(wd, "/tmp") {
		return nil, fmt.Errorf("the current directory %s is outside %s and /tmp, which are all a staged image shares with this machine", wd, home)
	}
	args := []string{
		"--unshare-user", "--uid", "0", "--gid", "0", "--unshare-pid", "--die-with-parent",
		"--bind", s.Root, "/",
		"--dev", "/dev", "--proc", "/proc", "--ro-bind", "/sys", "/sys",
		"--bind", "/tmp", "/tmp",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind", "/etc/hosts", "/etc/hosts",
		"--bind", home, home,
		"--chdir", wd,
		"--clearenv",
	}
	for _, kv := range s.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			args = append(args, "--setenv", k, v)
		}
	}
	args = append(args, "--setenv", "HOME", home)
	if term := os.Getenv("TERM"); term != "" {
		args = append(args, "--setenv", "TERM", term)
	}
	return append(args, "--"), nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
