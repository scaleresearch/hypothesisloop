import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { platformCodeRef } from '@/lib/api'
import { CodeRef } from '@/components/code-bundle'

const sha = '0123456789abcdef0123456789abcdef01234567'

describe("a job's code_ref", () => {
  it('is shown as it is for a git remote', () => {
    render(<CodeRef codeRef={`https://github.com/acme/model.git@${sha}`} />)
    expect(screen.getByText(`https://github.com/acme/model.git@${sha}`)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(platformCodeRef(`https://github.com/acme/model.git@${sha}`)).toBeNull()
  })

  // With no repository the code_ref was an address nobody could open in a browser.
  it('offers the commit as a git bundle when the platform keeps the code', () => {
    const ref = `https://api.example/platform-experiments/pe-1/agents/ag-1/code@${sha}`
    expect(platformCodeRef(ref)).toEqual({ platformExperimentID: 'pe-1', agentID: 'ag-1', sha })
    render(<CodeRef codeRef={ref} />)
    expect(screen.getByText(sha)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Download as a git bundle' })).toBeTruthy()
    expect(platformCodeRef(`https://api.example/platform-experiments/pe-1/agents/ag-1/code@main`)).toBeNull()
  })
})
