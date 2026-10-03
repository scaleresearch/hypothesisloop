import type { Metadata } from 'next'
import type { ReactNode } from 'react'
import { Inter, IBM_Plex_Mono } from 'next/font/google'
import { themeScript } from '@/lib/theme-script'
import './globals.css'

const inter = Inter({ subsets: ['latin'], variable: '--font-sans', display: 'swap' })
const plexMono = IBM_Plex_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap', weight: ['400', '500', '600', '700'] })

export const metadata: Metadata = {
  title: 'HypothesisLoop',
  description: 'Compute scheduler for autonomous ML research agents',
}

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    // The theme script sets data-theme before React hydrates, which React would otherwise report.
    <html lang="en" className={`${inter.variable} ${plexMono.variable}`} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body className="flex min-h-screen antialiased">
        {children}
      </body>
    </html>
  )
}
