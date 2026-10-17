package shared

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func Watch(client *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	apiURL, token := client.BaseURL, client.Token
	fs := command.NewFlags("watch", `usage: hl watch [--agent A | --all [--decisions]] [--platform-experiment PE] [--kinds K,...] [--follow | --no-wait | --wake K,...]
   or: hl watch --experiment JOB [--until 'status in COMPLETED,FAILED,EVICTED']

Prints changes, one JSON object per line, each with its id. With an agent's token or --agent and
no --experiment or --all, the agent's own resources, reading on from where its last watch stopped.
--all reads on the same way. occurred_at is when the change was made, in UTC. Changes are held
for 7 days: a watch whose saved place is older prints one line of kind watch.reset, reads on
from the newest change, and returns; re-read state (hl situation) then.
A job that waits is told as experiment.blocked, and as job.held once, when it has waited longer
than its estimate. value is the code of what it waits for (not_admitted_reason), detail the sentence
(not_admitted_message).`, stderr)
	experiment := fs.String("experiment", "", "watch every event of one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "platform experiment to watch (default the token's unless --experiment is set)")
	agent := fs.String("agent", "", "watch this agent's own resources: its jobs' outcomes and the queue reasons waiting will not clear, its quota and cut, stage advances, the run's status and description, others' comments, findings and links on its hypotheses, and others' comments on the objective (default the token's agent unless --experiment or --all is set)")
	all := fs.Bool("all", false, "watch the whole platform experiment instead of one agent's resources")
	decisions := fs.Bool("decisions", false, "with --all, leave out what needs no decision: a job being queued, admitted or started, and a queue reason that waiting clears")
	kinds := fs.String("kinds", "", "comma-separated event kinds instead of every kind; see GET /watch/kinds")
	showSubscription := fs.Bool("show-subscription", false, "print what the server says this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'. A job ends COMPLETED, FAILED, EVICTED, REJECTED or CANCELLED")
	wake := fs.String("wake", "", "comma-separated event kinds, each KIND, KIND=VALUE for events of that value only, KIND!=VALUE for events of any other, and KIND@SUBJECT, alone or ahead of =VALUE or !=VALUE, for events of that subject only: return only once an event of one of them arrives, and print ahead of it the others that came before. Until then those are not printed and the next watch gets them again")
	noWait := fs.Bool("no-wait", false, "print the changes already waiting and return once caught up, without listening for more")
	follow := fs.Bool("follow", false, "without --until, stream until the timeout instead of returning with the first batch of events")
	timeout := fs.Float64("timeout", 540.0, "give up after this many seconds")
	after := fs.Int64("after", 0, "read on from the change after this id instead of the saved place")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}

	if *experiment == "" {
		c := apiclient.New(apiURL, token)
		*platformExperiment = c.Experiment(*platformExperiment)
		if apiclient.ReadsEveryAgent(token, *agent) {
			*all = true
		}
		if !*all {
			*agent = c.Agent(*agent)
		}
	}

	// An agent watching its own resources with every kind reads on where its last watch stopped,
	// and so does a watch of the whole platform experiment. Any other subscription would skip
	// changes of kinds it did not carry.
	var cursorFile string
	if (*agent != "" || *all) && *platformExperiment != "" && *experiment == "" && *kinds == "" {
		dir, err := stateDir()
		if err != nil {
			fmt.Fprintf(stderr, "hl watch: %s\n", err)
			return 2
		}
		// No agent id is empty, so a whole experiment's cursor is no agent's file.
		cursorFile = filepath.Join(dir, "changes-"+url.PathEscape(*platformExperiment)+"-"+url.PathEscape(*agent)+".json")
	}

	var waking func(kind, subject, value string) bool
	if *wake != "" {
		var kinds []struct {
			Kind string `json:"kind"`
		}
		if err := apiclient.New(apiURL, token).Do("GET", "/watch/kinds", nil, &kinds); err != nil {
			return command.Fail(stderr, err)
		}
		known := map[string]bool{}
		for _, k := range kinds {
			known[k.Kind] = true
		}
		entries := strings.Split(*wake, ",")
		for _, entry := range entries {
			if kind := cutWake(entry).kind; !known[kind] {
				fmt.Fprintf(stderr, "hl watch: --wake: %q is no event kind; GET /watch/kinds lists them\n", kind)
				return 2
			}
		}
		waking = func(kind, subject, value string) bool {
			return slices.ContainsFunc(entries, func(entry string) bool {
				return cutWake(entry).wakes(kind, subject, value)
			})
		}
	}

	return watchclient.Run(watchclient.Options{
		URL:                apiURL,
		Token:              token,
		Experiment:         *experiment,
		PlatformExperiment: *platformExperiment,
		Agent:              *agent,
		Kinds:              *kinds,
		Decisions:          *decisions && *all,
		ShowSubscription:   *showSubscription,
		Until:              *until,
		Follow:             *follow,
		NoWait:             *noWait,
		Wake:               waking,
		Timeout:            time.Duration(*timeout * float64(time.Second)),
		After:              *after,
		CursorFile:         cursorFile,
		Stdout:             stdout,
		Stderr:             stderr,
	})
}

// wakeEntry is one entry of --wake: KIND[@SUBJECT][=VALUE|!=VALUE].
type wakeEntry struct {
	kind, subject, value string
	onSubject, onValue   bool
	// differs: the entry wakes for an event of any value but value.
	differs bool
}

// cutWake reads one entry of --wake: KIND wakes for every event of the kind, KIND@SUBJECT for
// those of that subject, and =VALUE or !=VALUE after either for those with that value and those
// with any other.
func cutWake(entry string) wakeEntry {
	var w wakeEntry
	rest := entry
	if before, value, ok := strings.Cut(rest, "!="); ok {
		rest, w.value, w.onValue, w.differs = before, value, true, true
	} else if before, value, ok := strings.Cut(rest, "="); ok {
		rest, w.value, w.onValue = before, value, true
	}
	w.kind = rest
	if kind, subject, ok := strings.Cut(rest, "@"); ok {
		w.kind, w.subject, w.onSubject = kind, subject, true
	}
	return w
}

func (w wakeEntry) wakes(kind, subject, value string) bool {
	if w.kind != kind || (w.onSubject && w.subject != subject) {
		return false
	}
	return !w.onValue || (value == w.value) != w.differs
}

// stateDir is where hl keeps state between runs: $HL_STATE_DIR, else $XDG_STATE_HOME/hl, else
// ~/.local/state/hl.
func stateDir() (string, error) {
	if dir := os.Getenv("HL_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "hl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no state directory: set HL_STATE_DIR (%w)", err)
	}
	return filepath.Join(home, ".local", "state", "hl"), nil
}
