package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateSecretEnv(t *testing.T) {
	spec := JobSpec{
		Env:    map[string]string{"LR": "0.1"},
		Groups: []JobGroup{{Name: "learner", Env: map[string]string{"ROLE": "learner"}}},
	}
	tooMany := map[string]string{}
	for i := 0; i <= MaxSecretEnvEntries; i++ {
		tooMany[fmt.Sprintf("S_%d", i)] = "v"
	}
	cases := []struct {
		name   string
		secret map[string]string
		wants  string
	}{
		{"none", nil, ""},
		{"a well-formed one", map[string]string{"HF_TOKEN": "x", "_private1": "y"}, ""},
		{"also in env", map[string]string{"LR": "x"}, `"LR" is in both job.env and job.secret_env`},
		{"also in a group's env", map[string]string{"ROLE": "x"}, `"ROLE" is in both job.groups["learner"].env and job.secret_env`},
		{"a name no shell would take", map[string]string{"HF-TOKEN": "x"}, `"HF-TOKEN" is not an environment variable name`},
		{"a name starting with a digit", map[string]string{"1TOKEN": "x"}, `"1TOKEN" is not an environment variable name`},
		{"an empty name", map[string]string{"": "x"}, `"" is not an environment variable name`},
		{"an empty value", map[string]string{"HF_TOKEN": ""}, `job.secret_env["HF_TOKEN"] is empty`},
		{"too many", tooMany, "more than the 64 allowed"},
		{"too large", map[string]string{"BIG": strings.Repeat("x", MaxSecretEnvBytes)}, "more than the 65536 allowed"},
	}
	for _, c := range cases {
		err := spec.ValidateSecretEnv(c.secret)
		switch {
		case c.wants == "" && err != nil:
			t.Errorf("%s: got = %v, want accepted", c.name, err)
		case c.wants != "" && (err == nil || !strings.Contains(err.Error(), c.wants)):
			t.Errorf("%s: got = %v, want an error naming %q", c.name, err, c.wants)
		case err != nil && strings.Contains(err.Error(), "xxxx"):
			t.Errorf("%s: the error repeats a secret value: %v", c.name, err)
		}
	}
}
