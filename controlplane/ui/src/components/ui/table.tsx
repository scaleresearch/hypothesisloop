'use client'

import { createContext, useContext } from 'react'
import type { HTMLAttributes, MouseEvent, ReactNode, TableHTMLAttributes, ThHTMLAttributes } from 'react'
import type { Header } from '@tanstack/react-table'
import { clsx } from 'clsx'
import NextLink from 'next/link'
import { useRouter } from 'next/navigation'
import { textLinkClass } from '@/components/ui/link'

/** The data table. Rows and cells inside are plain thead, tbody, tr, th and td, styled by .data-table in globals.css. */
export function Table({ className, ...props }: TableHTMLAttributes<HTMLTableElement>) {
  return <table className={clsx('data-table w-full border-collapse text-sm', className)} {...props} />
}

/** A cell or header of figures: right-aligned, digits of one width so a column lines up. */
export const num = 'text-right font-mono text-xs tabular-nums'

/** A cell of an id or a time, the quiet columns of a row. */
export const quiet = 'whitespace-nowrap font-mono text-2xs text-subtle'

const RowHref = createContext<string | null>(null)

// A click on these inside a row is theirs, never the row's.
const INTERACTIVE = 'a, button, input, select, textarea, label, summary, [role="button"]'

/**
 * A row that opens the record it lists. It stays a row of its table: the way in for a keyboard,
 * a screen reader, a middle click or a new tab is the RowAnchor in one of its cells, and a click
 * anywhere else on the row opens the same address, in a new tab with Ctrl or Cmd held.
 */
export function LinkTr({ href, className, ...props }: HTMLAttributes<HTMLTableRowElement> & { href: string }) {
  const router = useRouter()
  function open(e: MouseEvent<HTMLTableRowElement>) {
    if ((e.target as Element).closest(INTERACTIVE) || window.getSelection()?.toString()) return
    if (e.metaKey || e.ctrlKey || e.button === 1) window.open(href, '_blank', 'noopener')
    else if (e.button === 0) router.push(href)
  }
  return (
    <RowHref.Provider value={href}>
      <tr className={clsx('cursor-pointer', className)} onClick={open} onAuxClick={open} {...props} />
    </RowHref.Provider>
  )
}

/** The real link to what its LinkTr opens, for the cell that names the record. */
export function RowAnchor({ className, children, title }: { className?: string; children: ReactNode; title?: string }) {
  const href = useContext(RowHref)
  if (href === null) throw new Error('RowAnchor stands in a LinkTr')
  return <NextLink href={href} className={clsx(textLinkClass, className)} title={title}>{children}</NextLink>
}

interface SortableThProps extends ThHTMLAttributes<HTMLTableCellElement> {
  sorted: false | 'asc' | 'desc'
  onSort: () => void
}

/** A column header that sorts by its column on click or Enter. */
export function SortableTh({ sorted, onSort, className, children, ...props }: SortableThProps) {
  return (
    <th
      aria-sort={sorted === 'asc' ? 'ascending' : sorted === 'desc' ? 'descending' : 'none'}
      tabIndex={0}
      className={clsx('cursor-pointer select-none hover:text-foreground', className)}
      onClick={onSort}
      onKeyDown={e => {
        if (e.key !== 'Enter' && e.key !== ' ') return
        e.preventDefault()
        onSort()
      }}
      {...props}
    >
      {children}
      {sorted === 'desc' ? ' ▼' : sorted === 'asc' ? ' ▲' : ''}
    </th>
  )
}

/** SortableTh for a column of a tanstack table. */
export function ColumnTh<T>({ header, ...props }: { header: Header<T, unknown> } & ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <SortableTh sorted={header.column.getIsSorted()} onSort={() => header.column.toggleSorting()} {...props}>
      {String(header.column.columnDef.header)}
    </SortableTh>
  )
}
