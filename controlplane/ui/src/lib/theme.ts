'use client'

import { useCallback, useEffect, useState } from 'react'
import { THEME_KEY } from '@/lib/theme-script'
import type { Theme } from '@/lib/theme-script'

export type { Theme }

/** The theme this browser remembers; Light when there is none, or one no longer offered. */
function storedTheme(): Theme {
  try {
    return localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light'
  } catch {
    return 'light'
  }
}

function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme
}

/** The theme of this browser, remembered and applied to <html>. */
export function useTheme(): [Theme, (theme: Theme) => void] {
  const [theme, setTheme] = useState<Theme>('light')

  useEffect(() => {
    setTheme(storedTheme())
    const onStorage = (e: StorageEvent) => {
      if (e.key !== THEME_KEY) return
      const next = storedTheme()
      setTheme(next)
      applyTheme(next)
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const choose = useCallback((next: Theme) => {
    try {
      localStorage.setItem(THEME_KEY, next)
    } catch {
      // A browser that keeps no storage still switches for this page.
    }
    setTheme(next)
    applyTheme(next)
  }, [])

  return [theme, choose]
}
