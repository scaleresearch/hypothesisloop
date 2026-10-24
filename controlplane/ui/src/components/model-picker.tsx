'use client'

import { byProvider, formatContext, formatPrice, PROVIDERS, useModels } from '@/lib/models'
import type { ListedModel } from '@/lib/api/models'
import type { FieldSize } from '@/components/ui/field'
import { SearchableSelect } from '@/components/ui/searchable-select'

// Enough to browse, few enough that every keystroke stays quick on OpenRouter's few hundred.
const SHOWN = 80

function ModelRow({ m }: { m: ListedModel }) {
  return (
    <span className="flex min-w-0 items-baseline gap-3">
      <span className="grid min-w-0 flex-1">
        <span className="truncate font-sans">{m.name}</span>
        <span className="truncate font-mono text-2xs text-subtle">{m.model}</span>
      </span>
      <span className="flex-none whitespace-nowrap text-right font-sans text-2xs tabular-nums text-muted">
        {m.context_length > 0 && <>{formatContext(m.context_length)} ctx · </>}
        {m.input_usd_per_mtok === 0 && m.output_usd_per_mtok === 0
          ? 'free'
          : <>{formatPrice(m.input_usd_per_mtok)} in · {formatPrice(m.output_usd_per_mtok)} out /M</>}
      </span>
    </span>
  )
}

interface ModelPickerProps {
  /** A model reference, or empty for the model it inherits. */
  value: string
  onChange: (model: string) => void
  /** The model an empty value runs on when it is not the platform's default: an agent type's own, or a researcher's coordinator's. */
  inherited?: string
  /** Whose model inherited is. */
  inheritedFrom?: string
  /** For a coordinator or a researcher, whose default is the platform's coordinator model. */
  coordinator?: boolean
  fieldSize?: FieldSize
  className?: string
  'aria-label'?: string
}

/** Picks the model an agent or coordinator runs on, of those the platform serves, searched by reference and name. */
export function ModelPicker({ value, onChange, inherited, inheritedFrom = "Type's model", coordinator, fieldSize = 'default', className, 'aria-label': ariaLabel }: ModelPickerProps) {
  const { data, error } = useModels()
  const platformDefault = coordinator ? data?.coordinator_model : data?.default_model
  const empty = inherited
    ? `${inheritedFrom}: ${inherited}`
    : `Platform default${platformDefault ? `: ${platformDefault}` : ''}`
  return (
    <span className="block min-w-0" title={value || empty}>
      <SearchableSelect
        aria-label={ariaLabel}
        mono
        fieldSize={fieldSize}
        className={className ?? 'w-full min-w-0'}
        value={value}
        onChange={onChange}
        allLabel={empty}
        limit={SHOWN}
        options={byProvider(data?.models ?? []).map(m => ({
          value: m.model,
          label: m.model,
          keywords: m.name,
          group: PROVIDERS.find(p => p.provider === m.provider)?.label,
          content: <ModelRow m={m} />,
        }))}
        hint={error ? `Could not load the models: ${error.message}` : !data ? 'Loading models' : undefined}
      />
    </span>
  )
}
