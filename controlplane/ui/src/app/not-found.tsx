import { DeadEnd } from '@/components/dead-end'
import { Text } from '@/components/ui/text'

export default function NotFound() {
  return (
    <DeadEnd title="Page not found">
      <Text tone="muted">There is no page at this address. It may have moved, or the link was mistyped.</Text>
    </DeadEnd>
  )
}
