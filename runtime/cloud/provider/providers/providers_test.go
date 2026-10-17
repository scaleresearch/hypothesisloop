package providers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider/fake"
)

type exampleSettings struct {
	Region string `yaml:"region"`
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
	var gotTypes []string
	byName["example"] = adapter(func(s exampleSettings, secret string, acceleratorTypes []string, _ func(string, ...any)) (*fake.Provider, error) {
		got, gotSecret, gotTypes = s, secret, acceleratorTypes
		return fake.New(), nil
	})
	defer delete(byName, "example")

	rented, err := New("example", settingsNode(t, "settings: {region: eu}"), "a-secret", []string{"nvidia.com/gpu.product=NVIDIA-H100"}, t.Logf)
	if err != nil || rented == nil || got.Region != "eu" || gotSecret != "a-secret" {
		t.Fatalf("provider %v, settings %+v, %v", rented, got, err)
	}
	if len(gotTypes) != 1 || gotTypes[0] != "nvidia.com/gpu.product=NVIDIA-H100" {
		t.Errorf("accelerator types handed to the provider = %q", gotTypes)
	}
	if _, err := New("example", settingsNode(t, "settings: {regoin: eu}"), "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "the section of the provider") {
		t.Errorf("a misspelled provider setting: err = %v", err)
	}
	if _, err := New("example", yaml.Node{}, "", nil, t.Logf); err != nil {
		t.Errorf("a provider with no settings of its own: %v", err)
	}
}

func TestAProviderTheBinaryWasNotBuiltWithIsRefusedWithTheOnesItHas(t *testing.T) {
	_, err := New("other", yaml.Node{}, "", nil, t.Logf)
	if err == nil || !strings.Contains(err.Error(), `"other"`) || !strings.Contains(err.Error(), "(e2b, fly, vast)") {
		t.Fatalf("err = %v, want one naming the provider and listing e2b, fly and vast", err)
	}
}

// shippedSections is every section of a provider the shipped example of a settings section holds,
// by the provider's name, with each placeholder filled in.
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
	if section.Kind == yaml.SequenceNode {
		section = *section.Content[0]
	}
	var keys map[string]yaml.Node
	if err := section.Decode(&keys); err != nil {
		t.Fatal(err)
	}
	sections := map[string]yaml.Node{}
	for _, name := range Names() {
		if node, found := keys[name]; found {
			sections[name] = node
		}
	}
	return sections
}

// Every provider has its section in the shipped example of the settings section it rents for, and
// the section builds its adapter.
func TestTheShippedProviderSectionsBuildTheirAdapters(t *testing.T) {
	shipped := shippedSections(t, "../../../../controlplane/settings/sandbox.yaml", "sandbox")
	for name, node := range shippedSections(t, "../../../../controlplane/settings/cloud-compute.yaml", "cloud_compute") {
		shipped[name] = node
	}
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			node, found := shipped[name]
			if !found {
				t.Fatalf("no shipped example of the settings has a section of %s", name)
			}
			if _, err := New(name, node, "FlyV1 not-a-real-key", nil, t.Logf); err != nil {
				t.Fatalf("provider: %v", err)
			}
			_, err := New(name, node, "FlyV1 not-a-real-key", []string{"nvidia.com/gpu.product=NVIDIA-H100"}, t.Logf)
			if err == nil || !strings.Contains(err.Error(), "NVIDIA-H100") {
				t.Errorf("err = %v, want a refusal of an accelerator no shipped section rents", err)
			}
		})
	}
}

// A config that names a vendor and leaves out its settings must stop the process at startup,
// with the setting that is missing, before anything is rented.
func TestAVendorNamedWithoutASettingsSectionIsRefusedAtStartup(t *testing.T) {
	for name, want := range map[string]string{
		"fly": `fly: app ""`,
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
