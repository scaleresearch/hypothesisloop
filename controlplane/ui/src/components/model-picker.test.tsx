import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchModels } from '@/lib/api/models'
import { ModelPicker } from '@/components/model-picker'
import { MODELS, openRouterModel, pickModel } from '@/test-models'

vi.mock('@/lib/api/models', () => ({ fetchModels: vi.fn() }))

const picked = vi.fn()

function Picker({ initial = '', ...props }: { initial?: string; inherited?: string; coordinator?: boolean }) {
  const [value, setValue] = useState(initial)
  return <ModelPicker aria-label="Model" value={value} onChange={m => { picked(m); setValue(m) }} {...props} />
}

function show(props: Parameters<typeof Picker>[0] = {}) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><Picker {...props} /></SWRConfig>)
}

const field = () => screen.getByLabelText('Model') as HTMLInputElement

async function open() {
  await userEvent.click(field())
  await screen.findByRole('listbox')
  await waitFor(() => expect(screen.getAllByRole('option').length).toBeGreaterThan(1))
}

beforeEach(() => {
  picked.mockReset()
  vi.mocked(fetchModels).mockReset().mockResolvedValue(MODELS)
})

describe('the model picker', () => {
  it('groups the models by provider, the Claude subscription first, each row with its name, reference, context and prices', async () => {
    show()
    await open()
    const groups = within(screen.getByRole('listbox')).getAllByRole('group')
    expect(groups.map(g => g.getAttribute('aria-label'))).toEqual(['Claude subscription', 'OpenRouter'])
    const refs = (g: HTMLElement) => within(g).getAllByRole('option').map(o => within(o).getByText(/^[a-z]+\//).textContent)
    expect(refs(groups[0])).toEqual(['anthropic/claude-sonnet-5', 'anthropic/claude-opus-5-5'])
    expect(refs(groups[1])).toEqual(['openrouter/moonshotai/kimi-k2', 'openrouter/openai/gpt-5-nano', 'openrouter/qwen/qwen3.7-flash'])
    const kimi = screen.getByRole('option', { name: /kimi-k2/ }).textContent
    expect(kimi).toContain('MoonshotAI: Kimi K2')
    expect(kimi).toContain('131K ctx')
    expect(kimi).toContain('$0.60 in · $2.50 out /M')
    expect(screen.getByRole('option', { name: /qwen3\.7-flash/ }).textContent).toContain('1M ctx')
    expect(screen.getByRole('option', { name: /gpt-5-nano/ }).textContent).toContain('$0.05 in')
    expect(screen.getByRole('option', { name: /claude-opus-5-5/ }).textContent).not.toContain('ctx')
    expect(screen.getByRole('option', { name: /qwen3\.7-flash/ }).textContent).not.toContain('free')
  })

  it('searches the reference and the name, every word typed', async () => {
    show()
    await open()
    await userEvent.type(field(), 'moonshot k2')
    expect(within(screen.getByRole('group', { name: 'OpenRouter' })).getAllByRole('option').map(o => o.textContent)).toEqual([
      expect.stringContaining('openrouter/moonshotai/kimi-k2'),
    ])
    await userEvent.clear(field())
    await userEvent.type(field(), 'Qwen3.7')
    expect(screen.getByRole('option', { name: /qwen3\.7-flash/ })).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'Claude subscription' })).toBeNull()
    await userEvent.clear(field())
    await userEvent.type(field(), 'nothing like it')
    expect(screen.getByText('No matches')).toBeTruthy()
  })

  it('sends the reference picked, and empty for the default', async () => {
    show()
    await pickModel(field(), 'kimi', 'openrouter/moonshotai/kimi-k2')
    expect(picked).toHaveBeenLastCalledWith('openrouter/moonshotai/kimi-k2')
    expect(field().value).toBe('openrouter/moonshotai/kimi-k2')
    await userEvent.click(field())
    await userEvent.click(await screen.findByRole('option', { name: 'Platform default: anthropic/claude-sonnet-5' }))
    expect(picked).toHaveBeenLastCalledWith('')
    expect(field().placeholder).toBe('Platform default: anthropic/claude-sonnet-5')
  })

  it('names the default an empty value runs on: the type\'s model, or the coordinator\'s', async () => {
    const { unmount } = show({ inherited: 'openrouter/moonshotai/kimi-k2' })
    expect(field().placeholder).toBe("Type's model: openrouter/moonshotai/kimi-k2")
    unmount()
    show({ coordinator: true })
    await waitFor(() => expect(field().placeholder).toBe('Platform default: anthropic/claude-opus-5-5'))
  })

  it('shows a stored model the platform no longer lists as itself', async () => {
    show({ initial: 'openrouter/gone/model' })
    await waitFor(() => expect(fetchModels).toHaveBeenCalled())
    expect(field().value).toBe('openrouter/gone/model')
  })

  it('shows a few of hundreds of models and leaves the rest to a search', async () => {
    const many = Array.from({ length: 400 }, (_, i) => openRouterModel(`lab/model-${i}`, `Lab: Model ${i}`))
    vi.mocked(fetchModels).mockResolvedValue({ ...MODELS, models: [...MODELS.models, ...many] })
    show()
    await open()
    expect(screen.getAllByRole('option').length).toBeLessThanOrEqual(81)
    expect(screen.getByText(/^\d+ more, type to search$/)).toBeTruthy()
    await userEvent.type(field(), 'model-399')
    expect(screen.getByRole('option', { name: /lab\/model-399/ })).toBeTruthy()
  })

  it('says when the models could not be loaded', async () => {
    vi.mocked(fetchModels).mockRejectedValue(new Error('the platform is down'))
    show()
    await userEvent.click(field())
    expect(await screen.findByText('Could not load the models: the platform is down')).toBeTruthy()
  })
})
