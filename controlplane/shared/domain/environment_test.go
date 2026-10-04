package domain

import "testing"

func TestAnEnvironmentFileIsBuiltOnTheBaseThePlatformPasses(t *testing.T) {
	for file, valid := range map[string]bool{
		"ARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n":        true,
		"# syntax=docker/dockerfile:1\n\nARG BASE\nfrom $BASE AS env\n": true,
		"FROM ${BASE}\n": false,
		"ARG BASE=registry.test/old-job:1\nFROM ${BASE}\n":      false,
		"ARG BASE\nFROM registry.test/hypothesisloop-job:old\n": false,
		"ARG BASE\nARG OTHER\nFROM ${BASE}\n":                   false,
		"ARG BASE\nFROM ${BASE} AS\n":                           false,
		"":                                                      false,
		"ARG BASE\nFROM ${BASE}\n" + string(make([]byte, MaxEnvironmentFileBytes)): false,
	} {
		if err := ValidateEnvironmentFile(file); (err == nil) != valid {
			t.Errorf("%.60q: got = %v, want valid %v", file, err, valid)
		}
	}
}

func TestAnEnvironmentKeyNamesTheOrgTheFileAndTheBaseApart(t *testing.T) {
	if EnvironmentKey("o", "ab", "c") == EnvironmentKey("o", "a", "bc") || EnvironmentKey("o", "x", "base:1") == EnvironmentKey("o", "x", "base:2") ||
		EnvironmentKey("o1", "x", "base:1") == EnvironmentKey("o", "1x", "base:1") || EnvironmentKey("a", "x", "b") == EnvironmentKey("b", "x", "b") ||
		EnvironmentKey("o", "x", "base:1") != EnvironmentKey("o", "x", "base:1") {
		t.Error("a key must change with the org, the file and the base, and only with them")
	}
}
