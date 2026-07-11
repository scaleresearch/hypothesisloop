'use client'

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useRouter } from 'next/navigation'
import {
  FlaskConical, Bot, ListTree, Server, LineChart, Search, CornerDownLeft,
  ArrowUp, ArrowDown, Hash, Zap, type LucideIcon,
} from 'lucide-react'
import { fetchExperiment, fetchPlatformExperiment } from '@/lib/api'

type CommandItem = {
  id: string
  label: string
  sub?: string
  icon: LucideIcon
  keywords?: string
  action: () => void
}

const DESTINATIONS: Omit<CommandItem, 'action'>[] = [
  { id: 'nav-pe',   label: 'Platform Experiments', sub: 'Go to page', icon: FlaskConical, keywords: 'experiments campaigns' },
  { id: 'nav-agt',  label: 'Research Agents',      sub: 'Go to page', icon: Bot,          keywords: 'agents quota balances' },
  { id: 'nav-jobs', label: 'Jobs',                 sub: 'Go to page', icon: ListTree,     keywords: 'jobs runs workloads' },
  { id: 'nav-clu',  label: 'Compute Resources',    sub: 'Go to page', icon: Server,       keywords: 'clusters compute gpu' },
  { id: 'nav-dash', label: 'Scheduler Quality',    sub: 'Go to page', icon: LineChart,    keywords: 'dashboard metrics quality yield' },
]

const DEST_HREF: Record<string, string> = {
  'nav-pe': '/platform-experiments',
  'nav-agt': '/agents',
  'nav-jobs': '/jobs',
  'nav-clu': '/cluster',
  'nav-dash': '/dashboard',
}

/**
 * Global ⌘K / Ctrl+K command palette. It's the primary way both human
 * operators and LLM agents jump anywhere in the app without hunting through
 * nav: fuzzy destination search plus "paste an ID, land on the record."
 */
export function CommandPalette() {
  const router = useRouter()
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
              sub: 'open platform experiment',
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
    const base: CommandItem[] = DESTINATIONS.map(d => ({
      ...d,
      action: () => { router.push(DEST_HREF[d.id]); close() },
    }))
    const q = query.trim().toLowerCase()
    const filtered = q
      ? base.filter(i => (i.label + ' ' + (i.sub ?? '') + ' ' + (i.keywords ?? '')).toLowerCase().includes(q))
      : base
    return idResult ? [idResult, ...filtered] : filtered
  }, [query, router, close, idResult])

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
    <div className="cmdk-overlay" onClick={close}>
      <div className="cmdk-panel" onClick={e => e.stopPropagation()}>
        <div className="cmdk-input-row">
          <Search size={16} strokeWidth={2} className="cmdk-search-icon" />
          <input
            ref={inputRef}
            className="cmdk-input"
            placeholder="Jump to a page, or paste a job / experiment ID…"
            value={query}
            onChange={e => setQuery(e.target.value)}
            onKeyDown={onKeyDownInList}
          />
          {resolving && <span className="cmdk-resolving">resolving…</span>}
          <kbd className="cmdk-esc">ESC</kbd>
        </div>
        <div className="cmdk-list">
          {items.length === 0 && (
            <div className="cmdk-empty">No matches. Try a page name or paste a full job/experiment ID.</div>
          )}
          {items.map((item, i) => {
            const Icon = item.icon
            return (
              <div
                key={item.id}
                className={`cmdk-item${i === activeIndex ? ' active' : ''}`}
                onMouseEnter={() => setActiveIndex(i)}
                onClick={item.action}
              >
                <Icon size={15} strokeWidth={2} />
                <div className="cmdk-item-text">
                  <div className="cmdk-item-label">{item.label}</div>
                  {item.sub && <div className="cmdk-item-sub">{item.sub}</div>}
                </div>
                {i === activeIndex && <CornerDownLeft size={13} strokeWidth={2} className="cmdk-item-enter" />}
              </div>
            )
          })}
        </div>
        <div className="cmdk-footer">
          <span><ArrowUp size={11} /><ArrowDown size={11} /> navigate</span>
          <span><CornerDownLeft size={11} /> select</span>
          <span className="cmdk-footer-agent"><Zap size={11} /> agent- and human-readable IDs both work</span>
        </div>
      </div>
    </div>
  )
}

/** Small pill shown in the nav / top bar that opens the palette — mirrors the ⌘K hint. */
export function CommandPaletteTrigger() {
  return (
    <button
      className="cmdk-trigger"
      onClick={() => window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true }))}
    >
      <Search size={13} strokeWidth={2} />
      <span>Search or jump to…</span>
      <kbd>⌘K</kbd>
    </button>
  )
}
