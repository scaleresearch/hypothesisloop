/** The product mark and name, as the nav and the sign-in page show it. */
export function Logo() {
  return (
    <span className="flex items-center gap-2.5 text-lg font-medium tracking-tight text-foreground">
      <span aria-hidden="true" className="flex h-[30px] w-[30px] flex-none items-center justify-center rounded bg-primary text-base font-medium text-primary-foreground">O</span>
      HypothesisLoop
    </span>
  )
}
