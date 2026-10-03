'use client'

import NextLink from 'next/link'
import type { ComponentProps } from 'react'
import { clsx } from 'clsx'
import { useProjectHref } from '@/lib/query-state'

/** An in-app link that stays inside the selected project. */
export function Link({ href, ...props }: Omit<ComponentProps<typeof NextLink>, 'href'> & { href: string }) {
  const projectHref = useProjectHref()
  return <NextLink href={projectHref(href)} {...props} />
}

/** How a link that stands alone reads, a record's name in a table cell, for an anchor that is no in-app Link. */
export const textLinkClass = 'rounded-sm text-accent hover:underline'

/** How a link inside a sentence reads: underlined, so colour is not all that tells it from the words around it. */
export const inlineLinkClass = 'rounded-sm text-accent underline decoration-accent-edgeunderline-offset-2 hover:decoration-accent'

/** An in-app link that reads as one. `inline` is for a link inside a sentence. */
export function TextLink({ inline, className, ...props }: ComponentProps<typeof Link> & { inline?: boolean }) {
  return <Link className={clsx(inline ? inlineLinkClass : textLinkClass, className)} {...props} />
}
