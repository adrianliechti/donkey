import { useState } from 'react'
import { KeyRound, Loader2 } from 'lucide-react'
import { api, clearToken, setToken } from './api'
import { Button, ErrorNote, errText } from './ui'

export function Login({ onConnected }: { onConnected: () => void }) {
  const [token, setTokenInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function connect(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    // The token is only kept once the server accepted it — a failed attempt
    // must not leave a broken session behind for the next page load.
    setToken(token.trim())
    try {
      await api.ping()
      onConnected()
    } catch (err) {
      clearToken()
      setError(errText(err, 'That did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-sm">
        <div className="mb-8 text-center">
          <div className="text-5xl" aria-hidden>
            🫏
          </div>
          <h1 className="mt-3 font-mono text-2xl font-bold tracking-tight">donkey</h1>
          <p className="mt-1 text-sm text-stone">admin console</p>
        </div>
        <form
          onSubmit={connect}
          className="space-y-4 rounded-lg border border-line bg-card p-6 shadow-sm"
        >
          <label className="block">
            <span className="mb-1.5 flex items-center gap-1.5 text-sm font-medium">
              <KeyRound className="size-3.5 text-stone" aria-hidden /> API token
            </span>
            <input
              type="password"
              value={token}
              onChange={(e) => setTokenInput(e.target.value)}
              placeholder="dk_…"
              autoFocus
              disabled={busy}
              className="w-full rounded-md border border-line bg-surface px-3 py-2 font-mono text-sm focus:outline-2 focus:outline-offset-1 focus:outline-hay-deep disabled:opacity-60"
            />
            <span className="mt-1.5 block text-xs text-stone">
              Printed by <code className="font-mono">donkey server install</code> — ask whoever set
              up your donkey.
            </span>
          </label>
          <ErrorNote message={error} />
          <Button type="submit" disabled={busy || token.trim() === ''}>
            {busy ? (
              <span className="flex items-center gap-2">
                <Loader2 className="size-4 animate-spin" aria-hidden /> Connecting…
              </span>
            ) : (
              'Connect'
            )}
          </Button>
        </form>
      </div>
    </main>
  )
}
