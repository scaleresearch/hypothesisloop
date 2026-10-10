package domain

// ActorKind says what sort of credential made a request.
type ActorKind string

const (
	ActorUser        ActorKind = "user"
	ActorKey         ActorKind = "key"
	ActorAgent       ActorKind = "agent"
	ActorCoordinator ActorKind = "coordinator"
	ActorStaff       ActorKind = "staff"
	// ActorUnverified marks a record written before the platform recorded the credential behind
	// a write. Its Name is what the record said of its author then, and it has no ID.
	ActorUnverified ActorKind = "unverified"
)

// PersonKinds are the kinds of a person's credential: signed in, an API key, or staff.
var PersonKinds = []ActorKind{ActorUser, ActorKey, ActorStaff}

// Person says the credential is a person's, never an agent's or a coordinator's.
func (k ActorKind) Person() bool {
	return k == ActorUser || k == ActorKey || k == ActorStaff
}

// Actor is whose credential made a request: a signed-in person, an API key, an agent, the
// coordinator of a platform experiment (ID is the experiment's), or a staff token no person
// signed in with (ID is empty). It is made from the authenticated caller and never from what a
// request says. Kind and ID are stored with a record; Name is looked up when the record is read,
// so it is the name the actor carries now.
type Actor struct {
	Kind ActorKind `json:"kind" enum:"user,key,agent,coordinator,staff,unverified"`
	ID   string    `json:"id" doc:"The user, API key or agent, or the platform experiment of a coordinator. Empty for a staff token and for unverified."`
	Name string    `json:"name" doc:"The name the actor carries now, its id when it has none."`
}

// StoredActor is the actor a record was stored with. A record from before actors were kept is
// unverified and has no id: it is named by what it said of its author then, the author it
// stated or else the agent it named.
func StoredActor(kind ActorKind, id, author, agentID string) Actor {
	if kind != ActorUnverified {
		return Actor{Kind: kind, ID: id}
	}
	if author == "" {
		author = agentID
	}
	return Actor{Kind: ActorUnverified, Name: author}
}
