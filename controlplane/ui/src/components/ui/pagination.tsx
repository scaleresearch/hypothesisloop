import { Button } from '@/components/ui/button'

interface PaginationProps {
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
}

/** Offset-based prev/next pagination footer, driven by the API's X-Total-Count header. */
export function Pagination({ page, pageSize, total, onPageChange }: PaginationProps) {
  const pageCount = Math.max(1, Math.ceil(total / pageSize))
  if (pageCount <= 1) return null

  return (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '10px 4px 2px', gap: 12, flexWrap: 'wrap' }}>
      <span className="text-dim mono" style={{ fontSize: 11 }}>
        {total} total · page {page + 1} of {pageCount}
      </span>
      <div style={{ display: 'flex', gap: 6 }}>
        <Button size="sm" disabled={page === 0} onClick={() => onPageChange(0)}>« First</Button>
        <Button size="sm" disabled={page === 0} onClick={() => onPageChange(page - 1)}>‹ Prev</Button>
        <Button size="sm" disabled={page >= pageCount - 1} onClick={() => onPageChange(page + 1)}>Next ›</Button>
        <Button size="sm" disabled={page >= pageCount - 1} onClick={() => onPageChange(pageCount - 1)}>Last »</Button>
      </div>
    </div>
  )
}
