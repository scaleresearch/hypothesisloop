// Package hostinstall puts the bare-metal agent on a node as a systemd unit and takes it off
// again. Everything it does to the node goes through Host, so the steps are tested without one.
package hostinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	BinaryPath = "/usr/local/bin/bare-agent"
	EnvPath    = "/etc/hypothesisloop/bare-agent.env"
	UnitName   = "hypothesisloop-agent.service"
	UnitPath   = "/etc/systemd/system/" + UnitName
)

// Host is the node as the installer touches it: four files and systemctl.
type Host interface {
	ReadFile(path string) ([]byte, error)
	// WriteFile replaces path in one step, so a running binary or a unit being read is never
	// seen half written.
	WriteFile(path string, data []byte, mode os.FileMode) error
	// Rename moves from over to in one step. Both are in one directory.
	Rename(from, to string) error
	// Remove of a file that is not there is not an error.
	Remove(path string) error
	Systemctl(args ...string) error
}

// Settings is what the agent of one node runs with. The file that holds them is readable by
// root only, since the token is in it.
type Settings struct {
	ClusterName string
	APIURL      string
	Token       string
}

func (s Settings) envFile() ([]byte, error) {
	var out strings.Builder
	for _, pair := range [][2]string{{"CLUSTER_NAME", s.ClusterName}, {"API_URL", s.APIURL}, {"RUNTIME_TOKEN", s.Token}} {
		if pair[1] == "" {
			return nil, fmt.Errorf("%s is empty", pair[0])
		}
		if strings.ContainsAny(pair[1], "\n\r") {
			return nil, fmt.Errorf("%s holds a line break", pair[0])
		}
		// systemd reads a double-quoted value with backslash escapes and expands nothing in it.
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(pair[1])
		fmt.Fprintf(&out, "%s=\"%s\"\n", pair[0], escaped)
	}
	return []byte(out.String()), nil
}

// ReadSettings reads what Install wrote. installed is false on a node with no agent installed.
func ReadSettings(host Host) (settings Settings, installed bool, err error) {
	raw, err := host.ReadFile(EnvPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("read %s: %w", EnvPath, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, quoted, found := strings.Cut(line, "=")
		if !found || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			continue
		}
		values[key] = strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(quoted[1 : len(quoted)-1])
	}
	settings = Settings{ClusterName: values["CLUSTER_NAME"], APIURL: values["API_URL"], Token: values["RUNTIME_TOKEN"]}
	if settings.ClusterName == "" || settings.APIURL == "" || settings.Token == "" {
		return Settings{}, false, fmt.Errorf("%s does not hold CLUSTER_NAME, API_URL and RUNTIME_TOKEN", EnvPath)
	}
	return settings, true, nil
}

// engineUnits is what must be up before the agent can reach its engine.
var engineUnits = map[string]string{"docker": "docker.service", "podman": "podman.socket"}

func unit(engine string) ([]byte, error) {
	engineUnit, known := engineUnits[engine]
	if !known {
		return nil, fmt.Errorf("no systemd unit is known for the container engine %q", engine)
	}
	return []byte(`[Unit]
Description=HypothesisLoop bare-metal agent
Wants=network-online.target ` + engineUnit + `
After=network-online.target ` + engineUnit + `

[Service]
EnvironmentFile=` + EnvPath + `
ExecStart=` + BinaryPath + `
# A stop that is not a failure stays a stop: the agent exits cleanly once its cluster is removed.
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`), nil
}

// Plan says what Install writes and runs on a node whose engine is engine, and changes nothing.
func Plan(engine string) (string, error) {
	unitFile, err := unit(engine)
	if err != nil {
		return "", err
	}
	indented := "    " + strings.ReplaceAll(strings.TrimSpace(string(unitFile)), "\n", "\n    ")
	return fmt.Sprintf(`would write:
  %s  the agent, mode 0755
  %s  CLUSTER_NAME, API_URL and RUNTIME_TOKEN, mode 0600 (root only)
  %s  started at boot and restarted on failure:
%s
would run:
  systemctl daemon-reload
  systemctl enable %s
  systemctl restart %s
`, BinaryPath, EnvPath, UnitPath, indented, UnitName, UnitName), nil
}

// Install writes the agent's binary, settings and unit, and starts the unit, once check passes.
// check runs the agent's own preflight with those settings and names the engine it found, which
// the unit is ordered after. When check fails the node is left as it was. Running Install again
// with another binary is the upgrade.
//
// The three files are written beside the ones they replace and renamed into place only once all
// three are written, so a write that fails leaves the install that was there whole, and the
// agent of that install running. A systemctl call that fails leaves the new install whole on
// disk and the agent that was running untouched.
func Install(host Host, binary []byte, settings Settings, check func() (engine string, err error)) error {
	env, err := settings.envFile()
	if err != nil {
		return err
	}
	engine, err := check()
	if err != nil {
		return err
	}
	unitFile, err := unit(engine)
	if err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{{BinaryPath, binary, 0o755}, {EnvPath, env, 0o600}, {UnitPath, unitFile, 0o644}}
	for _, file := range files {
		if err := host.WriteFile(staged(file.path), file.data, file.mode); err != nil {
			for _, written := range files {
				err = errors.Join(err, host.Remove(staged(written.path)))
			}
			return err
		}
	}
	for _, file := range files {
		if err := host.Rename(staged(file.path), file.path); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", UnitName}, {"restart", UnitName}} {
		if err := host.Systemctl(args...); err != nil {
			return err
		}
	}
	return nil
}

// staged is where a file of an install waits beside path until all of them are written. systemd
// reads no unit from a name that ends this way.
func staged(path string) string { return path + ".new" }

// Attached reports whether the unit is installed and enabled, which is how a node that has an
// agent for a cluster differs from one that detached.
func Attached(host Host) bool {
	if _, err := host.ReadFile(UnitPath); err != nil {
		return false
	}
	return host.Systemctl("is-enabled", "--quiet", UnitName) == nil
}

// Stop stops the unit and keeps it from starting at boot. The files stay.
func Stop(host Host) error {
	if _, err := host.ReadFile(UnitPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return host.Systemctl("disable", "--now", UnitName)
}

// StayStopped keeps the unit from starting at boot. It is what the running agent calls on its
// way out once its cluster is removed, where stopping the unit would be stopping itself.
func StayStopped(host Host) error {
	if _, err := host.ReadFile(UnitPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return host.Systemctl("disable", UnitName)
}

// Uninstall removes the unit, the settings and the binary. It refuses a node that is still
// attached: removing its agent would leave the cluster registered and its job containers
// running with nothing to stop them.
func Uninstall(host Host) error {
	if Attached(host) {
		return fmt.Errorf("this node is still attached: run bare-agent detach first, which stops its jobs")
	}
	if err := Stop(host); err != nil {
		return err
	}
	for _, path := range []string{UnitPath, EnvPath, filepath.Dir(EnvPath), BinaryPath} {
		if err := host.Remove(path); err != nil {
			return err
		}
	}
	return host.Systemctl("daemon-reload")
}

// Node is the real Host.
type Node struct{}

func (Node) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (Node) WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(staged.Name())
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := staged.Chmod(mode); err != nil {
		staged.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (Node) Rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("put %s in place: %w", to, err)
	}
	return nil
}

func (Node) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func (Node) Systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
