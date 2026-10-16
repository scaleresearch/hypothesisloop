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

	// logPage is the most entries the log API answers one read with, whatever limit is asked
	// for. It takes no cursor that reads backwards.
	logPage = 500
)

type logAnswer struct {
	Data []struct {
		Attributes struct {
			Message string `json:"message"`
		} `json:"attributes"`
	} `json:"data"`
}

// Logs reads the Machine's last log entries, as many as one read gives, and keeps the command's
// of the Machine's newest start. The vendor's own lines sit among them, so a full read that
// holds fewer lines of the command than asked for, and not the start of its run, is an error:
// older ones exist and cannot be reached.
func (p *Provider) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	if tail <= 0 {
		return nil, fmt.Errorf("fly: logs of machine %s: tail must be positive, got %d", id, tail)
	}
	entries, err := p.logEntries(ctx, id, logPage)
	if err != nil {
		return nil, err
	}
	run, whole := lastRun(entries)
	lines := commandLines(run)
	if len(lines) >= tail {
		return lines[len(lines)-tail:], nil
	}
	if len(entries) >= logPage && !whole {
		return nil, fmt.Errorf("fly: logs of machine %s: the %d entries one read of the vendor's log gives hold %d lines of the command, and %d were asked for", id, logPage, len(lines), tail)
	}
	return lines, nil
}

// logEntries returns the last limit entries of the Machine's log, oldest first. The log of a
// Machine that never wrote, or that the app never had, is empty.
func (p *Provider) logEntries(ctx context.Context, id string, limit int) ([]string, error) {
	query := url.Values{"instance": {id}, "limit": {strconv.Itoa(limit)}}
	var page logAnswer
	if _, err := p.call(ctx, http.MethodGet, p.logsURL+"/api/v1/apps/"+p.cfg.App+"/logs?"+query.Encode(), nil, &page); err != nil {
		return nil, err
	}
	entries := make([]string, len(page.Data))
	for i, entry := range page.Data {
		entries[i] = entry.Attributes.Message
	}
	return entries, nil
}

// lastRun is what the Machine's newest start wrote: the entries after the supervisor's newest
// start mark. whole is false when entries hold no start mark, and they are then all returned:
// the run began before the first of them.
func lastRun(entries []string) (run []string, whole bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] == startMark {
			return entries[i+1:], true
		}
	}
	return entries, false
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
