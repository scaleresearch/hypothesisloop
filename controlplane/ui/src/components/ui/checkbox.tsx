import type { InputHTMLAttributes, ReactNode } from 'react'
import { clsx } from 'clsx'
import { tint } from '@/lib/colors'

interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> {
  label: ReactNode
  /** Drawn as a pill that lights up when checked, for a row of toggles. */
  pill?: boolean
  /** The colour a checked pill lights up in, when each toggle has its own (a chart series). */
  color?: string
}

export function Checkbox({ label, pill, color, className, ...props }: CheckboxProps) {
  return (
    <label
      className={clsx(
        'inline-flex cursor-pointer items-center gap-1.5 text-xs',
        pill && 'rounded-full border bg-surface-inset px-2 py-0.5 font-mono text-muted has-[:checked]:border-primary has-[:checked]:bg-primary/15 has-[:checked]:text-foreground',
        className,
      )}
      style={pill && color && props.checked ? { borderColor: color, background: tint(color, 0.1) } : undefined}
    >
      <input type="checkbox" className="m-0 h-3.5 w-3.5 cursor-pointer accent-primary" {...props} />
      {label}
    </label>
  )
}
