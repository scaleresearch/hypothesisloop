package shared

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

type modelListing struct {
	Models []struct {
		Model         string  `json:"model"`
		Provider      string  `json:"provider"`
		Name          string  `json:"name"`
		ContextLength int     `json:"context_length"`
		InputUSD      float64 `json:"input_usd_per_mtok"`
		OutputUSD     float64 `json:"output_usd_per_mtok"`
	} `json:"models"`
	DefaultModel     string `json:"default_model"`
	CoordinatorModel string `json:"coordinator_model"`
}

func Models(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("models", `usage: hl models [--provider PROVIDER] [--search TEXT] [--json]

The models an agent, an agent type or a coordinator can run on, one line each: the model to pass
to --model, its context length in tokens (- when unknown) and what a million input and output
tokens cost in USD. A model is always written <provider>/<model id>, such as
anthropic/claude-sonnet-5 or openrouter/moonshotai/kimi-k2. --provider keeps the models of one
provider the platform serves, such as anthropic, openrouter or openai. --search keeps the models whose
reference or name contains TEXT, in any case. The default and coordinator models follow.`, stderr)
	provider := fs.String("provider", "", "only the models of this provider, one the platform serves")
	search := fs.String("search", "", "only the models whose reference or name contains this text")
	asJSON := fs.Bool("json", false, "print the filtered listing as JSON")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	var out modelListing
	if err := c.Do("GET", "/models", nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	var served []string
	for _, m := range out.Models {
		if !slices.Contains(served, m.Provider) {
			served = append(served, m.Provider)
		}
	}
	if *provider != "" && !slices.Contains(served, *provider) {
		return command.ParseExit(command.Misuse(fs, "--provider must be a provider the platform serves, one of %s, got %s", strings.Join(served, ", "), *provider))
	}
	needle := strings.ToLower(*search)
	kept := out.Models[:0]
	for _, m := range out.Models {
		if (*provider == "" || m.Provider == *provider) &&
			(strings.Contains(strings.ToLower(m.Model), needle) || strings.Contains(strings.ToLower(m.Name), needle)) {
			kept = append(kept, m)
		}
	}
	out.Models = kept
	if *asJSON {
		command.PrintJSON(stdout, out)
		return 0
	}
	if len(kept) == 0 {
		fmt.Fprintln(stderr, "hl: no model matches")
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tCONTEXT\tIN $/M\tOUT $/M")
	for _, m := range kept {
		context := "-"
		if m.ContextLength > 0 {
			context = fmt.Sprint(m.ContextLength)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%.2f\t%.2f\n", m.Model, context, m.InputUSD, m.OutputUSD)
	}
	if err := w.Flush(); err != nil {
		return command.Fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "default: %s\ncoordinator: %s\n", out.DefaultModel, out.CoordinatorModel)
	return 0
}
