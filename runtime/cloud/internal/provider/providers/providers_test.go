package providers

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
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
	var gotTypes []string
	byName["example"] = adapter(func(s exampleSettings, acceleratorTypes []string) (*fake.Provider, error) {
		got, gotTypes = s, acceleratorTypes
		return fake.New(), nil
	})
	defer delete(byName, "example")

	rented, err := New("example", settingsNode(t, "settings: {region: eu}"), []string{"nvidia.com/gpu.product=NVIDIA-H100"})
	if err != nil || rented == nil || got.Region != "eu" {
		t.Fatalf("provider %v, settings %+v, %v", rented, got, err)
	}
	if len(gotTypes) != 1 || gotTypes[0] != "nvidia.com/gpu.product=NVIDIA-H100" {
		t.Errorf("accelerator types handed to the provider = %q", gotTypes)
	}
	if _, err := New("example", settingsNode(t, "settings: {regoin: eu}"), nil); err == nil || !strings.Contains(err.Error(), "provider.settings") {
		t.Errorf("a misspelled provider setting: err = %v", err)
	}
	if _, err := New("example", yaml.Node{}, nil); err != nil {
		t.Errorf("a provider with no settings of its own: %v", err)
	}
}

func TestAProviderTheBinaryWasNotBuiltWithIsRefusedWithTheOnesItHas(t *testing.T) {
	_, err := New("other", yaml.Node{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"other"`) || !strings.Contains(err.Error(), "(e2b, fly, vast)") {
		t.Fatalf("err = %v, want one naming the provider and listing e2b, fly and vast", err)
	}
}

// section is the provider part of a config file, as every process that rents instances embeds it.
type section struct {
	Provider struct {
		Name     string    `yaml:"name"`
		Settings yaml.Node `yaml:"settings"`
	} `yaml:"provider"`
}

func shippedSection(t *testing.T, path string) section {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var read section
	if err := yaml.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	return read
}

func TestTheShippedProviderSectionsBuildTheirAdapters(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "FlyV1 not-a-real-token")
	t.Setenv("E2B_API_KEY", "not-a-real-key")
	for name, path := range map[string]string{
		"fly": "../../../settings/provider.fly.yaml",
		"e2b": "../../../settings/provider.e2b.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			read := shippedSection(t, path)
			if read.Provider.Name != name {
				t.Fatalf("provider.name = %q, want %q", read.Provider.Name, name)
			}
			if _, err := New(read.Provider.Name, read.Provider.Settings, nil); err != nil {
				t.Fatalf("provider: %v", err)
			}
			_, err := New(read.Provider.Name, read.Provider.Settings, []string{"nvidia.com/gpu.product=NVIDIA-H100"})
			if err == nil || !strings.Contains(err.Error(), "NVIDIA-H100") {
				t.Errorf("err = %v, want a refusal of an accelerator: neither vendor is rented for one", err)
			}
		})
	}
}

// A config that names a vendor and leaves out its settings must stop the process at startup,
// with the setting that is missing, before anything is rented.
func TestAVendorNamedWithoutASettingsSectionIsRefusedAtStartup(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "FlyV1 not-a-real-token")
	t.Setenv("E2B_API_KEY", "not-a-real-key")
	for name, want := range map[string]string{
		"fly": "fly: api_token_env is required",
		"e2b": "e2b: api_key_env is required",
	} {
		for form, text := range map[string]string{"no settings key": "name: " + name, "an empty settings key": "name: " + name + "\nsettings:"} {
			t.Run(name+", "+form, func(t *testing.T) {
				if _, err := New(name, settingsNode(t, text), nil); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want %q", err, want)
				}
			})
		}
	}
}
