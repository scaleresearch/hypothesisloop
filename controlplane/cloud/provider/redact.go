package provider

import (
	"sort"
	"strings"
)

// minSecretLength keeps short values such as an accelerator type readable in an error: nothing that short
// is a secret.
const minSecretLength = 8

// redacted is an error whose text has a spec's secrets taken out. It still unwraps to the
// vendor's error, so errors.Is finds a sentinel such as ErrNoOffer.
type redacted struct {
	text string
	err  error
}

func (r *redacted) Error() string { return r.text }
func (r *redacted) Unwrap() error { return r.err }

// withoutSecrets is err with every environment value and the registry password of spec taken out
// of its text. A vendor that refuses a call may quote the request back, and the request holds
// the agent's token and the registry login.
func withoutSecrets(err error, spec Spec) error {
	if err == nil {
		return nil
	}
	secrets := map[string]string{}
	for name, value := range spec.Env {
		secrets[name] = value
	}
	if spec.ImageLogin != nil {
		secrets["registry password"] = spec.ImageLogin.Password
	}
	names := make([]string, 0, len(secrets))
	for name, value := range secrets {
		if len(value) >= minSecretLength {
			names = append(names, name)
		}
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(names, func(a, b int) bool {
		if len(secrets[names[a]]) != len(secrets[names[b]]) {
			return len(secrets[names[a]]) > len(secrets[names[b]])
		}
		return names[a] < names[b]
	})
	text := err.Error()
	for _, name := range names {
		text = strings.ReplaceAll(text, secrets[name], "<"+name+">")
	}
	if text == err.Error() {
		return err
	}
	return &redacted{text: text, err: err}
}
