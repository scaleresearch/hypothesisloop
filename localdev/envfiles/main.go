package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

// dir is where the files are written, each <program>.env, read by localdev/lib/env.sh's lib_run
// and by the compose stack.
const dir = "localdev/.run/env"

// made is what local development makes on this machine and never reads from .env: the address
// of each compose datastore as a process on this machine reaches it, and the files the first
// render writes.
var made = map[config.Key]func() (string, error){
	config.DatabaseURL:          constant("postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"),
	config.MetricsDBURL:         constant("http://localhost:4010"),
	config.GraphDBURL:           constant("bolt://localhost:7687"),
	config.AuthSigningSecret:    file("localdev/.run/signing-secret"),
	config.GitHostPlainPassword: file("localdev/.run/git-password"),
}

func main() {
	if len(os.Args) != 2 {
		fail("usage: envfiles <settings>")
	}
	settings, err := config.LoadSettings(os.Args[1])
	if err != nil {
		fail(err.Error())
	}
	files := map[string][][2]string{}
	for _, p := range config.Programs {
		for _, key := range settings.Needs(p) {
			value, err := valueOf(key)
			if err != nil {
				fail(err.Error())
			}
			files[string(p)] = append(files[string(p)], [2]string{key.Name, value})
		}
	}
	files["ui"] = uiKeys()
	// The compose stack's store, whose root password is the data store's secret key.
	files["object-store"] = [][2]string{{"MINIO_ROOT_PASSWORD", os.Getenv(config.DataStoreMinIOSecretAccessKey.Name)}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(err.Error())
	}
	for name, keys := range files {
		if err := write(filepath.Join(dir, name+".env"), keys); err != nil {
			fail(err.Error())
		}
	}
}

// valueOf is the value of key, empty when .env leaves it out: the program then refuses to start,
// naming it.
func valueOf(key config.Key) (string, error) {
	from, isMade := made[key]
	if !isMade {
		return os.Getenv(key.Name), nil
	}
	if os.Getenv(key.Name) != "" {
		return "", fmt.Errorf("envfiles: .env sets %s, which local development makes itself", key.Name)
	}
	return from()
}

// uiKeys is what the UI is built with, from the UI_ and IDENTITY_ keys of .env.
func uiKeys() [][2]string {
	apiURL := os.Getenv("UI_API_URL")
	if apiURL == "" {
		fail("envfiles: .env does not set UI_API_URL, the API's address as the UI's browser reaches it")
	}
	keys := [][2]string{{"NEXT_PUBLIC_API_URL", apiURL}, {"NEXT_PUBLIC_IDENTITY_PROVIDER", os.Getenv("IDENTITY_PROVIDER")}}
	if project := os.Getenv("IDENTITY_SUPABASE_PROJECT_REF"); project != "" {
		keys = append(keys, [2]string{"NEXT_PUBLIC_IDENTITY_SUPABASE_URL", "https://" + project + ".supabase.co"})
	}
	return append(keys,
		[2]string{"NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY", os.Getenv("IDENTITY_SUPABASE_PUBLISHABLE_KEY")},
		[2]string{"NEXT_PUBLIC_SIGN_IN_PROVIDERS", os.Getenv("IDENTITY_SIGN_IN_PROVIDERS")})
}

// write replaces path with keys as a shell reads them, KEY='value', each value whole over as many
// lines as it spans, and readable by its owner alone. A key without a value is left out.
func write(path string, keys [][2]string) error {
	var text strings.Builder
	for _, key := range keys {
		name, value := key[0], key[1]
		if value == "" {
			continue
		}
		if strings.Contains(value, "'") {
			return fmt.Errorf("envfiles: the value of %s holds a single quote, which %s cannot quote", name, path)
		}
		fmt.Fprintf(&text, "%s='%s'\n", name, value)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".env-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.WriteString(text.String()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

func constant(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

func file(path string) func() (string, error) {
	return func() (string, error) {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("envfiles: %w (make render-settings makes it)", err)
		}
		return strings.TrimSpace(string(content)), nil
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
