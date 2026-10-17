import type { Metadata } from 'next'
import type { ReactNode } from 'react'
import { headers } from 'next/headers'
import { NONCE_HEADER } from '@/lib/content-security'
import { Instrument_Sans, IBM_Plex_Mono } from 'next/font/google'
import { themeScript } from '@/lib/theme-script'
import { configScript } from '@/lib/config'
import './globals.css'

const sans = Instrument_Sans({ subsets: ['latin'], variable: '--font-sans', display: 'swap' })
const plexMono = IBM_Plex_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap', weight: ['400', '500'] })

export const metadata: Metadata = {
  title: 'HypothesisLoop',
  description: 'Compute scheduler for autonomous ML research agents',
}

export default function RootLayout({ children }: { children: ReactNode }) {
  const nonce = headers().get(NONCE_HEADER) ?? undefined
  return (
    // The theme script sets data-theme before React hydrates, which React would otherwise report.
    <html lang="en" className={`${sans.variable} ${plexMono.variable}`} suppressHydrationWarning>
      <head>
        <script nonce={nonce} dangerouslySetInnerHTML={{ __html: configScript() }} />
        <script nonce={nonce} dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body className="flex min-h-screen antialiased">
        {children}
      </body>
    </html>
  )
}
