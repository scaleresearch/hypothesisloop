/**
 * What agents of past runs needed and could not read from the brief, as lines a person adds to
 * it. The examples come from different kinds of experiment, so that none reads as the only shape.
 */
export const BRIEF_POINTS = [
  {
    label: 'Metric',
    ask: 'Which number decides, measured how, and which way is better?',
    example: 'val_loss on the held-out 1% of FineWeb, mean of the last 100 steps, lower is better.',
  },
  {
    label: 'Control',
    ask: 'What is there to beat, exactly as it runs today?',
    example: 'kernels/attn.cu at main with its default tile sizes, timed by bench.py.',
  },
  {
    label: 'Win',
    ask: 'How much better counts, over how many seeds or repeats?',
    example: 'at least 2% below the control, mean of 3 seeds, and more than the spread between them.',
  },
  {
    label: 'Held equal',
    ask: 'What does every compared run get the same of, in units a job can report?',
    example: 'the same 2,000 steps, batch 256 and one RTX 4090; every optimizer gets the same 16-trial LR sweep.',
  },
  {
    label: 'Fixed',
    ask: 'Which files, data and checks must stay as they are?',
    example: 'eval.py, the test split and tests/. Only model.py and optim.py may change.',
  },
  {
    label: 'For',
    ask: 'What is the result for, so a trick that only fits this data does not count?',
    example: 'a codec for any lidar recording, so nothing that relies on the layout of this one file.',
  },
] as const

export type BriefPoint = typeof BRIEF_POINTS[number]

const lineStart = (point: BriefPoint) => new RegExp(`^[ \\t]*${point.label}:`, 'im')

/** Whether text has a line for the point with something written after its label. */
export function covers(text: string, point: BriefPoint): boolean {
  return new RegExp(`^[ \\t]*${point.label}:[ \\t]*\\S`, 'im').test(text)
}

/**
 * The text with a line for the point, and where the caret goes to fill it in: the end of the
 * point's line when the text has one, else the end of a new line for it.
 */
export function withPoint(text: string, point: BriefPoint): { text: string; caret: number } {
  const found = lineStart(point).exec(text)
  if (found) {
    const end = text.indexOf('\n', found.index)
    return { text, caret: end === -1 ? text.length : end }
  }
  const added = `${text}${text === '' || text.endsWith('\n') ? '' : '\n'}${point.label}: `
  return { text: added, caret: added.length }
}
