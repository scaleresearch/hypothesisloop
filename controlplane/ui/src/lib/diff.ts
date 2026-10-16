export type DiffLine = { kind: 'same' | 'add' | 'del'; text: string; before?: number; after?: number }

/** The lines of `before` and `after` as one list: kept, taken out and put in, by longest common subsequence. */
export function diffLines(before: string, after: string): DiffLine[] {
  const a = before.split('\n')
  const b = after.split('\n')
  // lcs[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
  const lcs = Array.from({ length: a.length + 1 }, () => new Uint32Array(b.length + 1))
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }
  const out: DiffLine[] = []
  let i = 0
  let j = 0
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      out.push({ kind: 'same', text: a[i], before: i + 1, after: j + 1 })
      i++
      j++
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      out.push({ kind: 'del', text: a[i], before: i + 1 })
      i++
    } else {
      out.push({ kind: 'add', text: b[j], after: j + 1 })
      j++
    }
  }
  for (; i < a.length; i++) out.push({ kind: 'del', text: a[i], before: i + 1 })
  for (; j < b.length; j++) out.push({ kind: 'add', text: b[j], after: j + 1 })
  return out
}

type DiffHunk = { kind: 'lines'; lines: DiffLine[] } | { kind: 'skipped'; lines: DiffLine[] }

/** The diff with runs of unchanged lines longer than twice `context` folded, keeping `context` lines around each change. */
export function foldUnchanged(lines: DiffLine[], context = 3): DiffHunk[] {
  const out: DiffHunk[] = []
  let run: DiffLine[] = []
  const flush = (atStart: boolean, atEnd: boolean) => {
    const keepBefore = atStart ? 0 : context
    const keepAfter = atEnd ? 0 : context
    if (run.length > keepBefore + keepAfter + 1) {
      push(run.slice(0, keepBefore))
      out.push({ kind: 'skipped', lines: run.slice(keepBefore, run.length - keepAfter) })
      push(run.slice(run.length - keepAfter))
    } else {
      push(run)
    }
    run = []
  }
  const push = (ls: DiffLine[]) => {
    if (ls.length === 0) return
    const last = out.at(-1)
    if (last?.kind === 'lines') last.lines.push(...ls)
    else out.push({ kind: 'lines', lines: [...ls] })
  }
  let seenChange = false
  for (const line of lines) {
    if (line.kind === 'same') {
      run.push(line)
      continue
    }
    flush(!seenChange, false)
    seenChange = true
    push([line])
  }
  flush(!seenChange, true)
  return out
}
