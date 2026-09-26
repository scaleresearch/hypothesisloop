import { redirect } from 'next/navigation'

/** Home is the experiments page. The query goes along: it names the project. */
export default function Home({ searchParams }: { searchParams: Record<string, string | string[] | undefined> }) {
  const query = new URLSearchParams(Object.entries(searchParams).flatMap(([key, value]) => [value ?? ''].flat().map(v => [key, v])))
  redirect(`/platform-experiments${query.size > 0 ? `?${query}` : ''}`)
}
