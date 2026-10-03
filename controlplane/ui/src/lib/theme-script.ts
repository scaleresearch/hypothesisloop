// Free of React, so the root layout, a server component, can put the script in <head>.

export type ThemeChoice = 'light' | 'dark' | 'system'
export type Theme = 'light' | 'dark'

export const THEME_KEY = 'theme'
export const DARK_QUERY = '(prefers-color-scheme: dark)'

/**
 * Runs in <head> before the body is painted, so a page never flashes the wrong theme. It repeats
 * storedChoice and resolveTheme of lib/theme.ts in plain script; theme.test.ts runs it to keep the
 * two in step.
 */
export const themeScript = `try{var c=localStorage.getItem('${THEME_KEY}');var d=c==='dark'||(c==='system'&&matchMedia('${DARK_QUERY}').matches);document.documentElement.dataset.theme=d?'dark':'light'}catch(e){document.documentElement.dataset.theme='light'}`
