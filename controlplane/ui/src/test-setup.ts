import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// jsdom has no layout, so it ships no ResizeObserver, which the combobox watches its options with.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
}

// Node 25 puts a localStorage of its own in place of jsdom's, and it stores nothing without a
// file. Tests get one that lives in memory, emptied between them.
const stored = new Map<string, string>()
Object.defineProperty(globalThis, 'localStorage', {
  configurable: true,
  value: {
    getItem: (key: string) => stored.get(key) ?? null,
    setItem: (key: string, value: string) => void stored.set(key, String(value)),
    removeItem: (key: string) => void stored.delete(key),
    clear: () => stored.clear(),
  },
})

afterEach(() => {
  cleanup()
  stored.clear()
})
