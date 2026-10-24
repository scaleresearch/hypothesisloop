import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ListedModel, ModelListing } from '@/lib/api/models'

export function openRouterModel(id: string, name: string, more: Partial<ListedModel> = {}): ListedModel {
  return { model: `openrouter/${id}`, provider: 'openrouter', name, context_length: 131072, input_usd_per_mtok: 0.6, output_usd_per_mtok: 2.5, ...more }
}

/** What GET /models answers with both providers served. */
export const MODELS: ModelListing = {
  models: [
    openRouterModel('moonshotai/kimi-k2', 'MoonshotAI: Kimi K2'),
    { model: 'anthropic/claude-sonnet-5', provider: 'anthropic', name: 'claude-sonnet-5', context_length: 0, input_usd_per_mtok: 3, output_usd_per_mtok: 15 },
    { model: 'anthropic/claude-opus-5-5', provider: 'anthropic', name: 'claude-opus-5-5', context_length: 0, input_usd_per_mtok: 5, output_usd_per_mtok: 25 },
    openRouterModel('openai/gpt-5-nano', 'OpenAI: GPT-5 Nano', { context_length: 400000, input_usd_per_mtok: 0.05, output_usd_per_mtok: 0.4 }),
    openRouterModel('qwen/qwen3.7-flash', 'Qwen: Qwen3.7 Flash', { context_length: 1_000_000, input_usd_per_mtok: 0.1, output_usd_per_mtok: 0.4 }),
  ],
  default_model: 'anthropic/claude-sonnet-5',
  coordinator_model: 'anthropic/claude-opus-5-5',
}

/** Picks a model the way a person does: types part of its name and clicks its row. */
export async function pickModel(field: HTMLElement, search: string, model: string): Promise<void> {
  await userEvent.clear(field)
  await userEvent.type(field, search)
  await userEvent.click(await screen.findByRole('option', { name: new RegExp(`\\b${model.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')}\\b`) }))
}
