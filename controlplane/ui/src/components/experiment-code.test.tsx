import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ExperimentCode } from '@/components/experiment-code'

describe('the code of a platform experiment', () => {
  it('links the repository to where agents clone it, and names a custom agent image', () => {
    render(<ExperimentCode pe={{ repository: 'acme/widgets', repository_url: 'https://github.com/acme/widgets', agent_image: 'ghcr.io/acme/agent:1' }} />)
    const link = screen.getByRole('link', { name: 'acme/widgets' })
    expect(link.getAttribute('href')).toBe('https://github.com/acme/widgets')
    expect(link.getAttribute('rel')).toBe('noreferrer')
    expect(screen.getByText('ghcr.io/acme/agent:1')).toBeTruthy()
  })

  it('says so when it names no repository and runs the platform\'s image', () => {
    render(<ExperimentCode pe={{}} />)
    expect(screen.getByText('Repository').nextElementSibling!.textContent).toBe('none')
    expect(screen.getByText('Agent image').nextElementSibling!.textContent).toBe('the platform\'s')
    expect(screen.queryByRole('link')).toBeNull()
  })
})
