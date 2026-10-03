'use client'

import { useCallback, useEffect, useState } from 'react'
import { DARK_QUERY, THEME_KEY } from '@/lib/theme-script'
import type { Theme, ThemeChoice } from '@/lib/theme-script'

export type { Theme, ThemeChoice }

/** The choice this browser remembers; Light when there is none. */
export function storedChoice(): ThemeChoice {
  try {
    const value = localStorage.getItem(THEME_KEY)
    return value === 'dark' || value === 'system' ? value : 'light'
  } catch {
    return 'light'
  }
}

export function resolveTheme(choice: ThemeChoice): Theme {
  if (choice === 'system') return matchMedia(DARK_QUERY).matches ? 'dark' : 'light'
  return choice
}

export function applyTheme(choice: ThemeChoice) {
  document.documentElement.dataset.theme = resolveTheme(choice)
}

/** The theme choice of this browser, remembered, applied to <html>, and followed live while it is System. */
export function useThemeChoice(): [ThemeChoice, (choice: ThemeChoice) => void] {
  const [choice, setChoice] = useState<ThemeChoice>('light')

  useEffect(() => {
    setChoice(storedChoice())
    const onStorage = (e: StorageEvent) => {
      if (e.key !== THEME_KEY) return
      const next = storedChoice()
      setChoice(next)
      applyTheme(next)
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  useEffect(() => {
    if (choice !== 'system') return
    const query = matchMedia(DARK_QUERY)
    const follow = () => applyTheme('system')
    query.addEventListener('change', follow)
    return () => query.removeEventListener('change', follow)
  }, [choice])

  const choose = useCallback((next: ThemeChoice) => {
    try {
      localStorage.setItem(THEME_KEY, next)
    } catch {
      // A browser that keeps no storage still switches for this page.
    }
    setChoice(next)
    applyTheme(next)
  }, [])

  return [choice, choose]
}
