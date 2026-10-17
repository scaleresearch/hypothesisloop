'use client'

import { useEffect } from 'react'
import { useSWRConfig } from 'swr'
import { fetchChanges, watchURL, WATCH_SUBPROTOCOL, type After } from '@/lib/api/changes'
import type { Change, WatchFrame } from '@/types/api'
import { ApiError } from '@/lib/api/request'
import { accessToken, endSession } from '@/lib/auth'
import { LIVE_KINDS, readsOf, type Reads } from '@/lib/live'

// The server repeats its progress frame every 20 s on an idle stream, so a socket silent for
// longer than two of those lost its path.
const SILENCE_MS = 50_000
const FIRST_RETRY_MS = 1_000
const LAST_RETRY_MS = 30_000

const everything: Reads = () => true

/**
 * Follows the project's changes on one socket and reads again what each names. The cursor moves
 * only on a progress frame, so a socket that drops mid-read is opened again from the last one
 * finished and nothing is missed. A hidden tab reads nothing again: the page is read on focus.
 */
function follow(projectID: string, revalidate: (reads: Reads) => void): () => void {
  let cursor: After = 'head'
  let retry = FIRST_RETRY_MS
  let socket: WebSocket | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let stopped = false

  const visible = (reads: Reads) => { if (!document.hidden) revalidate(reads) }

  function later(delay: number) {
    timer = setTimeout(() => void open(), delay)
  }

  function backOff() {
    later(retry)
    retry = Math.min(retry * 2, LAST_RETRY_MS)
  }

  // A socket closed before its first frame failed its handshake, whose status a browser hides:
  // the same read over HTTP answers it. 401 ends the session on the way (apiFetch).
  async function diagnose() {
    try {
      await fetchChanges(projectID, LIVE_KINDS, cursor)
    } catch (error) {
      if (stopped) return
      if (error instanceof ApiError && error.status === 410) {
        // Starting from head reads everything again once the socket is there.
        cursor = 'head'
        later(0)
        return
      }
    }
    if (!stopped) backOff()
  }

  async function open() {
    let token: string | null
    try {
      token = await accessToken()
    } catch {
      if (!stopped) backOff()
      return
    }
    if (stopped) return
    if (!token) {
      void endSession('ended')
      return
    }
    const fromHead = cursor === 'head'
    const ws = new WebSocket(watchURL(projectID, LIVE_KINDS, cursor), [WATCH_SUBPROTOCOL, token])
    socket = ws
    let heard = false
    let pending: Change[] = []
    let silence = setTimeout(() => ws.close(), SILENCE_MS)
    ws.onmessage = event => {
      heard = true
      clearTimeout(silence)
      silence = setTimeout(() => ws.close(), SILENCE_MS)
      const frame = JSON.parse(event.data) as WatchFrame
      if ('subscription' in frame) {
        retry = FIRST_RETRY_MS
        // What the page read before the stream's head is pinned may have changed since.
        if (fromHead) visible(everything)
      } else if ('id' in frame) {
        pending.push(frame)
      } else {
        if (pending.length > 0) visible(readsOf(pending))
        pending = []
        cursor = frame.next
      }
    }
    ws.onclose = () => {
      clearTimeout(silence)
      if (stopped) return
      if (heard) backOff()
      else void diagnose()
    }
  }

  void open()
  return () => {
    stopped = true
    clearTimeout(timer)
    socket?.close()
  }
}

/** The one stream of the tab, of the project it shows. */
export function LiveChanges({ projectID }: { projectID: string }) {
  const { mutate } = useSWRConfig()
  useEffect(() => follow(projectID, reads => void mutate(reads)), [projectID, mutate])
  return null
}
