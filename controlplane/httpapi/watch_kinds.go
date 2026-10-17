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
		Description: "GET /watch is a WebSocket that pushes small typed records (kind, subject, value, detail, position, occurred_at, cursor) " +
			"so you can wait on a change instead of polling for it. Every event is a pointer, never a copy: follow one " +
			"with the ordinary GET when you want detail. `occurred_at` is when the change was made, the same on a live event and on its replay, and absent on the replay of a change from before the platform kept it.\n\n" +
			"Scope a subscription with `platform_experiment_id` or `experiment_id` (one is required), narrow it with " +
			"`agent` and a comma-separated `kinds`, and resume it with `since=<largest cursor you saw>`. A resume never " +
			"skips an event; it can repeat ones you already have, which carry the same `position` (the transaction " +
			"that made the change). A `since` this platform never issued is refused with 410: reconnect without it. " +
			"Omit `since` (or pass 0) and you get the current snapshot of everything your subscription admits before " +
			"going live.\n\n" +
			"`agent` scopes the stream to that agent's own resources, as `agent_scope` below says per kind: the terminal " +
			"status of its jobs and a queue reason that waiting will not clear, its quota and cut, ladder advances, the run's status and brief, and " +
			"other people's comments, findings and links on hypotheses it registered. Its own actions are not echoed back. " +
			"Without `agent` you get the whole platform experiment.\n\n" +
			"Omit `kinds` and you get every kind marked `default` below; only `metric.point` is left out, the " +
			"highest-volume kind and the one a reconnect cannot replay. An unknown kind is refused.\n\n" +
			"A connected client can inspect and re-scope itself without reconnecting: send a text frame `{}` to be " +
			"told the filter in force, or `{\"kinds\": \"a,b\"}` to change it (`{\"kinds\": \"\"}` restores the " +
			"default). The answer is a `{\"subscription\": {...}}` frame. Widening replays the added kinds from where " +
			"you last saw them; narrowing stops delivery. Nothing else may be sent up the socket.\n\n" +
			"`hl watch` holds the socket for you: with an agent's token it watches that agent's own resources, " +
			"resumes from the cursor it saved last time, and returns with the first batch of events.",
	}, listWatchKinds)
}

func listWatchKinds(context.Context, *struct{}) (*struct{ Body []domain.EventKindDoc }, error) {
	return &struct{ Body []domain.EventKindDoc }{Body: domain.EventKinds}, nil
}
