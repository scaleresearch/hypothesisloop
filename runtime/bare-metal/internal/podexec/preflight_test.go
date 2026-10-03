package podexec

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preflightExecutor(apiURL string, devices ...acceleratorDevice) *Executor {
	return &Executor{
		apiURL: apiURL, engineName: "docker", nodeName: "node-1", clusterID: "0123", storageDir: "/var/lib/docker",
		storageQuotaRefusal: "--storage-opt is supported only for overlay over xfs with 'pquota' mount option",
		probe:               func(context.Context) ([]acceleratorDevice, error) { return devices, nil },
	}
}

func healthy(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A node that cannot reach the control plane would start, show Disconnected and say why only in
// a log nobody reads. It says so before anything else and does not start.
func TestPreflightStopsANodeThatCannotReachTheControlPlane(t *testing.T) {
	gone := healthy(t)
	gone.Close()
	if _, err := preflightExecutor(gone.URL).Preflight(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), gone.URL+"/health") {
		t.Fatalf("unreachable API: err = %v, want one naming the address asked", err)
	}

	notTheAPI := httptest.NewServer(http.NotFoundHandler())
	defer notTheAPI.Close()
	if _, err := preflightExecutor(notTheAPI.URL).Preflight(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("an address that is not the API: err = %v, want the status it answered", err)
	}
}

func TestPreflightNamesTheAcceleratorsAsJobsAskForThem(t *testing.T) {
	server := healthy(t)
	chip := acceleratorDevice{DevicePath: "/dev/tenstorrent/0", Labels: []string{"tenstorrent.com/chipArch=blackhole"}}
	reserved := filepath.Join(t.TempDir(), "nr_hugepages")
	if err := os.WriteFile(reserved, []byte("4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := hugepageSizes
	hugepageSizes = map[string]hugepageSize{"hugepages-1Gi": {reserved: reserved}}
	defer func() { hugepageSizes = old }()

	lines, err := preflightExecutor(server.URL, chip, chip).Preflight(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	said := strings.Join(lines, "\n")
	for _, want := range []string{"accelerators: 2 x tenstorrent.com/chipArch=blackhole", "hugepages: 4 x 1 GiB reserved", "not enforced here: --storage-opt is supported only", "container engine: docker"} {
		if !strings.Contains(said, want) {
			t.Errorf("preflight did not say %q:\n%s", want, said)
		}
	}

	if err := os.WriteFile(reserved, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, _ = preflightExecutor(server.URL, chip).Preflight(context.Background(), server.Client())
	if !strings.Contains(strings.Join(lines, "\n"), "no 1 GiB hugepages are reserved") {
		t.Errorf("a Tenstorrent node without hugepages was not told so:\n%s", strings.Join(lines, "\n"))
	}
	if err := os.WriteFile(reserved, []byte("many\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, _ = preflightExecutor(server.URL, chip).Preflight(context.Background(), server.Client())
	if said := strings.Join(lines, "\n"); !strings.Contains(said, "cannot read how many 1 GiB hugepages") || strings.Contains(said, "no 1 GiB hugepages are reserved") {
		t.Errorf("a count that is no number was read as none reserved:\n%s", said)
	}

	lines, _ = preflightExecutor(server.URL).Preflight(context.Background(), server.Client())
	said = strings.Join(lines, "\n")
	if !strings.Contains(said, "accelerators: none found") || strings.Contains(said, "hugepages") {
		t.Errorf("a node with no accelerator:\n%s", said)
	}
}

// The probe runs before every pass, so a chip whose id this build does not know failed the whole
// inventory and with it every start and stop on the node.
func TestAChipTheProbeCannotReadIsSkippedAndTheRestReported(t *testing.T) {
	sys := t.TempDir()
	class := filepath.Join(sys, "class")
	if err := os.MkdirAll(class, 0o755); err != nil {
		t.Fatal(err)
	}
	for index, deviceID := range map[string]string{"0": "0xb140", "1": "0xffff"} {
		chip := filepath.Join(sys, "pci"+index, "tenstorrent", "tenstorrent!"+index)
		if err := os.MkdirAll(chip, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sys, "pci"+index, "device"), []byte(deviceID+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(chip, filepath.Join(class, "tenstorrent!"+index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(sys, "gone"), filepath.Join(class, "tenstorrent!2")); err != nil {
		t.Fatal(err)
	}
	old := tenstorrentClassDir
	tenstorrentClassDir = class
	defer func() { tenstorrentClassDir = old }()

	devices, err := probeTenstorrent(context.Background())
	if err != nil || len(devices) != 1 || devices[0].DevicePath != "/dev/tenstorrent/0" || devices[0].Labels[0] != "tenstorrent.com/chipArch=blackhole" {
		t.Fatalf("probe = %+v (err %v), want the one readable Blackhole chip", devices, err)
	}
}

// An agent that cannot load the NVIDIA library reads a GPU node as one with no GPUs. The card is
// on the PCI bus whatever the agent can load, so it stops and says so.
func TestPreflightStopsANodeWhoseNVIDIACardsItCannotRead(t *testing.T) {
	server := healthy(t)
	bus := t.TempDir()
	for name, attributes := range map[string][2]string{
		"0000:01:00.0": {"0x10de", "0x030200"},
		"0000:01:00.1": {"0x10de", "0x040300"},
		"0000:02:00.0": {"0x8086", "0x030000"},
	} {
		if err := os.MkdirAll(filepath.Join(bus, name), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(bus, name, "vendor"), []byte(attributes[0]+"\n"), 0o644)
		os.WriteFile(filepath.Join(bus, name, "class"), []byte(attributes[1]+"\n"), 0o644)
	}
	old := pciDevicesDir
	pciDevicesDir = bus
	defer func() { pciDevicesDir = old }()

	_, err := preflightExecutor(server.URL).Preflight(context.Background(), server.Client())
	if err == nil || !strings.Contains(err.Error(), "1 NVIDIA card(s)") {
		t.Fatalf("err = %v, want the one GPU on the bus named, its audio function and the Intel card left out", err)
	}
	gpu := acceleratorDevice{DevicePath: "/dev/nvidia0", Labels: []string{"nvidia.com/gpu.product=NVIDIA-L40"}}
	if _, err := preflightExecutor(server.URL, gpu).Preflight(context.Background(), server.Client()); err != nil {
		t.Fatalf("a node that reads its GPU: %v", err)
	}

}

// An entry of the bus that cannot be identified may be any device of the host, and an NVIDIA
// device that cannot be read fully is skipped as the probe skips a GPU it cannot read. Neither
// stops the node: a box with a chip of another vendor must start beside an unreadable entry.
func TestPreflightSkipsAndNamesAPCIDeviceItCannotRead(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	server := healthy(t)
	bus := t.TempDir()
	for name, attributes := range map[string][2]string{
		"0000:01:00.0": {"0x10de", ""},
		"0000:02:00.0": {"", "0x030000"},
		"0000:03:00.0": {"0x1e52", "0x120000"},
	} {
		if err := os.MkdirAll(filepath.Join(bus, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for file, value := range map[string]string{"vendor": attributes[0], "class": attributes[1]} {
			if value != "" {
				os.WriteFile(filepath.Join(bus, name, file), []byte(value+"\n"), 0o644)
			}
		}
	}
	old := pciDevicesDir
	pciDevicesDir = bus
	defer func() { pciDevicesDir = old }()

	chip := acceleratorDevice{DevicePath: "/dev/tenstorrent/0", Labels: []string{"tenstorrent.com/chipArch=blackhole"}}
	if _, err := preflightExecutor(server.URL, chip).Preflight(context.Background(), server.Client()); err != nil {
		t.Fatalf("a node with unreadable PCI entries beside its chip: %v, want it to start", err)
	}
	for _, want := range []string{
		"skipping NVIDIA PCI device " + filepath.Join(bus, "0000:01:00.0"),
		"skipping PCI device " + filepath.Join(bus, "0000:02:00.0") + ", which cannot be identified",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logged.String())
		}
	}
	if strings.Contains(logged.String(), "0000:03:00.0") {
		t.Errorf("a readable device of another vendor was logged:\n%s", logged.String())
	}
}
