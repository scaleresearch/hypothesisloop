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
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"
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

// stageDir holds what is staged on this machine: roots/, staged.json naming the current one, and
// stage.lock. It is under the user's cache, outside every repository.
func stageDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "hl", "env"), nil
}

// stageWait is how long hl env stage waits before it asks again for an image whose environment is
// still being built or pushed.
var stageWait = 5 * time.Second

func envStage(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env stage", `usage: hl env stage [JOB] [--platform-experiment PE] [--timeout SECONDS]

Pulls the image job JOB runs in, its environment once built, and extracts it on this machine,
where hl env run runs commands in it. JOB defaults to the platform experiment's baseline job, so
with no JOB this is the image every job built on the baseline runs in; the harness stages it in
the background when a session starts. Stage a job of yours after its commit changed the
environment. While the job's environment is being built or pushed it waits for it, as hl job
wait waits for a job, and exits 124 at --timeout; it fails at once when the environment failed.
The machine holds one staged image: a new one replaces it once it is whole, until then hl env
run uses the old one, and a command already running in the old one keeps it until it ends.
Staging an image already staged does nothing. Prints what it staged as one JSON line.

Refusals: environment_failed (the job's environment did not build: its message says why);
run_over.`, stderr)
	pe := experimentFlag(fs)
	timeout := fs.Float64("timeout", 540, "give up waiting for the environment after this many `seconds`")
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one JOB, got %d", len(operands)))
	}
	if *timeout <= 0 {
		return parseExit(misuse(fs, "--timeout wants a positive number of seconds"))
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
	deadline := time.Now().Add(time.Duration(*timeout * float64(time.Second)))
	out, err := stageJob(c, dir, operands, *pe, deadline, stderr)
	if err != nil {
		code := 1
		var waited *stageTimeout
		if errors.As(err, &waited) {
			code = watchclient.TimeoutExitCode
		}
		_ = os.WriteFile(filepath.Join(dir, "stage.failed"), []byte(err.Error()), 0o644)
		fail(stderr, fmt.Errorf("env stage: %w", err))
		return code
	}
	if err := os.Remove(filepath.Join(dir, "stage.failed")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(stderr, fmt.Errorf("env stage: %w", err))
	}
	return printStaged(stdout, out)
}

// stageTimeout is a stage that gave up waiting for its environment, message saying where it is.
type stageTimeout struct{ message string }

func (e *stageTimeout) Error() string {
	return "timed out waiting for the environment, which is not ready yet: " + e.message
}

// stageJob stages the image of the one job of operands, the baseline of pe without one, once its
// environment is ready, unless it is staged already.
func stageJob(c *apiclient.Client, dir string, operands []string, pe string, deadline time.Time, stderr io.Writer) (*staged, error) {
	var job string
	if len(operands) == 1 {
		job = operands[0]
	} else {
		var err error
		if job, err = baselineJob(c, experimentOf(c, pe)); err != nil {
			return nil, err
		}
	}
	var credential struct {
		Image          string `json:"image"`
		EnvironmentKey string `json:"environment_key"`
		Login          *struct {
			Registry string `json:"registry"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"login"`
	}
	for said := false; ; said = true {
		err := c.Do("POST", "/experiments/"+url.PathEscape(job)+"/image-credentials", nil, &credential)
		reason, message := refusalOf(err)
		if reason != "environment_not_ready" {
			if err != nil {
				return nil, err
			}
			break
		}
		if time.Now().Add(stageWait).After(deadline) {
			return nil, &stageTimeout{message}
		}
		if !said {
			fmt.Fprintf(stderr, "hl: env stage: waiting until the environment is ready: %s\n", message)
		}
		time.Sleep(stageWait)
	}
	if current, err := readStaged(dir); err == nil && current.Image == credential.Image {
		if _, err := os.Stat(current.Root); err == nil {
			return current, nil
		}
	}
	auth := authn.Anonymous
	if credential.Login != nil {
		auth = &authn.Basic{Username: credential.Login.Username, Password: credential.Login.Password}
	}
	started := time.Now()
	out, err := stage(context.Background(), dir, credential.Image, auth)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", credential.Image, err)
	}
	out.Job, out.EnvironmentKey = job, credential.EnvironmentKey
	if err := writeStaged(dir, out); err != nil {
		return nil, err
	}
	if err := removeUnused(filepath.Dir(out.Root), out.Root); err != nil {
		return nil, err
	}
	fmt.Fprintf(stderr, "hl: env stage: %s staged in %s\n", credential.Image, time.Since(started).Round(time.Second))
	return out, nil
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

// awaitStage waits for the hl env stage running now on dir to end, reporting whether one was.
func awaitStage(dir string, stderr io.Writer) bool {
	f, err := os.OpenFile(filepath.Join(dir, "stage.lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		return false
	}
	fmt.Fprintln(stderr, "hl: env run: nothing is staged yet and hl env stage is staging now: waiting for it to end")
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH) == nil
}

// stage extracts image into a root of its own under dir/roots, applying its layers in order.
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
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	roots := filepath.Join(dir, "roots")
	if err := os.MkdirAll(roots, 0o755); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(roots, digest.Hex[:12]+"-")
	if err != nil {
		return nil, err
	}
	for _, layer := range layers {
		if err := applyLayerOf(layer, root); err != nil {
			os.RemoveAll(root)
			return nil, err
		}
	}
	bytes, err := regularBytes(root)
	if err != nil {
		return nil, err
	}
	return &staged{Image: image, Env: config.Config.Env, Root: root, Bytes: bytes, StagedAt: time.Now().UTC()}, nil
}

func applyLayerOf(layer v1.Layer, root string) error {
	r, err := layer.Uncompressed()
	if err != nil {
		return err
	}
	defer r.Close()
	return applyLayer(r, root)
}

// applyLayer writes the layer r over root, which holds the layers below it, as an overlay engine
// merges them: a whiteout removes what the layers below hold, and a path is taken as it is named,
// never through a symlink. Owners are not kept: every file is the caller's, which hl env run maps
// to root. Device files are left out, as hl env run gives the command a /dev of its own.
func applyLayer(r io.Reader, root string) error {
	written := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		dir, base := path.Split(path.Clean("/" + h.Name))
		if base == "" {
			continue
		}
		parent, err := directoryAt(root, dir)
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		target := filepath.Join(parent, base)
		switch {
		case base == ".wh..wh..opq":
			err = pruneChildren(parent, written)
		case strings.HasPrefix(base, ".wh."):
			err = prune(filepath.Join(parent, strings.TrimPrefix(base, ".wh.")), written)
		default:
			written[target] = true
			err = writeEntry(h, tr, root, target)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
}

// prune removes what the layers below left at p, keeping what the current layer wrote.
func prune(p string, written map[string]bool) error {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !written[p] {
		return os.RemoveAll(p)
	}
	if info.IsDir() {
		return pruneChildren(p, written)
	}
	return nil
}

func pruneChildren(dir string, written map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := prune(filepath.Join(dir, e.Name()), written); err != nil {
			return err
		}
	}
	return nil
}

// directoryAt makes each component of dir under root a directory, in place of a file or symlink a
// layer below left there, as a directory of an upper layer hides what is beneath it.
func directoryAt(root, dir string) (string, error) {
	p := root
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			err = os.Remove(p)
		case errors.Is(err, fs.ErrNotExist):
			err = nil
		}
		if err == nil {
			err = os.Mkdir(p, 0o755)
		}
		if err != nil {
			return "", err
		}
	}
	return p, nil
}

func writeEntry(h *tar.Header, r io.Reader, root, target string) error {
	if info, err := os.Lstat(target); err == nil && !(info.IsDir() && h.Typeflag == tar.TypeDir) {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	mode := h.FileInfo().Mode().Perm()
	switch h.Typeflag {
	case tar.TypeDir:
		// Owner-writable, so that the root can be removed.
		if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return os.Chmod(target, mode|0o700)
	case tar.TypeReg:
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if err == nil {
			err = f.Chmod(mode)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	case tar.TypeSymlink:
		return os.Symlink(h.Linkname, target)
	case tar.TypeLink:
		dir, base := path.Split(path.Clean("/" + h.Linkname))
		linkParent, err := directoryAt(root, dir)
		if err != nil {
			return err
		}
		return os.Link(filepath.Join(linkParent, base), target)
	}
	return nil
}

func regularBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// removeUnused removes every root under roots but current that no hl env run holds.
func removeUnused(roots, current string) error {
	entries, err := os.ReadDir(roots)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(roots, e.Name())
		if p == current {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			err = os.RemoveAll(p)
		} else if errors.Is(err, syscall.EWOULDBLOCK) {
			err = nil
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// holdStaged answers what is staged, its root held from removal by a shared lock on a descriptor
// of the root. syscall.Open leaves the descriptor open across exec, so the lock lasts as long as
// the command run in the root.
func holdStaged(dir string) (*staged, int, error) {
	s, err := readStaged(dir)
	for {
		if err != nil {
			return nil, 0, err
		}
		fd, openErr := syscall.Open(s.Root, syscall.O_RDONLY, 0)
		if openErr == nil {
			if err := syscall.Flock(fd, syscall.LOCK_SH); err != nil {
				return nil, 0, err
			}
		}
		var now *staged
		now, err = readStaged(dir)
		if err == nil && now.Root == s.Root {
			if openErr != nil {
				return nil, 0, fmt.Errorf("the staged root %s: %v: hl env stage stages it again", s.Root, openErr)
			}
			return s, fd, nil
		}
		if openErr == nil {
			syscall.Close(fd)
		}
		s = now
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
imports, and what needs the device fails. With nothing staged yet it waits for the hl env stage
running now, and says why when none is. Exits with COMMAND's status.`, stderr)
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
	s, _, err := holdStaged(dir)
	if errors.Is(err, fs.ErrNotExist) && awaitStage(dir, stderr) {
		s, _, err = holdStaged(dir)
	}
	if errors.Is(err, fs.ErrNotExist) {
		why := "no hl env stage has run here"
		if failed, readErr := os.ReadFile(filepath.Join(dir, "stage.failed")); readErr == nil {
			why = "the last hl env stage failed: " + string(failed)
		}
		return fail(stderr, fmt.Errorf("env run: nothing is staged on this machine, as %s. Run hl env stage for the baseline's image, or hl env stage JOB for a job of yours, then this again", why))
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
