package providers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

type exampleSettings struct {
	Region string `yaml:"region"`
}

type exampleAccelerator struct {
	Model string `yaml:"model"`
}

func settingsNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var holder struct {
		Settings yaml.Node `yaml:"settings"`
	}
	if err := yaml.Unmarshal([]byte(text), &holder); err != nil {
		t.Fatal(err)
	}
	return holder.Settings
}

func TestAProviderIsBuiltFromItsOwnSectionOfTheConfig(t *testing.T) {
	var got exampleSettings
	var gotSecret string
	var gotTypes map[string]exampleAccelerator
	byName["example"] = adapter(func(s exampleSettings, secret string, accelerators map[string]exampleAccelerator, _ func(string, ...any)) (*fake.Provider, error) {
		got, gotSecret, gotTypes = s, secret, accelerators
		return fake.New(), nil
	})
	defer delete(byName, "example")

	const h100 = "nvidia.com/gpu.product=NVIDIA-H100"
	rented, err := New("example", settingsNode(t, "settings: {region: eu}"), "a-secret", map[string]yaml.Node{h100: settingsNode(t, "settings: {model: H100 SXM}")}, t.Logf)
	if err != nil || rented == nil || got.Region != "eu" || gotSecret != "a-secret" {
		t.Fatalf("provider %v, settings %+v, %v", rented, got, err)
	}
	if len(gotTypes) != 1 || gotTypes[h100].Model != "H100 SXM" {
		t.Errorf("accelerators handed to the provider = %v", gotTypes)
	}
	if _, err := New("example", yaml.Node{}, "", map[string]yaml.Node{h100: settingsNode(t, "settings: {modle: H100 SXM}")}, t.Logf); err == nil || !strings.Contains(err.Error(), h100) {
		t.Errorf("a misspelled key of an accelerator's entry: err = %v", err)
	}
	if _, err := New("example", settingsNode(t, "settings: {regoin: eu}"), "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "the section of the provider") {
		t.Errorf("a misspelled provider setting: err = %v", err)
	}
	if _, err := New("example", yaml.Node{}, "", nil, t.Logf); err != nil {
		t.Errorf("a provider with no settings of its own: %v", err)
	}
	if _, err := New("vast", settingsNode(t, "settings: {account_id: \"1\", max_price_per_hour: 1, accelerators: []}"), "a-key", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "accelerators") {
		t.Errorf("a provider section that names its own accelerators, which only the catalog names: err = %v", err)
	}
}

func TestAProviderTheBinaryWasNotBuiltWithIsRefusedWithTheOnesItHas(t *testing.T) {
	_, err := New("other", yaml.Node{}, "", nil, t.Logf)
	if err == nil || !strings.Contains(err.Error(), `"other"`) || !strings.Contains(err.Error(), "("+strings.Join(Names(), ", ")+")") {
		t.Fatalf("err = %v, want one naming the provider and listing %v", err, Names())
	}
}

// shippedSections is every section of a provider the shipped example of a settings section holds,
// in any of its entries, by the provider's name, with each placeholder filled in.
func shippedSections(t *testing.T, path, key string) map[string]yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]yaml.Node
	if err := yaml.Unmarshal(regexp.MustCompile(`REPLACE-WITH-[A-Z-]+`).ReplaceAll(raw, []byte("made-up")), &document); err != nil {
		t.Fatal(err)
	}
	section := document[key]
	entries := []*yaml.Node{&section}
	if section.Kind == yaml.SequenceNode {
		entries = section.Content
	}
	sections := map[string]yaml.Node{}
	for _, entry := range entries {
		var keys map[string]yaml.Node
		if err := entry.Decode(&keys); err != nil {
			t.Fatal(err)
		}
		for _, name := range Names() {
			if node, found := keys[name]; found {
				sections[name] = node
			}
		}
	}
	return sections
}

// Every provider has its section in the shipped example of the settings section it rents for, and
// the section builds its adapter, with every accelerator type the shipped catalog names on it.
func TestTheShippedProviderSectionsBuildTheirAdapters(t *testing.T) {
	shipped := shippedSections(t, "../../../../controlplane/settings/sandbox.yaml", "sandbox")
	for name, node := range shippedSections(t, "../../../../controlplane/settings/cloud-compute.yaml", "cloud_compute") {
		shipped[name] = node
	}
	catalog := shippedCatalog(t)
	for _, name := range Names() {
		if name == "fake" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			node, found := shipped[name]
			if !found {
				t.Fatalf("no shipped example of the settings has a section of %s", name)
			}
			if _, err := New(name, node, "FlyV1 not-a-real-key", catalog[name], t.Logf); err != nil {
				t.Fatalf("provider: %v", err)
			}
			if name != "vast" && name != "runpod" {
				return
			}
			if len(catalog[name]) == 0 {
				t.Fatalf("the shipped catalog names no accelerator type on %s", name)
			}
			_, err := New(name, node, "FlyV1 not-a-real-key", map[string]yaml.Node{"nvidia.com/gpu.product=NVIDIA-H100": {}}, t.Logf)
			if err == nil || !strings.Contains(err.Error(), "NVIDIA-H100") {
				t.Errorf("err = %v, want a refusal of an accelerator with an empty entry", err)
			}
		})
	}
}

// shippedCatalog is the shipped catalog's entry of every accelerator type on each provider, by
// provider and then type.
func shippedCatalog(t *testing.T) map[string]map[string]yaml.Node {
	t.Helper()
	raw, err := os.ReadFile("../../../../controlplane/settings/hypothesisloop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		AcceleratorTypes []struct {
			Name      string               `yaml:"name"`
			Providers map[string]yaml.Node `yaml:"providers"`
		} `yaml:"accelerator_types"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]yaml.Node{}
	for _, accelerator := range document.AcceleratorTypes {
		for name, entry := range accelerator.Providers {
			if out[name] == nil {
				out[name] = map[string]yaml.Node{}
			}
			out[name][accelerator.Name] = entry
		}
	}
	return out
}

// A config that names a vendor and leaves out its settings must stop the process at startup,
// with the setting that is missing, before anything is rented.
func TestAVendorNamedWithoutASettingsSectionIsRefusedAtStartup(t *testing.T) {
	for name, want := range map[string]string{
		"fly": `fly: org ""`,
		"e2b": "e2b: lifetime_seconds",
	} {
		for form, text := range map[string]string{"no settings key": "name: " + name, "an empty settings key": "name: " + name + "\nsettings:"} {
			t.Run(name+", "+form, func(t *testing.T) {
				if _, err := New(name, settingsNode(t, text), "a-secret", nil, t.Logf); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want %q", err, want)
				}
			})
		}
	}
}
