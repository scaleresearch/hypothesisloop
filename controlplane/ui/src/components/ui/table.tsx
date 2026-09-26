'use client'

import type { HTMLAttributes, TableHTMLAttributes, ThHTMLAttributes } from 'react'
import type { Header } from '@tanstack/react-table'
import { clsx } from 'clsx'
import { useProjectRouter } from '@/lib/query-state'

// The :where() keeps these at the weight of a plain utility, so a class on a cell wins.
const tableClass = clsx(
  'w-full border-collapse text-sm',
  '[:where(&)_th]:overflow-hidden [:where(&)_th]:text-ellipsis [:where(&)_th]:whitespace-nowrap [:where(&)_th]:border-b [:where(&)_th]:px-3 [:where(&)_th]:py-2 [:where(&)_th]:text-left',
  '[:where(&)_th]:text-2xs [:where(&)_th]:font-bold [:where(&)_th]:uppercase [:where(&)_th]:tracking-wider [:where(&)_th]:text-muted',
  '[:where(&)_td]:border-b [:where(&)_td]:px-3 [:where(&)_td]:py-2.5 [:where(&)_td]:align-middle',
  '[:where(&)_tbody>tr:last-child>td]:border-b-0 [:where(&)_tbody>tr:hover>td]:bg-primary/5',
)

/** The data table. Rows and cells inside are plain thead, tbody, tr, th and td. */
export function Table({ className, ...props }: TableHTMLAttributes<HTMLTableElement>) {
  return <table className={clsx(tableClass, className)} {...props} />
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
