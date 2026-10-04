'use client'

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useProjectRouter } from '@/lib/query-state'
import {
  FlaskConical, ListTree, Server, LineChart, Lightbulb, Search, CornerDownLeft,
  ArrowUp, ArrowDown, Hash, Zap, type LucideIcon,
} from 'lucide-react'
import { clsx } from 'clsx'
import { fetchExperiment, fetchPlatformExperiment } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline } from '@/components/ui/stack'
import { Kbd, Text } from '@/components/ui/text'

type CommandItem = {
  id: string
  label: string
  sub?: string
  icon: LucideIcon
  keywords?: string
  action: () => void
}

const DESTINATIONS: (Omit<CommandItem, 'action'> & { staffOnly?: boolean })[] = [
  { id: 'nav-pe',   label: 'Experiments',          sub: 'Go to page', icon: FlaskConical, keywords: 'experiments campaigns' },
  { id: 'nav-hyp',  label: 'Hypotheses',           sub: 'Go to page', icon: Lightbulb,    keywords: 'hypothesis graph claims findings' },
  { id: 'nav-jobs', label: 'Jobs',                 sub: 'Go to page', icon: ListTree,     keywords: 'jobs runs workloads' },
  { id: 'nav-clu',  label: 'Compute Resources',    sub: 'Go to page', icon: Server,       keywords: 'clusters compute accelerator' },
  { id: 'nav-stat', label: 'Statistics',           sub: 'Go to page', icon: LineChart,    keywords: 'dashboard scheduler quality evictions completion rate', staffOnly: true },
]

const DEST_HREF: Record<string, string> = {
  'nav-pe': '/platform-experiments',
  'nav-hyp': '/hypotheses',
  'nav-jobs': '/jobs',
  'nav-clu': '/cluster',
  'nav-stat': '/statistics',
}

/**
 * Global ⌘K / Ctrl+K command palette. It's the primary way both human
 * operators and LLM agents jump anywhere in the app without hunting through
 * nav: fuzzy destination search plus "paste an ID, land on the record."
 */
export function CommandPalette() {
  const router = useProjectRouter()
  const { staff } = useTenant()
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(0)
  const [resolving, setResolving] = useState(false)
  const [idResult, setIdResult] = useState<CommandItem | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const close = useCallback(() => {
    setOpen(false)
    setQuery('')
    setIdResult(null)
    setActiveIndex(0)
  }, [])

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      const isMeta = e.metaKey || e.ctrlKey
      if (isMeta && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen(o => !o)
        return
      }
      if (e.key === 'Escape' && open) {
        e.preventDefault()
        close()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [open, close])

  useEffect(() => {
    if (open) requestAnimationFrame(() => inputRef.current?.focus())
  }, [open])

  // A bare UUID-looking query resolves against jobs, then platform experiments.
  useEffect(() => {
    const q = query.trim()
    const looksLikeId = /^[0-9a-f]{6,}(-[0-9a-f]{4,}){0,4}$/i.test(q)
    if (!looksLikeId) {
      setIdResult(null)
      return
    }
    let cancelled = false
    setResolving(true)
    fetchExperiment(q)
      .then(exp => {
        if (cancelled) return
        setIdResult({
          id: 'resolved-job',
          label: `Job ${q.slice(0, 12)}…`,
          sub: `${exp.status} · open job`,
          icon: Hash,
          action: () => { router.push(`/jobs/${q}`); close() },
        })
      })
      .catch(() =>
        fetchPlatformExperiment(q)
          .then(pe => {
            if (cancelled) return
            setIdResult({
              id: 'resolved-pe',
              label: pe.name || `Platform experiment ${q.slice(0, 12)}…`,
              sub: 'open experiment',
              icon: Hash,
              action: () => { router.push(`/platform-experiments/${q}`); close() },
            })
          })
          .catch(() => { if (!cancelled) setIdResult(null) }),
      )
      .finally(() => { if (!cancelled) setResolving(false) })
    return () => { cancelled = true }
  }, [query, router, close])

  const items: CommandItem[] = useMemo(() => {
    const base: CommandItem[] = DESTINATIONS.filter(d => staff || !d.staffOnly).map(d => ({
      ...d,
      action: () => { router.push(DEST_HREF[d.id]); close() },
    }))
    const q = query.trim().toLowerCase()
    const filtered = q
      ? base.filter(i => (i.label + ' ' + (i.sub ?? '') + ' ' + (i.keywords ?? '')).toLowerCase().includes(q))
      : base
    return idResult ? [idResult, ...filtered] : filtered
  }, [query, router, close, idResult, staff])

  useEffect(() => { setActiveIndex(0) }, [items.length, query])

  function onKeyDownInList(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActiveIndex(i => Math.min(i + 1, items.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActiveIndex(i => Math.max(i - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      items[activeIndex]?.action()
    }
  }

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-scrim pt-drop backdrop-blur-sm" onClick={close}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Jump to a page or record"
        className="w-full max-w-xl overflow-hidden rounded-lg border border-border-strong bg-surface-raised shadow-lg"
        onClick={e => e.stopPropagation()}
      >
        <div className="flex items-center gap-2.5 border-b px-4 py-3.5">
          <Search size={16} strokeWidth={2} className="flex-none text-muted" />
          <Input
            ref={inputRef}
            bare
            className="flex-1 text-base"
            aria-label="Search"
            placeholder="Jump to a page, or paste a job / experiment ID…"
            value={query}
            onChange={e => setQuery(e.target.value)}
            onKeyDown={onKeyDownInList}
          />
          {resolving && <Text as="span" variant="meta" mono tone="accent" className="whitespace-nowrap">resolving…</Text>}
          <Kbd>ESC</Kbd>
        </div>
        <div className="max-h-80 overflow-y-auto p-1.5" role="listbox" aria-label="Results">
          {items.length === 0 && (
            <EmptyState>No matches. Try a page name or paste a full job/experiment ID.</EmptyState>
          )}
          {items.map((item, i) => {
            const Icon = item.icon
            const active = i === activeIndex
            return (
              <div
                key={item.id}
                role="option"
                aria-selected={active}
                className={clsx('flex cursor-pointer items-center gap-2.5 rounded px-2.5 py-2', active ? 'bg-highlight text-foreground' : 'text-muted')}
                onMouseEnter={() => setActiveIndex(i)}
                onClick={item.action}
              >
                <Icon size={15} strokeWidth={2} className={clsx('flex-none', active && 'text-accent')} />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-medium">{item.label}</div>
                  {item.sub && <Text as="div" variant="hint">{item.sub}</Text>}
                </div>
                {active && <CornerDownLeft size={13} strokeWidth={2} className="flex-none text-accent" />}
              </div>
            )
          })}
        </div>
        <Inline gap="lg" nowrap className="border-t bg-surface-inset px-4 py-2 text-2xs text-subtle">
          <Inline gap="xs" nowrap><ArrowUp size={11} /><ArrowDown size={11} /> navigate</Inline>
          <Inline gap="xs" nowrap><CornerDownLeft size={11} /> select</Inline>
          <Inline gap="xs" nowrap className="ml-auto"><Zap size={11} /> agent- and human-readable IDs both work</Inline>
        </Inline>
      </div>
    </div>
  )
}

/** Small pill shown in the nav / top bar that opens the palette — mirrors the ⌘K hint. */
export function CommandPaletteTrigger() {
  return (
    <Button
      size="sm"
      className="w-full"
      onClick={() => window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true }))}
    >
      <Search size={13} strokeWidth={2} className="flex-none text-subtle" />
      <span className="flex-1 text-left font-normal text-subtle">Search or jump to…</span>
      <Kbd>⌘K</Kbd>
    </Button>
  )
}
