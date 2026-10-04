package hostinstall

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeHost is a node held in memory: its files with their modes, and what systemctl was asked.
type fakeHost struct {
	files   map[string]string
	modes   map[string]os.FileMode
	asked   []string
	enabled bool
	refuse  string
	// unwritable is a path every write to fails.
	unwritable string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: map[string]string{}, modes: map[string]os.FileMode{}}
}

func (h *fakeHost) ReadFile(path string) ([]byte, error) {
	content, found := h.files[path]
	if !found {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (h *fakeHost) WriteFile(path string, data []byte, mode os.FileMode) error {
	if path == h.unwritable {
		return errors.New("write " + path + ": no space left on device")
	}
	h.files[path], h.modes[path] = string(data), mode
	return nil
}

func (h *fakeHost) Rename(from, to string) error {
	content, found := h.files[from]
	if !found {
		return fs.ErrNotExist
	}
	h.files[to], h.modes[to] = content, h.modes[from]
	delete(h.files, from)
	delete(h.modes, from)
	return nil
}

func (h *fakeHost) Remove(path string) error {
	delete(h.files, path)
	return nil
}

func (h *fakeHost) Systemctl(args ...string) error {
	call := strings.Join(args, " ")
	h.asked = append(h.asked, call)
	if call == h.refuse {
		return errors.New("systemctl " + call + ": refused")
	}
	switch args[0] {
	case "is-enabled":
		if !h.enabled {
			return errors.New("disabled")
		}
	case "enable":
		h.enabled = true
	case "disable":
		h.enabled = false
	}
	return nil
}

var settings = Settings{ClusterName: "gpu-box", APIURL: `https://api.example/a?b="1"\2&c=$(id)`, Token: "hls_cnVudGltZQ.c2ln"}

func passes(engine string) func() (string, error) {
	return func() (string, error) { return engine, nil }
}

func TestInstallWritesTheAgentAndStartsItAfterItsEngine(t *testing.T) {
	host := newFakeHost()
	if err := Install(host, []byte("binary-1"), settings, passes("docker")); err != nil {
		t.Fatal(err)
	}
	if host.files[BinaryPath] != "binary-1" || host.modes[BinaryPath] != 0o755 {
		t.Errorf("binary = %q mode %o", host.files[BinaryPath], host.modes[BinaryPath])
	}
	if host.modes[EnvPath] != 0o600 {
		t.Errorf("the file that holds the token has mode %o, want it readable by root only", host.modes[EnvPath])
	}
	unit := host.files[UnitPath]
	for _, want := range []string{"After=network-online.target docker.service", "Wants=network-online.target docker.service",
		"Restart=on-failure", "EnvironmentFile=" + EnvPath, "ExecStart=" + BinaryPath, "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, settings.Token) {
		t.Error("the token is in the unit, which every user of the node reads")
	}
	if want := []string{"daemon-reload", "enable " + UnitName, "restart " + UnitName}; !slices.Equal(host.asked, want) {
		t.Errorf("systemctl was asked %v, want %v", host.asked, want)
	}

	read, installed, err := ReadSettings(host)
	if err != nil || !installed || read != settings {
		t.Errorf("settings read back = %+v, installed = %v, err = %v; want what was installed", read, installed, err)
	}
}

func TestInstallOnAPodmanNodeOrdersTheUnitAfterPodmansSocket(t *testing.T) {
	host := newFakeHost()
	if err := Install(host, []byte("b"), settings, passes("podman")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(host.files[UnitPath], "After=network-online.target podman.socket") {
		t.Errorf("unit:\n%s", host.files[UnitPath])
	}
}

// A node that fails its preflight would otherwise be enabled, fail at every boot and restart
// every ten seconds for good.
func TestInstallLeavesTheNodeAsItWasWhenThePreflightFails(t *testing.T) {
	host := newFakeHost()
	err := Install(host, []byte("b"), settings, func() (string, error) {
		return "", errors.New("the control plane does not answer")
	})
	if err == nil || !strings.Contains(err.Error(), "the control plane does not answer") {
		t.Fatalf("err = %v, want the preflight's", err)
	}
	if len(host.asked) != 0 || len(host.files) != 0 {
		t.Errorf("systemctl was asked %v and files were written after a failed preflight: %v", host.asked, host.files)
	}
}

// Running the installer again with the binary of another image tag is the upgrade.
func TestInstallAgainReplacesTheBinaryAndRestartsTheAgent(t *testing.T) {
	host := newFakeHost()
	for _, binary := range []string{"binary-1", "binary-2"} {
		if err := Install(host, []byte(binary), settings, passes("docker")); err != nil {
			t.Fatal(err)
		}
	}
	if host.files[BinaryPath] != "binary-2" {
		t.Errorf("binary = %q after the second install", host.files[BinaryPath])
	}
	if last := host.asked[len(host.asked)-1]; last != "restart "+UnitName {
		t.Errorf("the last systemctl call was %q, so the running agent is still the old binary", last)
	}
}

// An upgrade that cannot write one of its files must not leave the new binary under the old unit
// and settings: the agent that runs would be restarted into a mix at the next boot.
func TestAnInstallThatCannotWriteOneFileLeavesTheInstallBeforeItWhole(t *testing.T) {
	for _, unwritable := range []string{BinaryPath, EnvPath, UnitPath} {
		host := newFakeHost()
		if err := Install(host, []byte("binary-1"), settings, passes("docker")); err != nil {
			t.Fatal(err)
		}
		before, asked := maps.Clone(host.files), len(host.asked)
		host.unwritable = unwritable + ".new"

		moved := Settings{ClusterName: "gpu-box", APIURL: "https://api.other.example", Token: "hls_b3RoZXI.c2ln"}
		err := Install(host, []byte("binary-2"), moved, passes("podman"))
		if err == nil || !strings.Contains(err.Error(), "no space left on device") {
			t.Fatalf("%s unwritable: err = %v, want the write's", unwritable, err)
		}
		if !maps.Equal(host.files, before) {
			t.Errorf("%s unwritable: files = %v, want the first install and nothing beside it: %v", unwritable, host.files, before)
		}
		if len(host.asked) != asked {
			t.Errorf("%s unwritable: systemctl was asked %v after the failed write", unwritable, host.asked[asked:])
		}
	}
}

func TestSettingsThatWouldBreakTheFileAreRefused(t *testing.T) {
	for _, broken := range []Settings{
		{ClusterName: "a", APIURL: "http://x", Token: "t\nINJECTED=1"},
		{ClusterName: "a", APIURL: "http://x", Token: ""},
	} {
		host := newFakeHost()
		if err := Install(host, []byte("b"), broken, passes("docker")); err == nil {
			t.Errorf("settings %+v were installed", broken)
		}
		if len(host.files) != 0 {
			t.Errorf("settings %+v: files written: %v", broken, host.files)
		}
	}
}

// Removing the agent of an attached node would leave its cluster registered and its job
// containers running with nothing left to stop them.
func TestUninstallRefusesAnAttachedNodeAndRemovesADetachedOne(t *testing.T) {
	host := newFakeHost()
	if err := Install(host, []byte("b"), settings, passes("docker")); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(host); err == nil || !strings.Contains(err.Error(), "bare-agent detach") {
		t.Fatalf("uninstalling an attached node: err = %v, want it to name detach", err)
	}
	if host.files[BinaryPath] == "" {
		t.Fatal("the refused uninstall removed the binary")
	}

	if err := Stop(host); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(host.asked, "disable --now "+UnitName) {
		t.Fatalf("systemctl was asked %v, want the unit stopped and disabled", host.asked)
	}
	if err := Uninstall(host); err != nil {
		t.Fatal(err)
	}
	if len(host.files) != 0 {
		t.Errorf("files left after uninstall: %v", host.files)
	}
	if last := host.asked[len(host.asked)-1]; last != "daemon-reload" {
		t.Errorf("the last systemctl call was %q, so systemd still knows the unit", last)
	}
}

// The agent that exits because its cluster was removed is the unit itself, so it only keeps
// itself from coming back at boot.
func TestARemovedClustersAgentKeepsItselfFromStartingAgain(t *testing.T) {
	host := newFakeHost()
	if err := StayStopped(host); err != nil || len(host.asked) != 0 {
		t.Fatalf("an agent run by hand asked systemctl %v, err = %v", host.asked, err)
	}
	if err := Install(host, []byte("b"), settings, passes("docker")); err != nil {
		t.Fatal(err)
	}
	if err := StayStopped(host); err != nil {
		t.Fatal(err)
	}
	if last := host.asked[len(host.asked)-1]; last != "disable "+UnitName {
		t.Errorf("the last systemctl call was %q", last)
	}
	if Attached(host) {
		t.Error("the node still reads as attached")
	}
}
