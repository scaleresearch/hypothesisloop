import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

/** Picks an option of a Select the way a person does: opens it, clicks the row. */
export async function choose(field: HTMLElement, option: string): Promise<void> {
  await userEvent.click(field)
  await userEvent.click(await screen.findByRole('option', { name: option }))
}
