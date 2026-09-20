package fly

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// logSettle is how long after a Machine stopped its log is taken as complete without the
	// supervisor's end mark. The log trailed a Machine by under five seconds on every read
	// measured.
	logSettle = 2 * time.Minute

	// endMarkWindow is how many of a stopped Machine's last log entries are searched for the end
	// mark. After the mark come only the lines of the vendor's own shutdown, six on the Machines
	// looked at.
	endMarkWindow = 50

	// foreignAllowance is how many entries beyond the asked tail the first read of a log takes,
	// for the lines of the vendor's init, ssh server and kernel that sit between the command's.
	foreignAllowance = 50
)

type logPage struct {
	Data []struct {
		Attributes struct {
			Message string `json:"message"`
		} `json:"attributes"`
	} `json:"data"`
}

// Logs reads the Machine's last log entries and keeps the command's. The API answers with the
// last entries of the Machine, the vendor's own among them, so a read that came back full
// without holding tail lines of the command is taken again, twice as large.
func (p *Provider) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	if tail <= 0 {
		return nil, fmt.Errorf("fly: logs of machine %s: tail must be positive, got %d", id, tail)
	}
	for limit := tail + foreignAllowance; ; limit *= 2 {
		entries, err := p.logEntries(ctx, id, limit)
		if err != nil {
			return nil, err
		}
		lines := commandLines(entries)
		if len(lines) >= tail {
			return lines[len(lines)-tail:], nil
		}
		if len(entries) < limit {
			return lines, nil
		}
	}
}

// logEntries returns the last limit entries of the Machine's log, oldest first. The log of a
// Machine that never wrote, or that the app never had, is empty.
func (p *Provider) logEntries(ctx context.Context, id string, limit int) ([]string, error) {
	query := url.Values{"instance": {id}, "limit": {strconv.Itoa(limit)}}
	var page logPage
	if _, err := p.call(ctx, http.MethodGet, p.logsURL+"/api/v1/apps/"+p.cfg.App+"/logs?"+query.Encode(), nil, &page); err != nil {
		return nil, err
	}
	entries := make([]string, len(page.Data))
	for i, entry := range page.Data {
		entries[i] = entry.Attributes.Message
	}
	return entries, nil
}

// commandLines keeps the lines the supervisor passed on from the command.
func commandLines(entries []string) []string {
	lines := []string{}
	for _, entry := range entries {
		if line, fromCommand := strings.CutPrefix(entry, outputPrefix); fromCommand {
			lines = append(lines, line)
		}
	}
	return lines
}
