'use client'

import { formatDate } from '@/lib/format'
import { orgPath } from '@/lib/routes'
import { orgLabels, useTenant } from '@/lib/tenant'
import { Badge } from '@/components/ui/badge'
import { ButtonLink } from '@/components/ui/button'
import { Table, num, quiet } from '@/components/ui/table'
import { Text } from '@/components/ui/text'

/**
 * Every org the caller belongs to, with their role in it, the one these settings are of, and a
 * switch to any other. Staff are listed every org in the switcher; here only their own.
 */
export function YourOrgs() {
  const { me, org } = useTenant()
  const own = me.orgs.filter(o => o.role)
  const labels = orgLabels(me)
  if (own.length === 0) return <Text tone="muted">You belong to no organization. As staff you reach every one from the switcher.</Text>
  return (
    <Table aria-label="Your organizations">
      <thead>
        <tr><th>Organization</th><th>Your role</th><th className="text-right">Projects</th><th>Created</th><th /></tr>
      </thead>
      <tbody>
        {own.map(o => {
          const current = o.id === org.id
          const label = labels.get(o.id) as string
          return (
            <tr key={o.id} aria-current={current || undefined}>
              <td>
                <div className="font-medium">{label}</div>
                <div className={quiet}>{o.id}</div>
              </td>
              <td><Badge tone={o.role === 'owner' ? 'accent' : 'neutral'}>{o.role}</Badge></td>
              <td className={num}>{me.projects.filter(p => p.org_id === o.id).length}</td>
              <td className="text-xs text-muted">{formatDate(o.created_at)}</td>
              <td className="text-right">
                {current
                  ? <Badge tone="success">Current</Badge>
                  : <ButtonLink size="sm" href={orgPath(o.id, 'settings')} aria-label={`Switch to ${label}`}>Switch</ButtonLink>}
              </td>
            </tr>
          )
        })}
      </tbody>
    </Table>
  )
}
