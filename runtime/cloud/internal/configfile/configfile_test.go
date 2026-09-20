package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type example struct {
	Name   string `yaml:"name"`
	Nested struct {
		Count int `yaml:"count"`
	} `yaml:"nested"`
}

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadReadsAFileIntoItsStruct(t *testing.T) {
	var cfg example
	if err := Load(write(t, "name: a\nnested:\n  count: 2\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "a" || cfg.Nested.Count != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadRefusesWhatItCannotPlace(t *testing.T) {
	for name, text := range map[string]string{
		"unknown key":        "name: a\nnmae: b\n",
		"unknown nested key": "nested:\n  cuont: 2\n",
		"wrong type":         "nested:\n  count: many\n",
		"not yaml":           "name: [unclosed\n",
	} {
		var cfg example
		err := Load(write(t, text), &cfg)
		if err == nil || !strings.Contains(err.Error(), "config: parse") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var cfg example
	if err := Load(filepath.Join(t.TempDir(), "missing.yaml"), &cfg); err == nil || !strings.Contains(err.Error(), "config: read") {
		t.Errorf("missing file: err = %v", err)
	}
}
