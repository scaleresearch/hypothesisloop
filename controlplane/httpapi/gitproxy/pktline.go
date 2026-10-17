package gitproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// maxPktLine is the longest pkt-line git writes, its four length digits included.
const maxPktLine = 65520

// maxCommands bounds the ref updates one push may name, so reading them holds a bounded amount.
const maxCommands = 10000

// command is one ref update of a git-receive-pack request: the value the client saw and the one
// it asks for, each all zeros for a ref that has none.
type command struct {
	Old, New, Ref string
}

func zeroID(id string) bool { return strings.Trim(id, "0") == "" }

func (c command) creates() bool { return zeroID(c.Old) }
func (c command) deletes() bool { return zeroID(c.New) }

// readPktLine reads one pkt-line from r into raw and returns its payload, nil with flush set for
// a flush packet. Any other special packet is an error: none belongs before a push's pack.
func readPktLine(r io.Reader, raw *bytes.Buffer) (payload []byte, flush bool, err error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, false, fmt.Errorf("reading a pkt-line length: %w", err)
	}
	n, err := strconv.ParseUint(string(length[:]), 16, 16)
	if err != nil {
		return nil, false, fmt.Errorf("pkt-line length %q is not four hex digits", length)
	}
	raw.Write(length[:])
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 || n > maxPktLine {
		return nil, false, fmt.Errorf("pkt-line length %d is out of range", n)
	}
	payload = make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, fmt.Errorf("reading a pkt-line: %w", err)
	}
	raw.Write(payload)
	return payload, false, nil
}

// readCommands reads the ref updates a git-receive-pack request opens with, up to the flush that
// ends them, and returns them with the capabilities the client asked for and every byte read, which
// go to the host ahead of the rest of the request. The shallow lines a push from a shallow clone
// sends first are passed over. A signed push is refused: nothing here checks its certificate.
func readCommands(r io.Reader) (commands []command, capabilities []string, raw []byte, err error) {
	var buf bytes.Buffer
	for {
		payload, flush, err := readPktLine(r, &buf)
		if err != nil {
			return nil, nil, nil, err
		}
		if flush {
			return commands, capabilities, buf.Bytes(), nil
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if strings.HasPrefix(line, "shallow ") && len(commands) == 0 {
			continue
		}
		if strings.HasPrefix(line, "push-cert") {
			return nil, nil, nil, errors.New("a signed push is not taken")
		}
		if len(commands) == 0 {
			var caps string
			line, caps, _ = strings.Cut(line, "\x00")
			capabilities = strings.Fields(caps)
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 || !objectID(parts[0]) || len(parts[1]) != len(parts[0]) || !objectID(parts[1]) || parts[2] == "" {
			return nil, nil, nil, fmt.Errorf("%q is no ref update", line)
		}
		if len(commands) == maxCommands {
			return nil, nil, nil, fmt.Errorf("a push names more than %d refs", maxCommands)
		}
		commands = append(commands, command{Old: parts[0], New: parts[1], Ref: parts[2]})
	}
}

// objectID is whether id is a SHA-1 or SHA-256 object name in hex.
func objectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}

func pktLine(payload string) []byte {
	return fmt.Appendf(nil, "%04x%s", len(payload)+4, payload)
}

// refusalReport is git-receive-pack's answer to a push none of whose updates is made: each ref
// with why, which git prints as "! [remote rejected] REF (WHY)", and message, which git prints as
// lines starting "remote:". Over side-band the report is the first band and message the second.
func refusalReport(commands []command, why func(command) string, capabilities []string, message string) []byte {
	var report bytes.Buffer
	report.Write(pktLine("unpack ok\n"))
	for _, c := range commands {
		report.Write(pktLine("ng " + c.Ref + " " + strings.ReplaceAll(why(c), "\n", " ") + "\n"))
	}
	report.WriteString("0000")
	if !slices.Contains(capabilities, "side-band-64k") && !slices.Contains(capabilities, "side-band") {
		return report.Bytes()
	}
	limit := 999 - 5
	if slices.Contains(capabilities, "side-band-64k") {
		limit = maxPktLine - 5
	}
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(message, "\n") {
		if line != "" {
			out.Write(pktLine("\x02" + line))
		}
	}
	for data := report.Bytes(); len(data) > 0; {
		n := min(limit, len(data))
		out.Write(pktLine("\x01" + string(data[:n])))
		data = data[n:]
	}
	out.WriteString("0000")
	return out.Bytes()
}
