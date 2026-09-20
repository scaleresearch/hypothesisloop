// Package configfile reads the one YAML file a cloud process is configured by.
package configfile

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads the file at path into a config struct, refusing a key the struct does not have.
func Load(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := Decode(data, into); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// Decode refuses a key the target does not have: a misspelled setting is an error at startup
// instead of a default nobody chose.
func Decode(data []byte, into any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(into)
}
