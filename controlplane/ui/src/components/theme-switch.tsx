'use client'

import { Moon, Sun } from 'lucide-react'
import { Segmented } from '@/components/ui/segmented'
import { useTheme } from '@/lib/theme'
import type { Theme } from '@/lib/theme'

const icon = { size: 13, strokeWidth: 2, 'aria-hidden': true } as const

const options: { value: Theme; label: string; icon: JSX.Element }[] = [
  { value: 'light', label: 'Light', icon: <Sun {...icon} /> },
  { value: 'dark', label: 'Dark', icon: <Moon {...icon} /> },
]

/** Light or Dark; this browser remembers the choice. */
export function ThemeSwitch() {
  const [theme, choose] = useTheme()
  return <Segmented label="Theme" size="sm" options={options} value={theme} onChange={choose} />
}
