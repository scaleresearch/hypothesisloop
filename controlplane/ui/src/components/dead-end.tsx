import type { ReactNode } from 'react'
import NextLink from 'next/link'
import { HOME_PATH, LANDING_PATH } from '@/lib/auth'
import { ButtonLink } from '@/components/ui/button'
import { Centered } from '@/components/ui/centered'
import { Logo } from '@/components/ui/logo'
import { Inline } from '@/components/ui/stack'

/** A page with nothing to show but why, and the ways back: to your experiments, and whatever `actions` adds. */
export function DeadEnd({ title, children, actions }: { title: string; children: ReactNode; actions?: ReactNode }) {
  return (
    <Centered wide head={<NextLink href={LANDING_PATH} className="inline-flex rounded"><Logo /></NextLink>} title={title}>
      {children}
      <Inline>
        <ButtonLink href={HOME_PATH} variant="primary">Go to your experiments</ButtonLink>
        {actions}
      </Inline>
    </Centered>
  )
}
