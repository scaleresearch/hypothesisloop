'use client'

import NextLink from 'next/link'
import type { ComponentProps } from 'react'
import { clsx } from 'clsx'

/** An in-app link. Its path names what it opens, so it reads the same to whoever it is sent to. */
export function Link(props: Omit<ComponentProps<typeof NextLink>, 'href'> & { href: string }) {
  return <NextLink {...props} />
}

/** How a link that stands alone reads, a record's name in a table cell, for an anchor that is no in-app Link. */
export const textLinkClass = 'rounded-sm text-accent hover:underline'

/** How a link inside a sentence reads: underlined, so colour is not all that tells it from the words around it. */
export const inlineLinkClass = 'rounded-sm text-accent underline decoration-accent-edge underline-offset-2 hover:decoration-accent'

/** An in-app link that reads as one. `inline` is for a link inside a sentence. */
export function TextLink({ inline, className, ...props }: ComponentProps<typeof Link> & { inline?: boolean }) {
  return <Link className={clsx(inline ? inlineLinkClass : textLinkClass, className)} {...props} />
}
