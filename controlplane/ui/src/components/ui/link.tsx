'use client'

import NextLink from 'next/link'
import type { ComponentProps } from 'react'
import { useProjectHref } from '@/lib/query-state'

/** An in-app link that stays inside the selected project, and inside an example unless it leaves it. */
export function Link({ href, leaveExample, ...props }: Omit<ComponentProps<typeof NextLink>, 'href'> & { href: string; leaveExample?: boolean }) {
  const projectHref = useProjectHref(leaveExample)
  return <NextLink href={projectHref(href)} {...props} />
}
