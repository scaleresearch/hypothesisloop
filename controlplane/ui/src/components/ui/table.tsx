'use client'

import type { HTMLAttributes, TableHTMLAttributes, ThHTMLAttributes } from 'react'
import type { Header } from '@tanstack/react-table'
import { clsx } from 'clsx'
import { useProjectRouter } from '@/lib/query-state'

/** The data table. Rows and cells inside are plain thead, tbody, tr, th and td, styled by .data-table in globals.css. */
export function Table({ className, ...props }: TableHTMLAttributes<HTMLTableElement>) {
  return <table className={clsx('data-table w-full border-collapse text-sm', className)} {...props} />
}

/** A cell or header of figures: right-aligned, digits of one width so a column lines up. */
export const num = 'text-right font-mono text-xs tabular-nums'

/** A cell of an id or a time, the quiet columns of a row. */
export const quiet = 'whitespace-nowrap font-mono text-2xs text-subtle'

/** A row that opens the record it lists, on a click or Enter. A control inside stops its own click. */
export function LinkTr({ href, className, ...props }: HTMLAttributes<HTMLTableRowElement> & { href: string }) {
  const router = useProjectRouter()
  return (
    <tr
      tabIndex={0}
      role="link"
      className={clsx('cursor-pointer', className)}
      onClick={() => router.push(href)}
      onKeyDown={e => { if (e.key === 'Enter' && e.target === e.currentTarget) router.push(href) }}
      {...props}
    />
  )
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
