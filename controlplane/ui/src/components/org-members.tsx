'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import { createOrgInvite, fetchOrgInvites, fetchOrgMembers, removeOrgMember, revokeOrgInvite, setOrgMemberRole } from '@/lib/api'
import { inviteLink } from '@/lib/invite'
import { HOME_PATH } from '@/lib/auth'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import type { CreatedOrgInvite, OrgInvite, OrgMember, OrgRole } from '@/types'
import { Actor, Person } from '@/components/actor'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Select } from '@/components/ui/select'
import { Secret } from '@/components/ui/secret'
import { Table } from '@/components/ui/table'
import { Eyebrow } from '@/components/ui/text'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

const ROLES: OrgRole[] = ['owner', 'member']

const INVITE_ROLES: { value: OrgRole; label: string }[] = [
  { value: 'member', label: 'Invite as member' },
  { value: 'owner', label: 'Invite as owner' },
]

// What is about to be done to a member, held while the question is on screen.
type Change =
  | { kind: 'role'; member: OrgMember; role: OrgRole }
  | { kind: 'remove'; member: OrgMember }

function when(at: string): string {
  return new Date(at).toLocaleString()
}

/** The pending invites of the org: create one and read its link once, list them, revoke one. */
function Invites({ onError }: { onError: (message: string) => void }) {
  const { me, org } = useTenant()
  const { data, error: listError, mutate } = useSWR(['org-invites', org.id], () => fetchOrgInvites(org.id))
  const [role, setRole] = useState<OrgRole>('member')
  const [busy, setBusy] = useState(false)
  // The token exists here and nowhere else: no read of the API returns it again.
  const [created, setCreated] = useState<CreatedOrgInvite | null>(null)
  const [revoking, setRevoking] = useState<OrgInvite | null>(null)

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    onError('')
    try {
      setCreated(await createOrgInvite(org.id, role))
      await mutate()
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  async function revoke(invite: OrgInvite) {
    setRevoking(null)
    onError('')
    try {
      await revokeOrgInvite(invite.id)
      if (created?.id === invite.id) setCreated(null)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    }
    await mutate()
  }

  const invites = data?.items
  return (
    <div className="grid gap-3 border-t pt-3">
      <Eyebrow>Invites</Eyebrow>
      <form className="flex flex-wrap items-center gap-2" onSubmit={create}>
        <Select fieldSize="sm" aria-label="Role of the invited person" value={role} onChange={setRole} options={INVITE_ROLES} />
        <Button size="sm" variant="primary" type="submit" loading={busy}>Create invite link</Button>
      </form>

      {created && (
        <Secret
          label={`Invite link, joins as ${created.role}`}
          value={inviteLink(created.token)}
          note={<>
            Send it to the person yourself: the platform sends no email. Whoever opens it and signs in
            joins {org.name} as {created.role === 'owner' ? 'an owner' : 'a member'}. It works once, until {when(created.expires_at)}.
            This is the only time it is shown: if you lose it, revoke the invite and create another.
          </>}
          dismiss="I have sent it"
          onDismiss={() => setCreated(null)}
        />
      )}

      {listError && <ErrorMessage>Could not load invites: {listError.message}</ErrorMessage>}
      {!invites && !listError && <Loading />}
      {invites && invites.length === 0 && <p className="text-xs text-subtle">No pending invites.</p>}
      {invites && invites.length > 0 && (
        <Table aria-label="Pending invites">
          <thead>
            <tr><th>Created by</th><th>Role</th><th>Created</th><th>Expires</th><th /></tr>
          </thead>
          <tbody>
            {invites.map(invite => (
              <tr key={invite.id}>
                <td><Actor actor={invite.actor} self={invite.actor.kind === 'user' && invite.actor.id === me.user_id} /></td>
                <td className="text-xs">{invite.role}</td>
                <td className="text-xs text-muted">{when(invite.created_at)}</td>
                <td className="text-xs text-muted">{when(invite.expires_at)}</td>
                <td className="text-right"><Button size="sm" variant="danger" onClick={() => setRevoking(invite)}>Revoke</Button></td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {data && data.total > data.items.length && (
        <p className="text-xs text-subtle">Showing the {data.items.length} newest of {data.total} pending invites.</p>
      )}
      {revoking && (
        <ConfirmDialog
          open
          title={`Revoke the invite ${revoking.actor.name} created ${when(revoking.created_at)}?`}
          confirmLabel="Revoke"
          onConfirm={() => revoke(revoking)}
          onCancel={() => setRevoking(null)}
        >
          Its link stops working at once. This cannot be undone.
        </ConfirmDialog>
      )}
    </div>
  )
}

/**
 * The people of the org and, for an owner, the controls over them: roles, removal, invites.
 */
export function OrgMembers() {
  const { me, org, administers } = useTenant()
  const router = useRouter()
  const refreshMe = useRefreshMe()
  const { data, error: listError, mutate } = useSWR(['org-members', org.id], () => fetchOrgMembers(org.id))
  const [error, setError] = useState('')
  const [change, setChange] = useState<Change | null>(null)

  async function apply(change: Change) {
    const { member } = change
    const self = member.user_id === me.user_id
    setChange(null)
    setError('')
    try {
      if (change.kind === 'role') {
        await setOrgMemberRole(org.id, member.user_id, change.role)
        if (self) await refreshMe()
      } else {
        await removeOrgMember(org.id, member.user_id)
        if (self) {
          // The org is no longer theirs to see, so the URL stops naming its project before the account is reread.
          router.replace(HOME_PATH)
          await refreshMe()
          return
        }
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
    await mutate()
  }

  const members = data?.items
  return (
    <div className="grid gap-3">
      <p className="text-sm text-muted">
        An owner changes everything in {org.name}. A member reads everything in it and runs experiments.
        {!administers && ' Only an owner can invite people, change a role or remove someone.'}
      </p>
      {error && <ErrorMessage>{error}</ErrorMessage>}
      {listError && <ErrorMessage>Could not load members: {listError.message}</ErrorMessage>}
      {!members && !listError && <Loading />}
      {members && members.length === 0 && <EmptyState>No members.</EmptyState>}
      {members && members.length > 0 && (
        <Table aria-label="Members">
          <thead>
            <tr><th>Member</th><th>Role</th><th>Joined</th><th /></tr>
          </thead>
          <tbody>
            {members.map(member => {
              const self = member.user_id === me.user_id
              return (
                <tr key={member.user_id}>
                  <td><Person id={member.user_id} name={member.name} self={self} /></td>
                  <td className="text-xs">
                    {administers
                      ? (
                        <Select
                          fieldSize="sm"
                          aria-label={`Role of ${member.name || member.user_id}`}
                          value={member.role}
                          onChange={role => setChange({ kind: 'role', member, role })}
                          options={ROLES.map(role => ({ value: role, label: role }))}
                        />
                      )
                      : member.role}
                  </td>
                  <td className="text-xs text-muted">{when(member.joined_at)}</td>
                  <td className="text-right">
                    {self
                      ? <Button size="sm" variant="danger" onClick={() => setChange({ kind: 'remove', member })}>Leave</Button>
                      : administers && <Button size="sm" variant="danger" onClick={() => setChange({ kind: 'remove', member })}>Remove</Button>}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </Table>
      )}
      {data && data.total > data.items.length && (
        <p className="text-xs text-subtle">Showing the first {data.items.length} of {data.total} members.</p>
      )}
      {change && <Question change={change} onConfirm={() => apply(change)} onCancel={() => setChange(null)} />}
      {administers && <Invites onError={setError} />}
    </div>
  )
}

function Question({ change, onConfirm, onCancel }: { change: Change; onConfirm: () => void; onCancel: () => void }) {
  const { me, org } = useTenant()
  const self = change.member.user_id === me.user_id
  const id = change.member.name || change.member.user_id
  if (change.kind === 'role') {
    const demotes = change.role === 'member'
    return (
      <ConfirmDialog
        open
        title={`Make ${self ? 'yourself' : id} ${demotes ? 'a member' : 'an owner'} of ${org.name}?`}
        confirmLabel={demotes ? 'Make member' : 'Make owner'}
        variant={demotes ? 'danger' : 'primary'}
        onConfirm={onConfirm}
        onCancel={onCancel}
      >
        {demotes
          ? `${self ? 'You keep' : 'They keep'} reading everything and running experiments, and lose what only an owner holds: ${self ? 'your' : 'their'} API keys of the whole organization stop working and the invites ${self ? 'you' : 'they'} made are revoked.`
          : 'An owner changes everything in the organization: its members, invites, API keys, clusters, projects and repositories.'}
      </ConfirmDialog>
    )
  }
  return (
    <ConfirmDialog
      open
      title={self ? `Leave ${org.name}?` : `Remove ${id} from ${org.name}?`}
      confirmLabel={self ? 'Leave' : 'Remove'}
      onConfirm={onConfirm}
      onCancel={onCancel}
    >
      {self
        ? 'You lose access to it at once: your API keys in it stop working and the invites you made are revoked. What you wrote stays. Coming back takes a new invite.'
        : 'They lose access at once: their API keys in it stop working and the invites they made are revoked. What they wrote stays.'}
    </ConfirmDialog>
  )
}
