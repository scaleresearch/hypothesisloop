import type { ReactNode } from 'react'
import { Table } from '@/components/ui/table'

/** A record's fields, a label beside each value. Details go inside. */
export function DetailList({ children }: { children: ReactNode }) {
  return <Table><tbody>{children}</tbody></Table>
}

/** One field of a DetailList. `mono` sets a value that is an id, a ref or a figure. */
export function Detail({ label, mono = true, children }: { label: string; mono?: boolean; children: ReactNode }) {
  return (
    <tr>
      <td className="w-44 whitespace-nowrap pr-3 align-top text-xs text-muted">{label}</td>
      <td className={mono ? 'font-mono text-xs' : 'text-xs'}>{children ?? '—'}</td>
    </tr>
  )
}
