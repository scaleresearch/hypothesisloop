'use client'

import NextLink from 'next/link'
import type { ComponentProps } from 'react'
import { useProjectHref } from '@/lib/query-state'

/** An in-app link that stays inside the selected project. */
export function Link({ href, ...props }: Omit<ComponentProps<typeof NextLink>, 'href'> & { href: string }) {
  const projectHref = useProjectHref()
  return <NextLink href={projectHref(href)} {...props} />
}
