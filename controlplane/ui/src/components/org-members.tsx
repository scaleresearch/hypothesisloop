'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import { createOrgInvite, fetchOrgInvites, fetchOrgMembers, removeOrgMember, revokeOrgInvite, setOrgMemberRole } from '@/lib/api/accounts'
import { errorMessage } from '@/lib/errors'
import { inviteLink } from '@/lib/invite'
import { HOME_PATH } from '@/lib/auth'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import { Actor, Person } from '@/components/actor'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Select } from '@/components/ui/select'
import { Secret } from '@/components/ui/secret'
import { Table } from '@/components/ui/table'
import { Eyebrow, Text } from '@/components/ui/text'
import { Inline, Stack } from '@/components/ui/stack'
import { EmptyState } from '@/components/ui/empty-state'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { keys } from '@/lib/keys'
import type { CreatedInvite, OrgInvite, OrgMember, OrgRole } from '@/types/api'

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
  const { data, error: listError, mutate } = useSWR(keys.orgInvites(org.id), () => fetchOrgInvites(org.id))
  const [role, setRole] = useState<OrgRole>('member')
  const [busy, setBusy] = useState(false)
  // The token exists here and nowhere else: no read of the API returns it again.
  const [created, setCreated] = useState<CreatedInvite | null>(null)
  const [revoking, setRevoking] = useState<OrgInvite | null>(null)

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    onError('')
    try {
      setCreated(await createOrgInvite(org.id, role))
      await mutate()
    } catch (e) {
      onError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  async function revoke(invite: OrgInvite) {
    await revokeOrgInvite(invite.id)
    if (created?.id === invite.id) setCreated(null)
    await mutate()
  }

  const invites = data?.items
  return (
    <Stack className="border-t pt-3">
      <Eyebrow>Invites</Eyebrow>
      <Inline as="form" onSubmit={create}>
        <Select fieldSize="sm" aria-label="Role of the invited person" value={role} onChange={setRole} options={INVITE_ROLES} />
        <Button size="sm" variant="primary" type="submit" loading={busy}>Create invite link</Button>
      </Inline>

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
      {invites && invites.length === 0 && <Text variant="hint">No pending invites.</Text>}
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
        <Text variant="hint">Showing the {data.items.length} newest of {data.total} pending invites.</Text>
      )}
      {revoking && (
        <ConfirmDialog
          open
          title={`Revoke the invite ${revoking.actor.name} created ${when(revoking.created_at)}?`}
          confirmLabel="Revoke"
          onConfirm={() => revoke(revoking)}
          onClose={() => setRevoking(null)}
        >
          Its link stops working at once. This cannot be undone.
        </ConfirmDialog>
      )}
    </Stack>
  )
}

/**
 * The people of the org and, for an owner, the controls over them: roles, removal, invites.
 */
export function OrgMembers() {
  const { me, org } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const router = useRouter()
  const refreshMe = useRefreshMe()
  const { data, error: listError, mutate } = useSWR(keys.orgMembers(org.id), () => fetchOrgMembers(org.id))
  const [error, setError] = useState('')
  const [change, setChange] = useState<Change | null>(null)

  async function apply(change: Change) {
    const { member } = change
    const self = member.user_id === me.user_id
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
    await mutate()
  }

  const members = data?.items
  const owners = members?.filter(m => m.role === 'owner').length
  return (
    <Stack>
      <Text tone="muted">
        An owner changes everything in {org.name}. A member reads everything in it and runs experiments.
        {!editsOrg && ' Only an owner can invite people, change a role or remove someone.'}
      </Text>
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
                    {editsOrg
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
                    {self && member.role === 'owner' && owners === 1
                      ? (
                        <Inline className="justify-end">
                          <Text as="span" variant="hint">Make someone else an owner before you leave</Text>
                          <Button size="sm" variant="danger" disabled>Leave</Button>
                        </Inline>
                      )
                      : self
                      ? <Button size="sm" variant="danger" onClick={() => setChange({ kind: 'remove', member })}>Leave</Button>
                      : editsOrg && <Button size="sm" variant="danger" onClick={() => setChange({ kind: 'remove', member })}>Remove</Button>}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </Table>
      )}
      {data && data.total > data.items.length && (
        <Text variant="hint">Showing the first {data.items.length} of {data.total} members.</Text>
      )}
      {change && <Question change={change} onConfirm={() => apply(change)} onClose={() => setChange(null)} />}
      {editsOrg && <Invites onError={setError} />}
    </Stack>
  )
}

function Question({ change, onConfirm, onClose }: { change: Change; onConfirm: () => Promise<void>; onClose: () => void }) {
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
        onClose={onClose}
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
      onClose={onClose}
    >
      {self
        ? 'You lose access to it at once: your API keys in it stop working and the invites you made are revoked. Coming back takes a new invite.'
        : 'They lose access at once: their API keys in it stop working and the invites they made are revoked.'}
    </ConfirmDialog>
  )
}
