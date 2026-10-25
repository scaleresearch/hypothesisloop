import { Fragment } from 'react'
import { ChevronDown, ChevronUp, GripVertical } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Stack } from '@/components/ui/stack'
import { InfoTip } from '@/components/ui/info-tip'
import { Code, Text } from '@/components/ui/text'
import type { ListedCluster } from '@/types/api'

/** The buttons that move one of the org's own clusters up or down its order, beside the grip a row is dragged by. */
export function MoveButtons({ cluster, index, of, onMove, disabled }: { cluster: ListedCluster; index: number; of: number; onMove: (to: number) => void; disabled: boolean }) {
  return (
    <span className="flex items-center gap-1">
      <GripVertical aria-hidden="true" size={14} className="cursor-grab text-subtle" />
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} up`} disabled={disabled || index === 0} onClick={() => onMove(index - 1)}>
        <ChevronUp aria-hidden="true" size={14} />
      </Button>
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} down`} disabled={disabled || index === of - 1} onClick={() => onMove(index + 1)}>
        <ChevronDown aria-hidden="true" size={14} />
      </Button>
    </span>
  )
}

/**
 * Where a job of the org tries the cluster: its place, which it may share with others the job
 * tries together, or that it is skipped while turned off.
 */
export function Place({ place, shared }: { place?: number; shared: boolean }) {
  if (!place) return <Text as="span" variant="note">Off</Text>
  return (
    <Stack gap="2xs" start>
      <Code>{place}</Code>
      {shared && <Text as="div" variant="meta" className="whitespace-nowrap">tied, most room first</Text>}
    </Stack>
  )
}

/**
 * Where a job of the org goes right now: the first place with a connected cluster. A job that does
 * not fit there moves down the order.
 */
export function NextPlacement({ clusters }: { clusters: ListedCluster[] }) {
  const placed = clusters.filter(c => c.place)
  if (placed.length === 0) return <Text variant="note">All clusters are off. Jobs wait in the queue.</Text>
  const first = Math.min(...placed.filter(c => c.connected).map(c => c.place!))
  if (!Number.isFinite(first)) return <Text variant="note">No cluster in use is connected. Jobs wait in the queue.</Text>
  const names = placed.filter(c => c.place === first && c.connected).map(c => c.name)
  return (
    <Text variant="note">
      Next job goes to{' '}
      {names.map((n, i) => <Fragment key={n}>{i > 0 && ' or '}<Code>{n}</Code></Fragment>)}
      <InfoTip>Jobs go to the first connected cluster in the order below that has room. A job that does not fit moves down the order.</InfoTip>
    </Text>
  )
}
