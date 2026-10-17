package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// registerWatchKinds documents the change stream in the one place an agent already looks. The
// socket itself cannot be a Huma operation — an upgrade needs the raw connection, and Huma owns
// the response of everything registered with it — so its vocabulary is served as an ordinary GET
// beside it. That makes it discoverable from /explore instead of folklore an agent could only
// learn by being told; the instructions the platform serves an agent carry only the rules, as /explore/rules.
//
// The body is domain.EventKinds itself, the same slice the stream validates a query against, so this
// cannot describe a stream the server does not serve.
func registerWatchKinds(doc *Doc) {
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-watch-kinds", Method: "GET", Path: "/watch/kinds",
		Summary: "List the event kinds GET /watch can carry", Tags: []string{"watch"},
		Description: "GET /watch is a WebSocket that pushes small typed records, the changes of a platform experiment, " +
			"so you can wait on a change instead of polling for it. Every change is a pointer, never a copy: follow one " +
			"with the ordinary GET when you want detail. A change frame is {\"id\", \"kind\", \"subject\", \"value\", \"detail\", " +
			"\"platform_experiment_id\", \"agent_id\", \"occurred_at\"}: `id` orders the stream, and `occurred_at` is when the change was made.\n\n" +
			"Scope a subscription with `platform_experiment_id` or `experiment_id` (one is required), narrow it with " +
			"`agent_id` and a comma-separated `kinds`, and say where to read from with `after`: the id of the last change you read, " +
			"`head` for only what comes next, or nothing for every change still held. The first frame is {\"subscription\": {...}}, " +
			"the filter in force. Each read ends with a progress frame {\"next\": N, \"caught_up\": true|false}: a client that saved N " +
			"reads on with after=N and misses nothing and repeats nothing. Changes are held for 7 days. An after this platform never issued, " +
			"or one older than what is held, is refused with 410: re-read state and watch from head.\n\n" +
			"`agent_id` scopes the stream to that agent's own resources, as `agent_scope` below says per kind: the terminal " +
			"status of its jobs and a queue reason that waiting will not clear, its quota and cut, ladder advances, the run's status and brief, and " +
			"other people's comments, findings and links on hypotheses it registered. Its own actions are not echoed back. " +
			"An agent's token is scoped to its own agent. Without `agent_id` you get the whole platform experiment.\n\n" +
			"Omit `kinds` and you get every kind below. An unknown kind is refused. Nothing may be sent up the socket.\n\n" +
			"GET /changes takes the same parameters and `limit` (at most 500) and answers one read as {\"changes\", \"next\", \"caught_up\"}.\n\n" +
			"`hl watch` holds the socket for you: with an agent's token it watches that agent's own resources, " +
			"reads on from where it stopped last time, and returns with the first batch of changes.",
	}, listWatchKinds)
}

func listWatchKinds(context.Context, *struct{}) (*struct{ Body []domain.EventKindDoc }, error) {
	return &struct{ Body []domain.EventKindDoc }{Body: domain.EventKinds}, nil
}
