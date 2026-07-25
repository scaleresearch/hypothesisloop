import type { Metadata } from 'next'
import type { ReactNode } from 'react'
import { Inter, JetBrains_Mono } from 'next/font/google'
import './globals.css'
import { Nav } from '@/components/nav'
import { CommandPalette } from '@/components/command-palette'

const inter = Inter({ subsets: ['latin'], variable: '--font-sans', display: 'swap' })
const jetbrainsMono = JetBrains_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap', weight: ['400', '500', '700'] })

export const metadata: Metadata = {
  title: 'HypothesisLoop — Autonomous Research Platform',
  description: 'Compute scheduler for autonomous ML research agents',
}

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${jetbrainsMono.variable} dark`}>
      <body className="flex min-h-screen bg-background text-foreground antialiased">
        <Nav />
        <main className="flex-1 overflow-auto wa-main">
          {children}
        </main>
        <CommandPalette />
      </body>
    </html>
  )
}
