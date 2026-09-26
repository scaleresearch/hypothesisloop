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

/** How a link in running text or a table cell reads, for an anchor that is no in-app Link. */
export const textLinkClass = 'rounded-sm text-accent hover:underline'

/** An in-app link that reads as one: the name of a record in a table, a way on in a sentence. */
export function TextLink({ className, ...props }: ComponentProps<typeof Link>) {
  return <Link className={clsx(textLinkClass, className)} {...props} />
}
