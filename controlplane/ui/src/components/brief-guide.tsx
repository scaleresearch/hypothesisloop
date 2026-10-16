import { Disclosure } from '@/components/ui/disclosure'

const POINTS = [
  { what: 'The number to improve, and how it is measured', example: 'val_loss on the held-out split, lower is better' },
  { what: 'What to compare against', example: 'train.py as it runs today' },
  { what: 'What must stay the same', example: 'the data, 2,000 steps, one RTX 4090, eval.py untouched' },
  { what: 'Limits on time or spend', example: 'a few hours, at most 10 GPU-hours' },
]

/** The few things a brief should say, folded until asked for. */
export function BriefGuide({ note, className }: { note: string; className?: string }) {
  return (
    <Disclosure label="What makes a good brief?" className={className}>
      <div className="grid gap-2 text-sm">
        <ul className="grid gap-1.5">
          {POINTS.map(point => (
            <li key={point.what}>
              <span className="font-medium">{point.what}</span>
              <span className="text-muted">, e.g. {point.example}</span>
            </li>
          ))}
        </ul>
        <p className="text-xs text-muted">{note}</p>
      </div>
    </Disclosure>
  )
}
