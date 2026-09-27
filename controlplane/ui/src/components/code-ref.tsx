import { textLinkClass } from '@/components/ui/link'

// A code_ref is repository_url@sha, and a branch of a platform experiment is hl/<its id>/<name>.
function parts(codeRef: string) {
  const at = codeRef.lastIndexOf('@')
  return { repository: codeRef.slice(0, at), sha: codeRef.slice(at + 1) }
}

/** A branch by its name within its platform experiment, linked to it in the repository its commit is in. */
export function Branch({ branch, codeRef, platformExperimentID }: { branch: string; codeRef: string; platformExperimentID: string }) {
  return (
    <a
      href={`${parts(codeRef).repository}/tree/${branch}`}
      target="_blank"
      rel="noreferrer"
      className={textLinkClass}
      title={branch}
      onClick={e => e.stopPropagation()}
    >
      {branch.slice(`hl/${platformExperimentID}/`.length)}
    </a>
  )
}

/** A commit by the start of its sha, linked to it in its repository. */
export function Commit({ codeRef }: { codeRef: string }) {
  const { repository, sha } = parts(codeRef)
  return (
    <a href={`${repository}/commit/${sha}`} target="_blank" rel="noreferrer" className={textLinkClass} title={codeRef}>
      {sha.slice(0, 8)}
    </a>
  )
}
