import { act, render, waitFor } from '@testing-library/react'
import useSWR, { SWRConfig } from 'swr'
import { beforeEach, expect, it, vi } from 'vitest'
import { fetchChanges } from '@/lib/api/changes'
import { ApiError } from '@/lib/api/request'
import { LiveChanges } from '@/components/live-changes'

vi.mock('@/lib/api/changes', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/changes')>()),
  fetchChanges: vi.fn(),
}))
vi.mock('@/lib/auth', () => ({ accessToken: vi.fn(async () => 'token'), endSession: vi.fn() }))

class FakeSocket {
  static opened: FakeSocket[] = []
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: ((event: { code: number }) => void) | null = null
  constructor(readonly url: string, readonly protocols: string[]) {
    FakeSocket.opened.push(this)
  }
  close() {}
  send(frame: object) {
    this.onmessage?.({ data: JSON.stringify(frame) })
  }
  end(code: number) {
    this.onclose?.({ code })
  }
}

const after = (socket: FakeSocket) => new URL(socket.url).searchParams.get('after')

// What the page shows, read once on mount and again whenever the stream says to.
function Page({ reads }: { reads: () => void }) {
  useSWR('page', async () => { reads(); return 1 }, { revalidateOnFocus: false })
  return <LiveChanges projectID="prj_1" />
}

beforeEach(() => {
  FakeSocket.opened = []
  vi.stubGlobal('WebSocket', FakeSocket)
})

// A cursor the server no longer holds was asked for again from the same place forever.
it('starts again from the head when the server says its cursor is gone, and reads the page again', async () => {
  const reads = vi.fn()
  vi.mocked(fetchChanges).mockRejectedValue(new ApiError(410, 'the changes after 42 are no longer kept', 'cursor_gone'))
  render(<SWRConfig value={{ provider: () => new Map() }}><Page reads={reads} /></SWRConfig>)
  await waitFor(() => expect(FakeSocket.opened).toHaveLength(1))
  const first = FakeSocket.opened[0]
  expect(after(first)).toBe('head')
  expect(first.protocols).toEqual(['hypothesisloop.watch', 'token'])
  await waitFor(() => expect(reads).toHaveBeenCalledTimes(1))

  act(() => {
    first.send({ subscription: { project_id: 'prj_1' } })
    first.send({ next: 42, caught_up: true })
  })
  await waitFor(() => expect(reads).toHaveBeenCalledTimes(2))
  // The server refuses the stream from 42, and the same read over HTTP says why.
  act(() => first.end(1008))
  await waitFor(() => expect(fetchChanges).toHaveBeenCalledWith('prj_1', expect.any(Array), 42))
  await waitFor(() => expect(FakeSocket.opened).toHaveLength(2))
  const second = FakeSocket.opened[1]
  expect(after(second)).toBe('head')

  act(() => second.send({ subscription: { project_id: 'prj_1' } }))
  await waitFor(() => expect(reads).toHaveBeenCalledTimes(3))
})
