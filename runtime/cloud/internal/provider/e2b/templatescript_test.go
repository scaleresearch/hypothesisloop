package e2b

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCurl stands in for the registry and for E2B: it answers by the URL, which is curl's last
// argument, and keeps what was sent to E2B.
const fakeCurl = `#!/bin/sh
for url; do :; done
case "$url" in
  */v2/) ;;
  */manifests/*) echo '{"config":{"digest":"sha256:config"}}' ;;
  */blobs/*) cat "$IMAGE_CONFIG" ;;
  *api.e2b.app*) cat >> "$E2B_SENT"; echo >> "$E2B_SENT"; echo '{"templateID":"tpl","buildID":"bld","status":"ready","logEntries":[]}' ;;
esac
`

// runTemplateScript runs the build script for an image whose config has the given ENV, and
// returns what it wrote and what it sent to E2B.
func runTemplateScript(t *testing.T, imageEnv ...string) (output string, sent string, err error) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("this host has no %s", tool)
		}
	}
	dir := t.TempDir()
	config, _ := json.Marshal(map[string]any{"config": map[string]any{"Env": imageEnv}})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../../../../../"+templateScript, "registry.example/agent:1", "2", "4096")
	cmd.Env = []string{
		"PATH=" + dir + ":" + os.Getenv("PATH"), "E2B_API_KEY=not-a-real-key",
		"IMAGE_CONFIG=" + filepath.Join(dir, "config.json"), "E2B_SENT=" + filepath.Join(dir, "sent"),
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	raw, _ := os.ReadFile(filepath.Join(dir, "sent"))
	return out.String(), string(raw), err
}

func TestTemplateScriptWritesTheImagesEnvironmentQuotedIntoTheTemplate(t *testing.T) {
	output, sent, err := runTemplateScript(t, "PATH=/usr/local/bin:/usr/bin", `NOTE=it's "quoted" $HOME`, "EMPTY=")
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, output)
	}
	if !strings.Contains(sent, `"name": "`+agentTemplate+`"`) {
		t.Errorf("sent %s, want a template named %s, the name the adapter asks for", sent, agentTemplate)
	}
	want := "export PATH='/usr/local/bin:/usr/bin'\n" + `export NOTE='it'\''s "quoted" $HOME'` + "\nexport EMPTY=''\n"
	if encoded := base64.StdEncoding.EncodeToString([]byte(want)); !strings.Contains(sent, "echo "+encoded+" | base64 -d > "+imageEnvPath) {
		t.Errorf("sent %s, want a step that writes %q to %s", sent, want, imageEnvPath)
	}
}

// The file the names go into is read by the shell of every sandbox of the template, as root's
// file, before the command starts.
func TestTemplateScriptRefusesAnImageWhoseEnvHoldsWhatIsNotAName(t *testing.T) {
	for name, entry := range map[string]string{
		"a command":       "A;touch /tmp/owned;B=1",
		"a substitution":  "A$(id)=1",
		"a space":         "A B=1",
		"a newline":       "A\nid\nB=1",
		"no name":         "=1",
		"a leading digit": "1A=1",
	} {
		t.Run(name, func(t *testing.T) {
			output, sent, err := runTemplateScript(t, "PATH=/usr/bin", entry)
			if err == nil || !strings.Contains(output, "is not an environment variable name") {
				t.Fatalf("the script ended with %v and wrote %q, want a refusal of the name", err, output)
			}
			if sent != "" {
				t.Errorf("a template was asked for all the same: %s", sent)
			}
		})
	}
}
