// Free of React, so the root layout, a server component, can put the script in <head>.

export type Theme = 'light' | 'dark'

export const THEME_KEY = 'theme'

/**
 * Runs in <head> before the body is painted, so a page never flashes the wrong theme. It repeats
 * storedTheme of lib/theme.ts in plain script: Dark when stored, Light for anything else.
 */
export const themeScript = `try{document.documentElement.dataset.theme=localStorage.getItem('${THEME_KEY}')==='dark'?'dark':'light'}catch(e){document.documentElement.dataset.theme='light'}`
