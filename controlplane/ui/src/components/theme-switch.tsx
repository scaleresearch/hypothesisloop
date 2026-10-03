'use client'

import { Monitor, Moon, Sun } from 'lucide-react'
import { Segmented } from '@/components/ui/tabs'
import { useThemeChoice } from '@/lib/theme'
import type { ThemeChoice } from '@/lib/theme'

const icon = { size: 13, strokeWidth: 2, 'aria-hidden': true } as const

const options: { value: ThemeChoice; label: string; icon: JSX.Element }[] = [
  { value: 'light', label: 'Light', icon: <Sun {...icon} /> },
  { value: 'dark', label: 'Dark', icon: <Moon {...icon} /> },
  { value: 'system', label: 'System', icon: <Monitor {...icon} /> },
]

/** Light, Dark, or what the operating system says; this browser remembers the choice. */
export function ThemeSwitch() {
  const [choice, choose] = useThemeChoice()
  return <Segmented label="Theme" size="sm" options={options} value={choice} onChange={choose} />
}
