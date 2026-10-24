'use client'

import { Fragment, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { shortId } from '@/lib/format'

type Block =
  | { kind: 'heading'; text: string; note?: string }
  | { kind: 'list'; ordered: boolean; items: string[] }
  | { kind: 'paragraph'; lines: string[] }

// An agent writes plain text with a light markdown habit: "# " or an ALL-CAPS line as a heading,
// "- " and "1. " lists, `code` and **bold**. Anything else is a paragraph, line breaks kept.
const listItem = /^\s*(?:([-*•])|(\d+)[.)])\s+(.*)$/
const markdownHeading = /^#{1,4}\s+(.+)$/
// Two or more words in capitals open the line, and what follows in brackets or after a colon is a
// note; or one word in capitals is the whole line.
const capsHeading = /^([A-Z][A-Z0-9'’/&-]*(?:\s+[A-Z0-9][A-Z0-9'’/&-]*)+)\s*(?:[:(]\s*(.*?)\)?\s*:?)?$|^([A-Z][A-Z'’-]+):?$/

function sentenceCase(caps: string): string {
  const lower = caps.toLowerCase()
  return lower.charAt(0).toUpperCase() + lower.slice(1)
}

export function parseProse(text: string): Block[] {
  const blocks: Block[] = []
  for (const raw of text.replace(/\r\n?/g, '\n').split('\n')) {
    const line = raw.trimEnd()
    const last = blocks[blocks.length - 1]
    if (!line.trim()) {
      if (last?.kind === 'paragraph') blocks.push({ kind: 'paragraph', lines: [] })
      continue
    }
    const md = markdownHeading.exec(line)
    if (md) {
      blocks.push({ kind: 'heading', text: md[1].replace(/\*\*/g, '') })
      continue
    }
    const caps = capsHeading.exec(line.trim())
    const capsText = caps?.[1] ?? caps?.[3]
    if (capsText && capsText.replace(/[^A-Z]/g, '').length >= 6) {
      blocks.push({ kind: 'heading', text: sentenceCase(capsText), note: caps?.[2] || undefined })
      continue
    }
    const item = listItem.exec(line)
    if (item) {
      const ordered = item[2] !== undefined
      if (last?.kind === 'list' && last.ordered === ordered) last.items.push(item[3])
      else blocks.push({ kind: 'list', ordered, items: [item[3]] })
      continue
    }
    // An indented line goes on with the list item above it.
    if (last?.kind === 'list' && /^\s+/.test(raw)) {
      last.items[last.items.length - 1] += ` ${line.trim()}`
      continue
    }
    if (last?.kind === 'paragraph') last.lines.push(line)
    else blocks.push({ kind: 'paragraph', lines: [line] })
  }
  return blocks.filter(b => b.kind !== 'paragraph' || b.lines.length > 0)
}

const inlinePattern = /(`[^`]+`|\*\*[^*]+\*\*|https?:\/\/[^\s)]+|\b[a-z]+-[0-9a-f]{32}\b|\b[0-9a-f]{40}\b)/g

/** Renders the inline marks of one line; `linkId` turns a record's id into a link to its page. */
function Inline({ text, linkId }: { text: string; linkId?: (id: string) => string | undefined }) {
  const parts = text.split(inlinePattern)
  return (
    <>
      {parts.map((part, i) => {
        if (i % 2 === 0) return <Fragment key={i}>{part}</Fragment>
        if (part.startsWith('`')) return <code key={i} className="rounded-sm bg-surface-inset px-1 font-mono text-[0.92em]">{part.slice(1, -1)}</code>
        if (part.startsWith('**')) return <strong key={i} className="font-medium text-foreground">{part.slice(2, -2)}</strong>
        if (part.startsWith('http')) return <a key={i} href={part} target="_blank" rel="noreferrer" className="break-all text-accent hover:underline">{part}</a>
        if (/^[0-9a-f]{40}$/.test(part)) return <code key={i} title={part} className="font-mono text-[0.92em]">{part.slice(0, 7)}</code>
        const href = linkId?.(part)
        return href
          ? <a key={i} href={href} title={part} className="font-mono text-[0.92em] text-accent hover:underline">{shortId(part)}</a>
          : <code key={i} title={part} className="font-mono text-[0.92em]">{shortId(part)}</code>
      })}
    </>
  )
}

interface ProseProps {
  text: string
  /** Shows about this many lines first, with the rest one click away. */
  collapsedLines?: number
  linkId?: (id: string) => string | undefined
  className?: string
}

/**
 * Text an agent or a person wrote, set for reading: a measure of about 80 characters, headings,
 * lists and inline code, and ids shortened to what tells them apart.
 */
export function Prose({ text, collapsedLines, linkId, className }: ProseProps) {
  const blocks = parseProse(text)
  const ref = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [overflows, setOverflows] = useState(false)
  useLayoutEffect(() => {
    const el = ref.current
    if (el && collapsedLines) setOverflows(el.scrollHeight > el.clientHeight + 4)
  }, [text, collapsedLines])

  const clamped = collapsedLines !== undefined && !open
  const body: ReactNode = blocks.map((b, i) => {
    if (b.kind === 'heading') {
      return (
        <h4 key={i} className="text-sm font-medium text-foreground [&:not(:first-child)]:mt-5">
          {b.text}
          {b.note && <span className="ml-2 text-xs font-normal text-muted"><Inline text={b.note} linkId={linkId} /></span>}
        </h4>
      )
    }
    if (b.kind === 'list') {
      const List = b.ordered ? 'ol' : 'ul'
      return (
        <List key={i} className={clsx('space-y-1.5 pl-5 marker:text-subtle', b.ordered ? 'list-decimal' : 'list-disc')}>
          {b.items.map((item, j) => <li key={j} className="pl-1"><Inline text={item} linkId={linkId} /></li>)}
        </List>
      )
    }
    return (
      <p key={i}>
        {b.lines.map((line, j) => <Fragment key={j}>{j > 0 && <br />}<Inline text={line} linkId={linkId} /></Fragment>)}
      </p>
    )
  })

  return (
    <div className={clsx('max-w-2xl', className)}>
      <div
        ref={ref}
        className={clsx('relative space-y-2.5 break-words text-sm leading-relaxed text-foreground', clamped && 'overflow-hidden')}
        style={clamped ? { maxHeight: `${collapsedLines * 1.625}em` } : undefined}
      >
        {body}
        {clamped && overflows && <div aria-hidden className="pointer-events-none absolute inset-x-0 bottom-0 h-12 bg-gradient-to-t from-surface to-transparent" />}
      </div>
      {collapsedLines !== undefined && (overflows || open) && (
        <button type="button" onClick={() => setOpen(!open)} className="mt-2 rounded-sm text-xs font-medium text-accent hover:underline">
          {open ? 'Show less' : 'Read all'}
        </button>
      )}
    </div>
  )
}
