package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/repotemplate"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

var envCommands = []subcommand{
	{"build", "start building HEAD's .hypothesisloop/Dockerfile before the job that runs in it", envBuild, asAgent},
	{"status", "say whether HEAD's .hypothesisloop/Dockerfile is building, built or failed, building nothing", envStatus, asAgent},
	{"run", "run a command in the environment on the job's hardware, to check code before a job", envRun, asAgent},
}

func envBuild(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env build", `usage: hl env build [FILE] [--image IMAGE] [--hypothesis ID] [--platform-experiment PE]

Starts building the environment HEAD's commit declares in .hypothesisloop/Dockerfile, on the
image its job will name, so that the job finds it built, or building, when it is submitted: a
job of the same commit naming the same image runs in this one build. Commit the file first; the
commit is pushed as hl job submit pushes it. Run it as soon as the file is committed, then write
the rest of the job while it builds. Asking again is safe and builds again a file that failed.

FILE is the job spec the job will be submitted with: its job.image, and metadata.hypothesis_id
for any agent but the baseline. It defaults to the repository's .hypothesisloop/job.yaml when
that exists. --image and --hypothesis set them, and override the file's. Prints the
environment: its key, state (building, ready, failed) and a message saying where the build is,
or the end of its output once the file failed.

Refusals: no_environment_file (the commit has no .hypothesisloop/Dockerfile);
invalid_environment_file; no_environment_builder; unknown_job_image; code_ref_not_in_repository.`, stderr)
	image := fs.String("image", "", "the image the job will name, job.image of FILE by default")
	hypothesis := fs.String("hypothesis", "", "the hypothesis the job will test, metadata.hypothesis_id of FILE by default")
	pe := experimentFlag(fs)
	request, code := environmentAsked(c, fs, args, image, hypothesis, pe, "env build", stderr)
	if code >= 0 {
		return code
	}
	var out environmentState
	if err := c.Do("POST", "/environments", request, &out); err != nil {
		return fail(stderr, err)
	}
	return printEnvironment(stdout, out)
}

func envStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env status", `usage: hl env status [FILE] [--image IMAGE] [--hypothesis ID] [--platform-experiment PE]

Prints the environment HEAD's commit declares in .hypothesisloop/Dockerfile, on the image its
job will name, as it is: its key, state (building, ready, failed) and a message saying where the
build is, or the end of its output once the file failed. It builds nothing: a failed file stays
failed, and a job is never made. FILE, --image and --hypothesis are those of hl env build.
Exits 0 while building or once ready, 1 once failed.

Refusals: environment_not_requested (nothing asked for its build: hl env build starts it);
no_environment_file; invalid_environment_file.`, stderr)
	image := fs.String("image", "", "the image the job will name, job.image of FILE by default")
	hypothesis := fs.String("hypothesis", "", "the hypothesis the job will test, metadata.hypothesis_id of FILE by default")
	pe := experimentFlag(fs)
	request, code := environmentAsked(c, fs, args, image, hypothesis, pe, "env status", stderr)
	if code >= 0 {
		return code
	}
	var out environmentState
	if err := c.Do("GET", environmentPath(request), nil, &out); err != nil {
		return fail(stderr, err)
	}
	return printEnvironment(stdout, out)
}

// environmentState is an environment as the platform answers it.
type environmentState struct {
	Key     string `json:"key"`
	State   string `json:"state"`
	Base    string `json:"base"`
	Image   string `json:"image,omitempty"`
	Fault   string `json:"fault,omitempty"`
	Message string `json:"message"`
}

func printEnvironment(stdout io.Writer, out environmentState) int {
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	if out.State == "failed" {
		return 1
	}
	return 0
}

// environmentPath is GET /environments of the environment request names.
func environmentPath(request map[string]any) string {
	query := url.Values{}
	for _, name := range []string{"platform_experiment_id", "code_ref", "image"} {
		if v, _ := request[name].(string); v != "" {
			query.Set(name, v)
		}
	}
	return "/environments?" + query.Encode()
}

// environmentAsked is the request naming the environment HEAD's commit declares for the job of
// FILE, the one operand of args, or of the repository's job spec, with image and hypothesis in
// place of the file's. A code of 0 or more is the exit of a command that cannot ask.
func environmentAsked(c *apiclient.Client, fs *flagSet, args []string, image, hypothesis, pe *string, command string, stderr io.Writer) (map[string]any, int) {
	operands, err := parseOperands(fs, args)
	if err != nil {
		return nil, parseExit(err)
	}
	if len(operands) > 1 {
		return nil, parseExit(misuse(fs, "want at most one FILE, got %d", len(operands)))
	}
	if len(operands) == 0 {
		if root, err := git(".", "rev-parse", "--show-toplevel"); err == nil {
			spec := filepath.Join(root, repotemplate.JobSpec)
			if _, err := os.Stat(spec); err == nil {
				operands = []string{spec}
			}
		}
	}
	metadata := map[string]any{}
	if len(operands) == 1 {
		body, err := readYAMLBody(operands[0])
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return nil, 2
		}
		if m, ok := body["metadata"].(map[string]any); ok {
			metadata = m
		}
		if job, ok := body["job"].(map[string]any); ok && *image == "" {
			*image, _ = job["image"].(string)
		}
	}
	if *image == "" {
		return nil, parseExit(misuse(fs, "no image: name the job spec FILE or --image IMAGE, the image the job will run on"))
	}
	if *hypothesis != "" {
		metadata["hypothesis_id"] = *hypothesis
	}
	if v, _ := metadata["platform_experiment_id"].(string); *pe == "" && v != "" {
		*pe = v
	}
	metadata["platform_experiment_id"] = experimentOf(c, *pe)
	if err := fillCodeRef(metadata, ""); err != nil {
		return nil, fail(stderr, fmt.Errorf("%s: %w", command, err))
	}
	return map[string]any{"platform_experiment_id": metadata["platform_experiment_id"], "code_ref": metadata["code_ref"], "image": *image}, -1
}

// environmentReadTries is the most reads of a building environment in a row that may fail before hl
// env run stops waiting for it.
const environmentReadTries = 10

// awaitEnvironment starts the build of the environment the check's commit declares on its image,
// as its submit would, and waits for it, so no check is made while it builds or once its file
// failed. It returns -1 once the check may be made: the environment is ready, or the commit
// declares none. Otherwise it returns the exit of hl env run, and no check was made.
func awaitEnvironment(c *apiclient.Client, request map[string]any, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	var env environmentState
	if err := c.Do("POST", "/environments", request, &env); err != nil {
		if reason, _ := refusalOf(err); reason == "no_environment_file" {
			return -1
		}
		return fail(stderr, fmt.Errorf("env run: the environment: %w", err))
	}
	said, failedReads := "", 0
	for {
		switch env.State {
		case "building":
		case "failed":
			fmt.Fprintf(stderr, "hl: env run: the environment failed to build, so no check was made: fix .hypothesisloop/Dockerfile, commit and run again\n")
			return printEnvironment(stdout, env)
		default:
			return -1
		}
		if env.Message != said {
			fmt.Fprintf(stderr, "hl: env run: no check is made until the environment is built: %s\n", env.Message)
			said = env.Message
		}
		select {
		case <-signals:
			fmt.Fprintf(stderr, "hl: env run: interrupted while the environment builds; no check was made, and hl env status says where the build is\n")
			return 130
		case <-time.After(checkPoll):
		}
		var now environmentState
		if err := c.Do("GET", environmentPath(request), nil, &now); err != nil {
			if failedReads++; failedReads >= environmentReadTries {
				return fail(stderr, fmt.Errorf("env run: reading the environment: %w; no check was made, and hl env status says where the build is", err))
			}
			continue
		}
		failedReads, env = 0, now
	}
}

// checkPoll is how often hl env run reads its check's status and logs.
var checkPoll = 3 * time.Second

// checkReadTries is how many reads of the check's status in a row may fail before hl env run
// stops following it.
const checkReadTries = 10

// checkLogLines is how much of the check's log tail each poll reads.
const checkLogLines = 500

func envRun(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runCheck(c, args, stdout, stderr, signals)
}

func runCheck(c *apiclient.Client, args []string, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	fs := newFlags("env run", `usage: hl env run [FILE] [--cpu N] [--memory Q] [--storage Q] [--estimated-duration-hours H] [--env K=V ...] [--secret-env NAME ...] [--repeat N] -- COMMAND [ARG...]

Runs COMMAND as a check: in the experiment's environment, on a cluster node of the hardware
the job spec names, in a clone of the commit HEAD is at. It is the way to check that code
builds and runs before a real job, where your own machine lacks the job's toolchain or
hardware. Commit first; the branch is pushed as hl job submit pushes it.

A check is no result: it tests no hypothesis, ranks nowhere, owes no summary, and the metrics
it posts are not recorded. It holds your quota like a job, its estimate until it ends and then
what it used, and is stopped after 10 minutes.

FILE is the job spec the check takes its image, accelerator, cpu, memory, storage and env from,
the repository's .hypothesisloop/job.yaml by default; its command is replaced by COMMAND, run
from the clone's root. One COMMAND argument is run as a shell line ('cargo build && ./x');
several are run as one command with their arguments quoted.

A commit that declares .hypothesisloop/Dockerfile is built first, as hl env build builds it, and
no check is made until it is built: hl env run says on stderr where the build is while it waits,
and once the file failed it prints the environment with the end of its output and exits 1.

Prints the check's id, then its output as it arrives, and its status on stderr while it waits
or runs. Exits with COMMAND's exit code, 0 once COMPLETED, 1 for an end that has no code.
Interrupted, it cancels the check, or makes none while the environment builds.`, stderr)
	agent := fs.String("agent", "", "whose check it is, for a person or an API key; an agent's token says it")
	pe := experimentFlag(fs)
	cpu := fs.String("cpu", "", "sets job.cpu, the cores the check holds")
	memory := fs.String("memory", "", "sets job.memory, e.g. 8Gi")
	storage := fs.String("storage", "", "sets job.storage, its scratch space, e.g. 10Gi")
	estimate := fs.String("estimated-duration-hours", "", "sets metadata.estimated_duration_hours, at most 10 minutes; the platform's check estimate by default")
	repeat := fs.Int("repeat", 1, "which run of this same check it is: an identical one already run is answered with its result, `N` runs it an Nth time")
	var env stringList
	fs.Var(&env, "env", "job.env entry as `K=V`, repeat per variable")
	var secretEnv stringList
	fs.Var(&secretEnv, "secret-env", "job.secret_env entry by `NAME`, its value read from $NAME here; repeat per variable")

	split := slices.Index(args, "--")
	if split < 0 {
		if slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" }) {
			return parseExit(fs.Parse([]string{"--help"}))
		}
		return parseExit(misuse(fs, "want -- COMMAND: the command to run follows a --"))
	}
	command := args[split+1:]
	operands, err := parseOperands(fs, args[:split])
	if err != nil {
		return parseExit(err)
	}
	if len(command) == 0 {
		return parseExit(misuse(fs, "want a COMMAND after --"))
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one FILE, got %d", len(operands)))
	}
	if len(operands) == 0 {
		if root, err := git(".", "rev-parse", "--show-toplevel"); err == nil {
			if spec := filepath.Join(root, repotemplate.JobSpec); fileExists(spec) {
				operands = []string{spec}
			}
		}
	}
	if len(operands) == 0 {
		return parseExit(misuse(fs, "no job spec: name FILE, or run it in a repository with %s", repotemplate.JobSpec))
	}
	if *agent = agentOf(c, *agent); *agent == "" {
		return parseExit(misuse(fs, "--agent is required: HL_TOKEN is no agent's token, so say whose check this is"))
	}
	if *repeat < 1 {
		return parseExit(misuse(fs, "--repeat wants 1 or more, got %d", *repeat))
	}
	body, err := readYAMLBody(operands[0])
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	job, _ := body["job"].(map[string]any)
	if job == nil {
		return parseExit(misuse(fs, "%s has no job mapping to take the image and resources from", operands[0]))
	}
	job["command"] = checkCommand(command)
	delete(job, "args")
	if *cpu != "" {
		if n, err := strconv.ParseFloat(*cpu, 64); err == nil {
			job["cpu"] = n
		} else {
			job["cpu"] = *cpu
		}
	}
	if *memory != "" {
		job["memory"] = *memory
	}
	if *storage != "" {
		job["storage"] = *storage
	}
	if err := setJobEnv(body, env); err != nil {
		return parseExit(misuse(fs, "%s", err))
	}
	if err := setJobSecretEnv(body, secretEnv); err != nil {
		return parseExit(misuse(fs, "%s", err))
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	delete(metadata, "hypothesis_id")
	delete(metadata, "theory")
	delete(metadata, "estimated_duration_hours")
	metadata["check"] = true
	metadata["agent_id"] = *agent
	if *estimate != "" {
		hours, err := strconv.ParseFloat(*estimate, 64)
		if err != nil || hours <= 0 {
			return parseExit(misuse(fs, "--estimated-duration-hours wants a positive number of hours, got %q", *estimate))
		}
		metadata["estimated_duration_hours"] = hours
	}
	if v, _ := metadata["platform_experiment_id"].(string); *pe == "" && v != "" {
		*pe = v
	}
	metadata["platform_experiment_id"] = experimentOf(c, *pe)
	if err := fillCodeRef(metadata, ""); err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	body["metadata"] = metadata
	if image, _ := job["image"].(string); image != "" {
		request := map[string]any{"platform_experiment_id": metadata["platform_experiment_id"], "code_ref": metadata["code_ref"], "image": image}
		if code := awaitEnvironment(c, request, stdout, stderr, signals); code >= 0 {
			return code
		}
	}
	if body["submission_key"], err = submissionKey(body, *repeat); err != nil {
		return fail(stderr, fmt.Errorf("env run: %w", err))
	}
	var out struct {
		ID      string `json:"id"`
		Status  string `json:"status,omitempty"`
		CodeRef string `json:"code_ref,omitempty"`
		Branch  string `json:"branch,omitempty"`
		Check   bool   `json:"check,omitempty"`
	}
	if err := c.Do("POST", "/experiments", body, &out); err != nil {
		return fail(stderr, err)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	if out.ID == "" {
		fmt.Fprintln(stderr, "hl: env run: the platform answered the check with no id")
		return 1
	}
	if out.Status != "QUEUED" {
		fmt.Fprintf(stderr, "hl: env run: an identical check already ran as %s, which is %s: its output follows. --repeat %d runs it again\n", out.ID, out.Status, *repeat+1)
	}
	return followCheck(c, out.ID, stdout, stderr, signals)
}

// checkCommand runs command from the root of a clone of the job's commit. One argument is a shell
// line, several are one command whose arguments are quoted.
func checkCommand(command []string) []string {
	line := command[0]
	if len(command) > 1 {
		quoted := make([]string, len(command))
		for i, arg := range command {
			quoted[i] = shellQuote(arg)
		}
		line = strings.Join(quoted, " ")
	}
	return []string{"bash", "-c", "dir=$(hl-clone) && cd \"$dir\" && " + line}
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkRecord is what hl env run reads of its check on every poll.
type checkRecord struct {
	Status             string `json:"status"`
	NotAdmittedReason  string `json:"not_admitted_reason"`
	NotAdmittedMessage string `json:"not_admitted_message"`
	EvictionReason     string `json:"eviction_reason"`
	RanOn              struct {
		Node string `json:"node"`
	} `json:"ran_on"`
	PhaseDetail struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"phase_detail"`
}

// followCheck prints the check's new output and status changes until it ends, and returns the
// exit code its command ended with.
func followCheck(c *apiclient.Client, id string, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	var printed []string
	said, failedReads := "", 0
	path := "/experiments/" + url.PathEscape(id)
	printNew := func() {
		var tail []string
		if err := c.Do("GET", path+"/logs?n="+strconv.Itoa(checkLogLines), nil, &tail); err == nil {
			fresh := newLines(printed, tail)
			for _, l := range fresh {
				fmt.Fprintln(stdout, l)
			}
			printed = append(printed, fresh...)
			if len(printed) > checkLogLines {
				printed = printed[len(printed)-checkLogLines:]
			}
		}
	}
	for {
		var rec checkRecord
		statusErr := c.Do("GET", path, nil, &rec)
		printNew()
		if statusErr != nil {
			if failedReads++; failedReads >= checkReadTries {
				return fail(stderr, fmt.Errorf("env run: reading %s: %w; hl job status %s and hl job logs %s read it later", id, statusErr, id, id))
			}
			fmt.Fprintf(stderr, "hl: env run: reading %s: %v; trying again\n", id, statusErr)
		} else if rec.Status == "" {
			fmt.Fprintf(stderr, "hl: env run: the platform answered %s with no status\n", id)
			return 1
		} else {
			failedReads = 0
			if now := checkSays(rec); now != said {
				fmt.Fprintf(stderr, "hl: env run: %s %s\n", id, now)
				said = now
			}
			if slices.Contains(endedStatuses, rec.Status) {
				// The cluster may report the last lines after the end it reported.
				time.Sleep(checkPoll)
				printNew()
				return checkExit(rec, printed)
			}
		}
		select {
		case <-signals:
			fmt.Fprintf(stderr, "hl: env run: interrupted, cancelling %s\n", id)
			if err := c.Do("POST", path+"/cancel", map[string]any{}, nil); err != nil {
				fmt.Fprintf(stderr, "hl: env run: cancel %s: %v; hl job cancel %s cancels it\n", id, err, id)
			}
			return 130
		case <-time.After(checkPoll):
		}
	}
}

// checkSays is the status line of rec, which is said again only when it changes.
func checkSays(rec checkRecord) string {
	switch {
	case rec.Status == "QUEUED" && rec.NotAdmittedReason != "":
		return strings.TrimSpace("QUEUED " + rec.NotAdmittedReason + ": " + rec.NotAdmittedMessage)
	case rec.Status == "RUNNING" && rec.RanOn.Node != "":
		return "RUNNING on node " + rec.RanOn.Node
	case rec.EvictionReason != "":
		return rec.Status + ": " + rec.EvictionReason
	}
	return rec.Status
}

// newLines is what of tail follows the lines already printed: the tail starts somewhere in
// printed, at the longest end of printed it begins with.
func newLines(printed, tail []string) []string {
	for k := min(len(printed), len(tail)); k > 0; k-- {
		if slices.Equal(printed[len(printed)-k:], tail[:k]) {
			return tail[k:]
		}
	}
	return tail
}

var exitCodeIn = regexp.MustCompile(`(?:exit(?:ed with)? code|exit-code=)\s*(\d+)`)

// checkExit is the code the check's command ended with: 0 once COMPLETED, else the code its
// wrapper printed or its runtime reported, else 1.
func checkExit(rec checkRecord, printed []string) int {
	if rec.Status == "COMPLETED" {
		return 0
	}
	if code, ok := exitline.Find(printed); ok && code != 0 {
		return code
	}
	for _, text := range []string{rec.EvictionReason, rec.PhaseDetail.Message} {
		if m := exitCodeIn.FindStringSubmatch(text); m != nil {
			if code, err := strconv.Atoi(m[1]); err == nil && code > 0 && code < 256 {
				return code
			}
		}
	}
	return 1
}
