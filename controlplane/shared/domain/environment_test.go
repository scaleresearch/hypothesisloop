package domain

import (
	"regexp"
	"strings"
	"testing"
)

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
		"ARG BASE\nFROM ${BASE}\n" + string(make([]byte, MaxEnvironmentFileBytes)):   false,
		"ARG BASE\nFROM ${BASE}\nCOPY vendor/codec /opt/codec\n":                     false,
		"ARG BASE\nFROM ${BASE}\nRUN true\nadd --chown=1:1 \\\n  setup.sh /opt/\n":   false,
		"ARG BASE\nFROM ${BASE}\nCOPY [\"requirements.txt\", \"/tmp/\"]\n":           false,
		"ARG BASE\nFROM ${BASE}\nCOPY --from=ghcr.io/astral-sh/uv:0.4 /uv /bin/uv\n": true,
		"ARG BASE\nFROM ${BASE}\nADD https://example.test/tool.tgz /opt/\n":          true,
		"ARG BASE\nFROM ${BASE}\nCOPY <<EOF /etc/tool.conf\nCOPY x y\nEOF\n":         true,
		"ARG BASE\nFROM ${BASE}\nRUN <<EOF\necho COPY a b\nEOF\n":                    true,
	} {
		if err := ValidateEnvironmentFile(file); (err == nil) != valid {
			t.Errorf("%.60q: got = %v, want valid %v", file, err, valid)
		}
	}
}

func TestAFileThatCopiesFromABuildContextIsToldItIsBuiltAlone(t *testing.T) {
	err := ValidateEnvironmentFile("ARG BASE\nFROM ${BASE}\nRUN true\nCOPY vendor/codec /opt/codec\n")
	if err == nil || !strings.Contains(err.Error(), "line 4") || !strings.Contains(err.Error(), "vendor/codec") ||
		!strings.Contains(err.Error(), "built from this file alone") || !strings.Contains(err.Error(), "clone") {
		t.Errorf("got %v, want the line, the source, and that the file is built alone and the code comes by the clone", err)
	}
}

func TestAnEnvironmentKeyNamesTheOrgTheRepositoryTheFileAndTheBaseApart(t *testing.T) {
	if EnvironmentKey("o", "r1", "x", "b", "") == EnvironmentKey("o", "r2", "x", "b", "") || EnvironmentKey("o", "ra", "bx", "b", "") == EnvironmentKey("o", "rab", "x", "b", "") ||
		EnvironmentKey("o", "r", "ab", "c", "") == EnvironmentKey("o", "r", "a", "bc", "") || EnvironmentKey("o", "r", "x", "base:1", "") == EnvironmentKey("o", "r", "x", "base:2", "") ||
		EnvironmentKey("o1", "r", "x", "base:1", "") == EnvironmentKey("o", "r", "1x", "base:1", "") || EnvironmentKey("a", "r", "x", "b", "") == EnvironmentKey("b", "r", "x", "b", "") ||
		EnvironmentKey("o", "r", "x", "base:1", "t") == EnvironmentKey("o", "r", "x", "base:1", "") || EnvironmentKey("o", "r", "x", "base:1", "t:1") == EnvironmentKey("o", "r", "x", "base:1", "t:2") ||
		EnvironmentKey("o", "r", "x", "base:1", "t") != EnvironmentKey("o", "r", "x", "base:1", "t") {
		t.Error("a key must change with the org, the repository, the file and the base, and only with them")
	}
}

func TestACacheScopeIsAHostNameOfOneRepositoryOfOneOrg(t *testing.T) {
	scope := EnvironmentCacheScope("o", "acme/model")
	if scope != EnvironmentCacheScope("o", "acme/model") || scope == EnvironmentCacheScope("o", "acme/other") ||
		scope == EnvironmentCacheScope("p", "acme/model") || EnvironmentCacheScope("o1", "r") == EnvironmentCacheScope("o", "1r") {
		t.Error("a cache scope must change with the org and the repository, and only with them")
	}
	if !regexp.MustCompile(`^[a-z0-9-]{1,63}$`).MatchString(scope) {
		t.Errorf("cache scope %q is not a host name", scope)
	}
}
