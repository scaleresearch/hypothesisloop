import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// jsdom has no layout, so it ships no ResizeObserver, which the combobox watches its options with.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
}

afterEach(cleanup)

// The page's configuration is the test's environment, so vi.stubEnv changes what the browser reads.
// The tests of server code run in the node environment, which has no window.
if (typeof window !== 'undefined') {
  // jsdom has no layout, so it ships no scrollIntoView, which a list scrolls its selected row into view with.
  Element.prototype.scrollIntoView = () => {}
  window.__hypothesisloopConfig = new Proxy({}, { get: (_, name) => typeof name === 'string' ? process.env[name] : undefined })
}
