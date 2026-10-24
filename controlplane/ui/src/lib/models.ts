import useSWR from 'swr'
import { fetchModels } from '@/lib/api/models'
import type { ListedModel, ModelProvider } from '@/lib/api/models'
import { keys } from '@/lib/keys'

/** Each provider in the order its models are offered, with its name in a picker and on a badge. */
export const PROVIDERS: { provider: ModelProvider; label: string; short: string }[] = [
  { provider: 'anthropic', label: 'Claude subscription', short: 'Claude' },
  { provider: 'openai', label: 'ChatGPT subscription', short: 'ChatGPT' },
  { provider: 'openrouter', label: 'OpenRouter', short: 'OpenRouter' },
]

export function useModels() {
  return useSWR(keys.models(), fetchModels)
}

/** A reference's provider and model id, split at the first slash; the provider is none the platform knows for a malformed one. */
export function parseModelRef(ref: string): { provider: ModelProvider | undefined; id: string } {
  const slash = ref.indexOf('/')
  const provider = PROVIDERS.find(p => p.provider === ref.slice(0, slash))?.provider
  return slash > 0 && provider ? { provider, id: ref.slice(slash + 1) } : { provider: undefined, id: ref }
}

/**
 * What an agent that names no model of its own runs on, as the platform resolves it: its type's
 * model, else for a researcher the coordinator's (the experiment's coordinator model, else the
 * platform's coordinator default), else nothing here, the platform's default of its role.
 */
export function inheritedModel(typeModel: string | undefined, researcher: boolean, coordinatorModel: string | undefined): { inherited?: string; inheritedFrom?: string } {
  if (typeModel) return { inherited: typeModel, inheritedFrom: "Type's model" }
  if (researcher) return { inherited: coordinatorModel || undefined, inheritedFrom: "Coordinator's model" }
  return {}
}

/** The models in the order a picker offers them: by provider, each provider's in the order listed. */
export function byProvider(models: ListedModel[]): ListedModel[] {
  return PROVIDERS.flatMap(p => models.filter(m => m.provider === p.provider))
}

/** 131072 tokens as 131K. */
export function formatContext(tokens: number): string {
  if (tokens >= 1_000_000) return `${+(tokens / 1_000_000).toFixed(1)}M`
  return `${Math.round(tokens / 1000)}K`
}

export function formatPrice(usdPerMTok: number): string {
  return `$${usdPerMTok < 0.1 && usdPerMTok > 0 ? +usdPerMTok.toPrecision(2) : usdPerMTok.toFixed(2)}`
}
