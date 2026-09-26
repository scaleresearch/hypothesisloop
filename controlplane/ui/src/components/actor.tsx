'use client'

import { clsx } from 'clsx'
import type { Actor as ActorRecord } from '@/types'
import { relTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { useAgentName } from '@/components/agent-label'

const UNVERIFIED = 'Written before the platform kept who wrote a record. The name is the one stated then, and no account stands behind it.'

function who(actor: ActorRecord): string {
  switch (actor.kind) {
    case 'key': return `key ${actor.name}`
    case 'unverified': return actor.name ? `${actor.name} (unverified)` : 'unverified'
    default: return actor.name
  }
}

// An agent's record made with its own token names the agent alone, as does one from before
// actors were kept.
function ownersOwn(actor: ActorRecord, agentID: string): boolean {
  return agentID !== '' && (actor.kind === 'unverified' || (actor.kind === 'agent' && actor.id === agentID))
}

/** Who made a record, in words, for a title or a sentence. agentID is the agent whose record it is. */
export function actorLabel(actor: ActorRecord, agentID = '', agentName = agentID): string {
  if (ownersOwn(actor, agentID)) return agentName
  return agentID ? `${agentName} (by ${who(actor)})` : who(actor)
}

/** Who gave a verdict and when, empty while none was given. */
export function verdictBy(h: { agent_id: string; status_actor: ActorRecord | null; status_changed_at: string | null }): string {
  if (h.status_changed_at == null) return ''
  return `${h.status_actor ? `by ${actorLabel(h.status_actor, h.agent_id)} · ` : ''}${relTime(h.status_changed_at)}`
}

type Judged = { status: string; agent_id: string; status_actor: ActorRecord | null; status_changed_at: string | null }

/** A hypothesis's verdict with who gave it and when. Nothing while none was given. */
export function Verdict({ hypothesis: h }: { hypothesis: Judged }) {
  if (h.status_changed_at == null) return null
  return (
    <>
      {h.status}{' '}
      {h.status_actor && <>by <Actor actor={h.status_actor} agentID={h.agent_id} /> · </>}
      {relTime(h.status_changed_at)}
    </>
  )
}

function Credential({ actor }: { actor: ActorRecord }) {
  switch (actor.kind) {
    case 'user':
      return <><Badge status="human">human</Badge><span title={actor.id}>{actor.name}</span></>
    case 'key':
      return <><Badge tone="info">key</Badge><span title={actor.id}>{actor.name}</span></>
    case 'coordinator':
      return <Badge tone="purple" title={actor.id}>coordinator</Badge>
    case 'staff':
      return <Badge tone="neutral">staff</Badge>
    case 'unverified':
      return <>{actor.name && <span>{actor.name}</span>}<Badge tone="neutral" title={UNVERIFIED}>unverified</Badge></>
    case 'agent':
      return <span className="font-mono text-xs" title={actor.id}>{actor.name}</span>
  }
}

/**
 * Who made a record: the credential the request came with. agentID is the agent whose record it
 * is, shown first when another credential wrote for it. `compact` is one truncated line of text,
 * for a graph node or an outline row.
 */
export function Actor({ actor, agentID = '', compact = false }: { actor: ActorRecord; agentID?: string; compact?: boolean }) {
  const agentName = useAgentName()(agentID)
  if (compact) {
    const label = actorLabel(actor, agentID, agentName)
    return <span className={clsx('max-w-[110px] truncate', !agentID && 'text-warning')} title={label}>{label}</span>
  }
  const agent = agentID && <span className="font-mono text-xs" title={agentName === agentID ? undefined : agentID}>{agentName}</span>
  if (ownersOwn(actor, agentID)) return agent
  return (
    <span className="inline-flex items-center gap-1.5">
      {agent && <>{agent}<span className="text-subtle">by</span></>}
      <Credential actor={actor} />
    </span>
  )
}

/** A person of an org by name, their user id on hover, or by id while they carry no name. */
export function Person({ id, name, self }: { id: string; name: string; self: boolean }) {
  return (
    <span className="inline-flex items-center gap-2">
      {name ? <span className="text-xs" title={id}>{name}</span> : <span className="font-mono text-xs">{id}</span>}
      {self && <Badge tone="accent">you</Badge>}
    </span>
  )
}
