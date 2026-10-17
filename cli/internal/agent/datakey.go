package agent

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func DataKey(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("data-key", `usage: hl data-key [--platform-experiment PE] [--agent A]

Your own S3 key to the platform experiment's data, as export lines: eval "$(hl data-key)".
It reads everything under $HYPOTHESISLOOP_DATA_SHARED and writes under $HYPOTHESISLOOP_DATA_URI,
where your jobs write. It opens nothing of another run: what a job of one of its prior_runs
(hl platform-experiments get) wrote, hl job data JOB lists with a url for each object. The
same key every time; it does not expire. Any S3 client works:
  uvx --from awscli aws s3 ls "$HYPOTHESISLOOP_DATA_SHARED"`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	agent := fs.String("agent", "", "whose key it is, for a person or an API key; an agent's token says it")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	*pe = c.Experiment(*pe)
	*agent = c.Agent(*agent)
	if *pe == "" || *agent == "" || fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment and --agent are required, and nothing else"))
	}
	var out clusterapi.DataAccess
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(*pe)+"/data-key", map[string]string{"agent_id": *agent}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	for _, v := range [][2]string{
		{jobenv.AccessKeyID, out.AccessKeyID},
		{jobenv.SecretAccessKey, out.SecretAccessKey},
		{jobenv.Endpoint, out.Endpoint},
		{jobenv.EndpointS3, out.Endpoint},
		{jobenv.Region, out.Region},
		{jobenv.DataURI, out.URI},
		{jobenv.DataShared, out.Shared},
	} {
		fmt.Fprintf(stdout, "export %s='%s'\n", v[0], strings.ReplaceAll(v[1], "'", `'\''`))
	}
	return 0
}
