import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { getCoreRowModel, getSortedRowModel, useReactTable } from '@tanstack/react-table'
import type { SortingState } from '@tanstack/react-table'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { ColumnTh, SortableTh, Table } from '@/components/ui/table'

describe('a sortable header', () => {
  function header(sorted: false | 'asc' | 'desc', onSort = vi.fn()) {
    render(<Table><thead><tr><SortableTh sorted={sorted} onSort={onSort}>Status</SortableTh></tr></thead></Table>)
    return { onSort, th: screen.getByRole('columnheader') }
  }

  it('says which way it is sorted', () => {
    expect(header('desc').th.getAttribute('aria-sort')).toBe('descending')
    expect(screen.getByRole('columnheader').textContent).toBe('Status ▼')
  })

  it('says it is not sorted', () => {
    const { th } = header(false)
    expect(th.getAttribute('aria-sort')).toBe('none')
    expect(th.textContent).toBe('Status')
  })

  it('sorts on a click and from the keyboard', async () => {
    const { onSort, th } = header('asc')
    await userEvent.click(th)
    th.focus()
    await userEvent.keyboard('{Enter}')
    await userEvent.keyboard(' ')
    expect(onSort).toHaveBeenCalledTimes(3)
  })
})

describe('a column header of a tanstack table', () => {
  const data = [{ name: 'b', score: 1 }, { name: 'a', score: 3 }, { name: 'c', score: 2 }]
  const columns = [{ accessorKey: 'name', header: 'Name' }, { accessorKey: 'score', header: 'Score' }]

  function Scores() {
    const [sorting, setSorting] = useState<SortingState>([])
    const table = useReactTable({ data, columns, state: { sorting }, onSortingChange: setSorting, getCoreRowModel: getCoreRowModel(), getSortedRowModel: getSortedRowModel() })
    return (
      <Table>
        <thead><tr>{table.getFlatHeaders().map(h => <ColumnTh key={h.id} header={h} />)}</tr></thead>
        <tbody>{table.getRowModel().rows.map(r => <tr key={r.id}><td>{r.original.name}</td><td>{r.original.score}</td></tr>)}</tbody>
      </Table>
    )
  }

  const names = () => screen.getAllByRole('row').slice(1).map(r => r.firstChild!.textContent)

  it('sorts the rows by its column, one way and then the other', async () => {
    render(<Scores />)
    expect(names()).toEqual(['b', 'a', 'c'])
    await userEvent.click(screen.getByRole('columnheader', { name: /Score/ }))
    expect(names()).toEqual(['a', 'c', 'b'])
    expect(screen.getByRole('columnheader', { name: /Score/ }).getAttribute('aria-sort')).toBe('descending')
    await userEvent.click(screen.getByRole('columnheader', { name: /Score/ }))
    expect(names()).toEqual(['b', 'c', 'a'])
  })
})
