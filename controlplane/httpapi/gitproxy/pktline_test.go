package gitproxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const (
	oldID = "1111111111111111111111111111111111111111"
	newID = "2222222222222222222222222222222222222222"
	zero  = "0000000000000000000000000000000000000000"
)

func pkts(lines ...string) string {
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			b.WriteString("0000")
			continue
		}
		b.Write(pktLine(line))
	}
	return b.String()
}

func TestTheRefUpdatesOfAPushAreReadUpToTheirFlushAndNoFurther(t *testing.T) {
	pack := "PACK-and-everything-after-it"
	request := pkts(
		"shallow "+oldID,
		oldID+" "+newID+" refs/heads/hl/pe-1/executor-2/x\x00report-status side-band-64k agent=git/2.54",
		zero+" "+newID+" refs/tags/v1\n",
		newID+" "+zero+" refs/heads/old",
		"",
	) + pack
	r := strings.NewReader(request)
	commands, capabilities, raw, err := readCommands(r)
	if err != nil {
		t.Fatal(err)
	}
	want := []command{
		{Old: oldID, New: newID, Ref: "refs/heads/hl/pe-1/executor-2/x"},
		{Old: zero, New: newID, Ref: "refs/tags/v1"},
		{Old: newID, New: zero, Ref: "refs/heads/old"},
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %+v, want %+v", commands, want)
	}
	for i := range want {
		if commands[i] != want[i] {
			t.Errorf("command %d = %+v, want %+v", i, commands[i], want[i])
		}
	}
	if !commands[1].creates() || commands[1].deletes() || !commands[2].deletes() || commands[0].creates() {
		t.Errorf("creates and deletes are not told apart: %+v", commands)
	}
	if strings.Join(capabilities, " ") != "report-status side-band-64k agent=git/2.54" {
		t.Errorf("capabilities = %q", capabilities)
	}
	if string(raw) != strings.TrimSuffix(request, pack) {
		t.Errorf("raw = %q, want every byte up to the flush", raw)
	}
	if rest, _ := io.ReadAll(r); string(rest) != pack {
		t.Errorf("left unread = %q, want the pack", rest)
	}
}

func TestASHA256PushIsRead(t *testing.T) {
	long := strings.Repeat("a", 64)
	commands, _, _, err := readCommands(strings.NewReader(pkts(long+" "+strings.Repeat("0", 64)+" refs/heads/x\x00report-status", "")))
	if err != nil || len(commands) != 1 || !commands[0].deletes() {
		t.Fatalf("commands = %+v, %v, want the one delete", commands, err)
	}
}

func TestAPushThatIsNoListOfRefUpdatesIsRefused(t *testing.T) {
	for name, request := range map[string]string{
		"a signed push":                pkts("push-cert\x00report-status", "certificate version 0.1", ""),
		"a short object name":          pkts("1111 "+newID+" refs/heads/x", ""),
		"object names of mixed length": pkts(oldID+" "+strings.Repeat("2", 64)+" refs/heads/x", ""),
		"an object name in capitals":   pkts(strings.ToUpper("abcdef"+oldID[6:])+" "+newID+" refs/heads/x", ""),
		"no ref":                       pkts(oldID+" "+newID, ""),
		"a ref with a space":           pkts(oldID+" "+newID+" refs/heads/a b", ""),
		"no flush":                     pkts(oldID + " " + newID + " refs/heads/x"),
		"a length that is not hex":     "zzzz",
		"a length out of range":        "0003",
		"a delimiter packet":           "0001",
		"a cut packet":                 "0040" + oldID,
	} {
		if commands, _, _, err := readCommands(strings.NewReader(request)); err == nil {
			t.Errorf("%s: read as %+v, want it refused", name, commands)
		}
	}
}

func readReport(t *testing.T, data []byte, sideband bool) (report, message string) {
	t.Helper()
	r := bytes.NewReader(data)
	var reportBuf, messageBuf bytes.Buffer
	for {
		var raw bytes.Buffer
		payload, flush, err := readPktLine(r, &raw)
		if err != nil {
			t.Fatalf("the report is no pkt-line stream: %v", err)
		}
		if flush && (!sideband || r.Len() == 0) {
			if !sideband {
				reportBuf.WriteString("0000")
			}
			break
		}
		switch {
		case !sideband:
			reportBuf.Write(raw.Bytes())
		case payload[0] == 1:
			reportBuf.Write(payload[1:])
		case payload[0] == 2:
			messageBuf.Write(payload[1:])
		}
	}
	if r.Len() != 0 {
		t.Errorf("%d bytes after the report", r.Len())
	}
	return reportBuf.String(), messageBuf.String()
}

func TestARefusedPushIsReportedRefByRefAsGitReadsIt(t *testing.T) {
	commands := []command{{Old: oldID, New: newID, Ref: "refs/heads/main"}, {Old: zero, New: newID, Ref: "refs/heads/hl/pe-1/executor-2/x"}}
	why := func(c command) string {
		if c.Ref == "refs/heads/main" {
			return "the default branch main is no agent's\nsecond line"
		}
		return "pushed with a ref that is refused"
	}
	want := pkts("unpack ok\n", "ng refs/heads/main the default branch main is no agent's second line\n", "ng refs/heads/hl/pe-1/executor-2/x pushed with a ref that is refused\n", "")
	if report, _ := readReport(t, refusalReport(commands, why, []string{"report-status"}, "refused\n"), false); report != want {
		t.Errorf("without side-band: %q, want %q", report, want)
	}
	for _, band := range []string{"side-band-64k", "side-band"} {
		report, message := readReport(t, refusalReport(commands, why, []string{"report-status", band}, "refused\n"), true)
		if report != want || message != "refused\n" {
			t.Errorf("over %s: report %q and message %q, want %q and the message", band, report, message, want)
		}
	}
}

// endless repeats line without end.
type endless struct {
	line []byte
	read int
}

func (e *endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], e.line[e.read%len(e.line):])
		n += c
		e.read += c
	}
	return n, nil
}

func TestAPushOpensWithABoundedAmountBeforeItsPack(t *testing.T) {
	for name, line := range map[string]string{
		"shallow lines": "shallow " + oldID,
		"ref updates":   oldID + " " + newID + " refs/heads/hl/pe-1/executor-2/" + strings.Repeat("x", 60000),
	} {
		r := &endless{line: pktLine(line)}
		if _, _, _, err := readCommands(r); err == nil || r.read > 2*maxCommandBytes {
			t.Errorf("%s: err = %v after %d bytes, want refused within %d", name, err, r.read, 2*maxCommandBytes)
		}
	}
}
