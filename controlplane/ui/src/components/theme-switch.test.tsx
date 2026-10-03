import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ThemeSwitch } from '@/components/theme-switch'
import { THEME_KEY, themeScript } from '@/lib/theme-script'

/** An operating system in dark or light mode that can change while the page is open. */
function system(dark: boolean) {
  const listeners = new Set<() => void>()
  const query = {
    get matches() { return dark },
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
  }
  vi.stubGlobal('matchMedia', () => query)
  return {
    turn(next: boolean) {
      dark = next
      listeners.forEach(fn => fn())
    },
  }
}

const theme = () => document.documentElement.dataset.theme
const pressed = (name: string) => screen.getByRole('button', { name }).getAttribute('aria-pressed')

// Node's own localStorage, unusable without a file, shadows the page's, so each test gets one in memory.
function memoryStorage() {
  const items = new Map<string, string>()
  return {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, String(value)),
    removeItem: (key: string) => void items.delete(key),
    clear: () => items.clear(),
  }
}

beforeEach(() => {
  vi.stubGlobal('localStorage', memoryStorage())
  delete document.documentElement.dataset.theme
})

describe('the script in <head>', () => {
  const run = () => new Function(themeScript)()

  it('sets Light with nothing stored, on a machine in dark mode too', () => {
    system(true)
    run()
    expect(theme()).toBe('light')
  })

  it('sets the stored choice', () => {
    system(false)
    localStorage.setItem(THEME_KEY, 'dark')
    run()
    expect(theme()).toBe('dark')
  })

  it('follows the operating system only when System is stored', () => {
    const os = system(true)
    localStorage.setItem(THEME_KEY, 'system')
    run()
    expect(theme()).toBe('dark')
    os.turn(false)
    run()
    expect(theme()).toBe('light')
  })

  it('falls back to Light on a stored value it does not know', () => {
    system(true)
    localStorage.setItem(THEME_KEY, 'sepia')
    run()
    expect(theme()).toBe('light')
  })
})

describe('ThemeSwitch', () => {
  it('shows Light with nothing stored, whatever the operating system says', () => {
    system(true)
    render(<ThemeSwitch />)
    expect(screen.getByRole('group', { name: 'Theme' })).toBeTruthy()
    expect(pressed('Light')).toBe('true')
    expect(pressed('Dark')).toBe('false')
    expect(pressed('System')).toBe('false')
  })

  it('applies a choice to <html> and remembers it', async () => {
    system(false)
    render(<ThemeSwitch />)
    await userEvent.click(screen.getByRole('button', { name: 'Dark' }))
    expect(theme()).toBe('dark')
    expect(localStorage.getItem(THEME_KEY)).toBe('dark')
    expect(pressed('Dark')).toBe('true')

    await userEvent.click(screen.getByRole('button', { name: 'Light' }))
    expect(theme()).toBe('light')
    expect(localStorage.getItem(THEME_KEY)).toBe('light')
  })

  it('opens on the remembered choice', () => {
    system(false)
    localStorage.setItem(THEME_KEY, 'dark')
    render(<ThemeSwitch />)
    expect(pressed('Dark')).toBe('true')
  })

  it('follows the operating system live while System is chosen, and stops when it is not', async () => {
    const os = system(false)
    render(<ThemeSwitch />)
    await userEvent.click(screen.getByRole('button', { name: 'System' }))
    expect(theme()).toBe('light')
    expect(localStorage.getItem(THEME_KEY)).toBe('system')

    act(() => os.turn(true))
    expect(theme()).toBe('dark')

    await userEvent.click(screen.getByRole('button', { name: 'Light' }))
    act(() => os.turn(true))
    expect(theme()).toBe('light')
  })
})
