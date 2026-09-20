import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// jsdom has no layout, so it ships no ResizeObserver, which the combobox watches its options with.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
}

afterEach(cleanup)
