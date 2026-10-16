// The UI browser run's stand-in sign-in service, serving the graph-1k stack:
//   node identity-provider.mjs PORT KEY_PATH UI_ORIGIN
import { startIdentityProvider } from '../../controlplane/ui/e2e/identity-provider.mjs'

const [port, keyPath, ui] = process.argv.slice(2)
await startIdentityProvider({ port: Number(port), keyPath, ui })
