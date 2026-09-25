import { useState } from 'react'
import { CircleCheck, KeyRound, Lock, Trash2 } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, Card, ErrorBox, Field, PageHeader, Spinner, cx, inputCls } from '../components/ui'
import { dateTime } from '../format'
import { useCan } from '../session'

function GitHubCard() {
  const gh = useResource(api.github, [])
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [editing, setEditing] = useState(false)

  const save = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.saveGitHub(token.trim())
      setToken('')
      setEditing(false)
      gh.reload()
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }
  const remove = async () => {
    if (!window.confirm('Remove the stored GitHub token? GitOps upgrades will stop until a new token is added.')) return
    await api.removeGitHub()
    gh.reload()
  }

  const d = gh.data
  const stored = d?.configured && d.source === 'jin'
  return (
    <Card title={<span className="flex items-center gap-2"><KeyRound className="size-4" /> GitHub</span>}>
      <div className="space-y-4 p-5">
        <p className="text-sm text-muted">
          Used by GitOps upgrades to read your infrastructure repository and open pull requests. Use a fine-grained token limited to that repository,
          with <b>Contents</b> and <b>Pull requests</b> set to read and write.
        </p>
        {gh.loading && !d && <Spinner />}
        {d?.configured && !editing && (
          <div className="flex flex-wrap items-center gap-3 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm">
            <CircleCheck className="size-4 text-emerald-600" />
            {stored ? (
              <span>
                Token <span className="font-mono">{d.hint}</span> for <b>@{d.login}</b>, stored encrypted by {d.setBy} on {d.setAt && dateTime(d.setAt)}
                {d.scopes && <span className="text-muted"> · scopes: {d.scopes}</span>}
              </span>
            ) : (
              <span>
                Using <span className="font-mono">{d.source?.replace('env:', '$')}</span> from the server environment.
              </span>
            )}
            <div className="ml-auto flex gap-2">
              <Button onClick={() => setEditing(true)}>{stored ? 'Replace' : 'Store a token in Jin'}</Button>
              {stored && (
                <Button variant="ghost" onClick={remove} icon={<Trash2 className="size-4" />}>
                  Remove
                </Button>
              )}
            </div>
          </div>
        )}
        {(d && (!d.configured || editing)) && (
          <div className="space-y-3">
            <Field label="Personal access token" hint="Jin checks it with GitHub, then stores it encrypted. It is never shown again.">
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="github_pat_…"
                className={cx(inputCls, 'font-mono')}
              />
            </Field>
            <div className="flex gap-2">
              <Button variant="primary" onClick={save} loading={busy} disabled={token.trim().length < 20} icon={<Lock className="size-4" />}>
                Verify and save
              </Button>
              {editing && <Button onClick={() => setEditing(false)}>Cancel</Button>}
            </div>
            <a
              className="block text-xs text-brand hover:underline"
              href="https://github.com/settings/personal-access-tokens/new"
              target="_blank"
              rel="noopener noreferrer"
            >
              Create a fine-grained token on GitHub →
            </a>
          </div>
        )}
        {error && <ErrorBox error={error} title="Token not saved" />}
      </div>
    </Card>
  )
}

export function Integrations() {
  const can = useCan()
  return (
    <>
      <PageHeader title="Integrations" subtitle="Credentials Jin uses to act on your behalf. Secrets are encrypted at rest and never returned by the API." />
      {can('admin') ? <GitHubCard /> : <p className="text-sm text-muted">Only administrators can manage integrations.</p>}
      <p className="mt-6 text-xs text-muted">
        Cloud access (AWS, Google Cloud, Azure) comes from the credentials available to the Jin server, such as AWS profiles, SSO sessions or
        instance roles. Jin never asks for cloud access keys.
      </p>
    </>
  )
}
