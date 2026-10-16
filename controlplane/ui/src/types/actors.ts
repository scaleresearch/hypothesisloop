// Whose credential made a record, as the platform authenticated it. Nothing a request says sets
// it. `unverified` is a record from before actors were kept: its name is the one stated then.
export type ActorKind = 'user' | 'key' | 'agent' | 'coordinator' | 'staff' | 'unverified'

export interface Actor {
  kind: ActorKind
  /** The user, key, agent or platform experiment (for a coordinator); empty for staff and unverified. */
  id: string
  /** The name it carries now, its id when it has none. */
  name: string
}
