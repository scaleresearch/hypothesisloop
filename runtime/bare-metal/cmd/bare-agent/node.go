package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/hostinstall"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const usage = `usage: bare-agent                                          run the agent (what the installed unit does)
       bare-agent install --cluster NAME --api-url URL     install or upgrade the agent on this node and start it
                          [--dry-run]                      check the node and the token, print what install does, change nothing
       bare-agent detach                                   remove this node's cluster, stop its jobs, stop the agent
       bare-agent version                                  print the version this binary was built as
       bare-agent uninstall                                remove the agent from a detached node`

var commands = map[string]func(args []string) error{
	"install":   install,
	"detach":    detach,
	"uninstall": uninstall,
	"version": func([]string) error {
		fmt.Println(version)
		return nil
	},
}

func say(format string, args ...any) {
	fmt.Printf("[bare-agent] "+format+"\n", args...)
}

// The installed agent runs as root: it writes below /usr/local/bin, /etc and /etc/systemd,
// and the engine it starts containers through is root on the node whoever asks it.
func asRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("this needs root (sudo)")
	}
	return nil
}

func agentFor(settings hostinstall.Settings) *agentloop.Agent {
	return &agentloop.Agent{
		ClusterName:       settings.ClusterName,
		APIURL:            settings.APIURL,
		Token:             settings.Token,
		Version:           version,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		Log:               say,
	}
}

func install(args []string) error {
	flags := flag.NewFlagSet("bare-agent install", flag.ContinueOnError)
	cluster := flags.String("cluster", "", "the name this node was registered under")
	apiURL := flags.String("api-url", "", "the platform's API address, as this node and its job containers reach it")
	dryRun := flags.Bool("dry-run", false, "check this node, the platform and the token, print what install would write and run, and change nothing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *cluster == "" || *apiURL == "" || flags.NArg() != 0 {
		return errors.New("--cluster NAME and --api-url URL are required")
	}
	if err := asRoot(); err != nil {
		return err
	}
	host := hostinstall.Node{}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// The printed command stages the binary beside its place. Installed or refused, the staged
	// copy has done its part.
	if self == hostinstall.BinaryPath+".new" {
		defer host.Remove(self)
	}
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("this host is not run by systemd, which is what keeps the agent running")
	}
	installed, _, err := hostinstall.ReadSettings(host)
	if err != nil {
		return err
	}
	settings := hostinstall.Settings{ClusterName: *cluster, APIURL: *apiURL}
	if settings.Token, err = readToken(settings.ClusterName, installed); err != nil {
		return err
	}
	if err := startPodmanSocket(host, *dryRun); err != nil {
		return err
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		return err
	}

	check := func() (string, error) {
		executor, err := newExecutor(settings.ClusterName, settings.APIURL)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		facts, err := executor.Preflight(ctx, &http.Client{Timeout: 10 * time.Second})
		if err != nil {
			return "", err
		}
		for _, fact := range facts {
			say("%s", fact)
		}
		agent := agentFor(settings)
		agent.Executor = executor
		if err := agent.CheckAccess(ctx); errors.Is(err, agentloop.ErrClusterRemoved) {
			return "", fmt.Errorf("no cluster named %s is registered at %s", settings.ClusterName, settings.APIURL)
		} else if err != nil {
			return "", fmt.Errorf("the platform does not take this token for cluster %s: %w", settings.ClusterName, err)
		}
		return executor.Engine(), nil
	}
	if *dryRun {
		engine, err := check()
		if err != nil {
			return fmt.Errorf("dry run, nothing was changed: %w", err)
		}
		plan, err := hostinstall.Plan(engine)
		if err != nil {
			return err
		}
		fmt.Print(plan)
		say("dry run: this node is ready and nothing was changed. Run the same command without --dry-run to install.")
		return nil
	}
	if err := hostinstall.Install(host, binary, settings, check); err != nil {
		return fmt.Errorf("nothing was installed: %w", err)
	}
	say("installed %s, %s (root only) and %s", hostinstall.BinaryPath, hostinstall.EnvPath, hostinstall.UnitPath)
	say("the agent of cluster %s is started and starts at boot: journalctl -u %s -f", settings.ClusterName, hostinstall.UnitName)
	say("to detach this node: sudo bare-agent detach")
	return nil
}

// readToken reads the runtime token from standard input, without echo when that is a terminal,
// so it is in no shell history and no process's arguments. Given nothing, an upgrade keeps the
// token the node already holds for the same cluster.
func readToken(cluster string, installed hostinstall.Settings) (string, error) {
	keeps := installed.ClusterName == cluster && installed.Token != ""
	var raw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		prompt := "Runtime token of " + cluster
		if keeps {
			prompt += " (Enter keeps the one installed)"
		}
		fmt.Fprint(os.Stderr, prompt+": ")
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	}
	if err != nil {
		return "", fmt.Errorf("read the runtime token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" && keeps {
		return installed.Token, nil
	}
	if token == "" {
		return "", errors.New("no runtime token was given on standard input")
	}
	return token, nil
}

// A node with podman and no docker has an engine only once podman's socket is listening, which
// is off until asked for.
func startPodmanSocket(host hostinstall.Host, dryRun bool) error {
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("podman"); err != nil {
		return errors.New("neither docker nor podman is on this node")
	}
	if host.Systemctl("is-active", "--quiet", "podman.socket") == nil {
		return nil
	}
	if dryRun {
		return errors.New("docker is not on this node and podman's socket is off: install would start it (systemctl enable --now podman.socket), and the dry run cannot check the node without it")
	}
	say("docker is not on this node: starting podman's socket (systemctl enable --now podman.socket)")
	return host.Systemctl("enable", "--now", "podman.socket")
}

func detach(args []string) error {
	if len(args) != 0 {
		return errors.New("detach takes no arguments")
	}
	host := hostinstall.Node{}
	settings, installed, err := hostinstall.ReadSettings(host)
	if errors.Is(err, os.ErrPermission) {
		return asRoot()
	}
	if err != nil {
		return err
	}
	if installed {
		if err := asRoot(); err != nil {
			return err
		}
	} else {
		// An agent started by hand: the same three variables it runs with.
		settings = hostinstall.Settings{
			ClusterName: agentloop.RequiredEnv(binaryName, "CLUSTER_NAME"),
			APIURL:      agentloop.RequiredEnv(binaryName, "API_URL"),
			Token:       agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN"),
		}
	}
	agent := agentFor(settings)
	ctx, cancel := agentloop.SignalContext(say)
	defer cancel()

	requeued, err := agent.Detach(ctx)
	switch {
	case errors.Is(err, agentloop.ErrClusterRemoved):
		say("cluster %s is already removed from %s", settings.ClusterName, settings.APIURL)
	case err != nil:
		return fmt.Errorf("nothing was changed: %w", err)
	default:
		say("removed cluster %s from %s: %d of its jobs went back to the queue", settings.ClusterName, settings.APIURL, len(requeued))
	}
	if err := hostinstall.Stop(host); err != nil {
		return err
	}
	// With the unit stopped this process is the node's only agent, and the loop that keeps a
	// node in step is what stops everything a removed cluster's node still runs.
	executor, err := newExecutor(settings.ClusterName, settings.APIURL)
	if err != nil {
		return err
	}
	agent.Executor = executor
	err = agent.Run(ctx)
	executor.Wait()
	if !errors.Is(err, agentloop.ErrClusterRemoved) {
		return errors.New("interrupted before every job container was stopped; run it again")
	}
	say("every job container this node ran is stopped, and the agent no longer starts at boot")
	if installed {
		say("sudo bare-agent uninstall removes the agent from this node")
	}
	return nil
}

func uninstall(args []string) error {
	if len(args) != 0 {
		return errors.New("uninstall takes no arguments")
	}
	if err := asRoot(); err != nil {
		return err
	}
	if err := hostinstall.Uninstall(hostinstall.Node{}); err != nil {
		return err
	}
	say("removed %s, %s and %s", hostinstall.UnitPath, hostinstall.EnvPath, hostinstall.BinaryPath)
	say("left on this node: the images its jobs pulled, which the engine's image prune removes")
	return nil
}
