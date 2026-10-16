package providertest

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
)

const (
	envValue = `two  spaces = 'single' "double" $HOME`

	// The trap ends the command at once, so a vendor that grants any time after SIGTERM passes.
	lifecycleScript = `trap 'echo conformance-terminated; exit 0' TERM
printf 'conformance-env=%s\n' "$HL_CONFORMANCE_VALUE"
for a in "$@"; do printf 'conformance-arg=%s\n' "$a"; done
echo conformance-stderr >&2
if wget -q -O /dev/null "$HL_CONFORMANCE_URL"; then echo conformance-outbound-ok; else echo conformance-outbound-failed; fi
echo conformance-ready
while :; do sleep 1 & wait $!; done`
)

var lifecycleArgs = []string{"two  spaces", `quotes ' " and $HOME \ kept`, "--flag=a b", ""}

// lifecycle follows one instance from Create to gone: listed under its name, the longest a caller
// may ask for, and only under its prefix, env and argv delivered byte for byte, an outbound HTTPS request, output readable while
// running and after Stop, and Destroy that can be repeated.
func lifecycle(t *testing.T, r *run) {
	ctx := r.ctx
	spec := r.spec("lifecycle-", lifecycleScript, lifecycleArgs...)
	spec.Name += strings.Repeat("x", provider.MaxNameLength-len(spec.Name))
	spec.Env = map[string]string{"HL_CONFORMANCE_VALUE": envValue, "HL_CONFORMANCE_URL": r.opts.OutboundURL}
	id, asked := r.create(t, ctx, spec)

	listed, ok := r.find(t, ctx, id)
	if !ok {
		t.Fatalf("instance %s is not in List(%q) when Create returns", id, r.prefix)
	}
	if listed.Name != spec.Name {
		t.Errorf("List names the instance %q, want %q", listed.Name, spec.Name)
	}
	if listed.State != provider.StateStarting && listed.State != provider.StateRunning {
		t.Errorf("a fresh instance is %q", listed.State)
	}
	foreign, err := r.p.List(ctx, suitePrefix+"another-prefix-")
	if err != nil {
		t.Fatalf("List of another prefix: %v", err)
	}
	if slices.ContainsFunc(foreign, func(i provider.Instance) bool { return i.ID == id }) {
		t.Errorf("instance %s (%s) is listed under a prefix it does not have", id, spec.Name)
	}

	lines := r.awaitLine(t, ctx, id, "conformance-ready", r.opts.StartTimeout)
	t.Logf("command output readable %s after Create returned", since(asked))
	running := r.awaitState(t, ctx, id, provider.StateRunning, r.opts.SettleTimeout)
	if running.StartedAt.IsZero() || running.StartedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("StartedAt = %s", running.StartedAt)
	}
	want := []string{"conformance-env=" + envValue}
	for _, arg := range lifecycleArgs {
		want = append(want, "conformance-arg="+arg)
	}
	want = append(want, "conformance-stderr", "conformance-outbound-ok", "conformance-ready")
	for _, line := range want {
		if count(lines, line) != 1 {
			t.Errorf("output has %d lines %q, want 1; output: %q", count(lines, line), line, lines)
		}
	}

	stopped := time.Now()
	if err := r.p.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	exited := r.awaitState(t, ctx, id, provider.StateExited, r.opts.SettleTimeout)
	t.Logf("exited %s after Stop", since(stopped))
	if exited.Name != spec.Name {
		t.Errorf("exited instance is named %q, want %q", exited.Name, spec.Name)
	}
	final := r.logs(t, ctx, id, logTail)
	if count(final, "conformance-terminated") != 1 {
		t.Errorf("Stop did not reach the command as SIGTERM; output of the exited instance: %q", final)
	}
	if count(final, "conformance-ready") != 1 {
		t.Errorf("output written before the command ended is lost or repeated: %q", final)
	}
	if len(final) >= 2 {
		if last := r.logs(t, ctx, id, 2); !slices.Equal(last, final[len(final)-2:]) {
			t.Errorf("Logs(tail 2) = %q, want the last two lines of %q", last, final)
		}
	}
	if err := r.p.Stop(ctx, id); err != nil {
		t.Errorf("Stop of an exited instance: %v", err)
	}

	if err := r.p.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	destroyed := time.Now()
	for {
		if _, still := r.find(t, ctx, id); !still {
			break
		}
		if time.Since(destroyed) > r.opts.SettleTimeout {
			t.Fatalf("instance %s is still in List %s after Destroy", id, r.opts.SettleTimeout)
		}
		pause(t, ctx)
	}
	t.Logf("gone from List %s after Destroy", since(destroyed))
	if err := r.p.Destroy(ctx, id); err != nil {
		t.Errorf("Destroy of a gone instance: %v", err)
	}
	if err := r.p.Stop(ctx, id); err != nil {
		t.Errorf("Stop of a gone instance: %v", err)
	}
}
