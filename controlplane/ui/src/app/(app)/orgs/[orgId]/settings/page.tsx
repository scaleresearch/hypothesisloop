'use client'

import { useState } from 'react'
import { createProject, renameOrg, renameProject } from '@/lib/api/accounts'
import { GITHUB_CARD } from '@/lib/github-connect'
import { orgPath } from '@/lib/routes'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import { APIKeys } from '@/components/api-keys'
import { OrgGitHub } from '@/components/org-github'
import { OrgMembers } from '@/components/org-members'
import { YourOrgs } from '@/components/your-orgs'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { TextLink } from '@/components/ui/link'
import { errorMessage } from '@/lib/errors'

/** A name with a Save beside it. `save` rejects with the API's own reason, which is shown. */
function NameForm({ label, initial, action, placeholder, save }: {
  label: string
  initial: string
  action: string
  placeholder?: string
  save: (name: string) => Promise<unknown>
}) {
  const [name, setName] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await save(name.trim())
      if (initial === '') setName('')
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Inline as="form" onSubmit={submit}>
      <Input
        fieldSize="sm"
        className="w-64"
        aria-label={label}
        placeholder={placeholder}
        value={name}
        onChange={e => setName(e.target.value)}
      />
      <Button size="sm" type="submit" disabled={busy || name.trim() === '' || name.trim() === initial}>
        {busy ? 'Saving…' : action}
      </Button>
      {error && <InlineError>{error}</InlineError>}
    </Inline>
  )
}

function Organization() {
  const { org } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Section title="Organization">
      <Stack gap="sm">
        <NameForm
          // Keyed by the stored name, so a rename made in another tab resets the field.
          key={org.name}
          label="Organization name"
          initial={org.name}
          action="Rename"
          save={async name => { await renameOrg(org.id, name); await refreshMe() }}
        />
        <Text variant="meta" tone="subtle" mono>{org.id}</Text>
      </Stack>
    </Section>
  )
}

function Projects() {
  const { me, org, project } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Section title="Projects">
      <Stack>
        {me.projects.filter(p => p.org_id === org.id).map(p => (
          <Stack key={p.id} gap="xs">
            <NameForm
              key={p.name}
              label={`Name of project ${p.name}`}
              initial={p.name}
              action="Rename"
              save={async name => { await renameProject(p.id, name); await refreshMe() }}
            />
            <Text as="span" variant="meta" tone="subtle" mono>
              {p.id}{p.id === project.id && ' · selected'}
            </Text>
          </Stack>
        ))}
        <div className="border-t pt-3">
          <NameForm
            label="New project name"
            initial=""
            action="Create project"
            placeholder="new project name"
            save={async name => { await createProject(org.id, name); await refreshMe() }}
          />
        </div>
      </Stack>
    </Section>
  )
}

export default function SettingsPage() {
  const { me, org } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const where = org.role ? `, where you are ${org.role}` : ''
  return (
    <Page title="Settings" description={`Settings of ${org.name}${where}.`}>
      <Section title="Your organizations" note={me.orgs.filter(o => o.role).length} scrollX>
        <YourOrgs />
      </Section>

      {!editsOrg && (
        <Text tone="muted">
          You are a member of {org.name}. Only an owner can rename it or manage its projects and GitHub accounts.
        </Text>
      )}

      {editsOrg && (
        <>
          <Organization />
          <Projects />
        </>
      )}

      {org.actions.includes('create_api_key') && (
        <Section title="API keys" scrollX>
          <APIKeys />
        </Section>
      )}

      <Section title="Members" scrollX>
        <OrgMembers />
      </Section>

      <Text tone="muted">
        The clusters of {org.name} are managed on <TextLink href={orgPath(org.id, 'clusters')}>Compute Resources</TextLink>.
      </Text>

      <Section id={GITHUB_CARD} title="GitHub" className="scroll-mt-4" scrollX>
        <OrgGitHub />
      </Section>
    </Page>
  )
}
