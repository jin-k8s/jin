import { useState } from 'react'
import { CircleCheck, Plus, Search, X } from 'lucide-react'
import { api, useResource } from '../api'
import { Button, ErrorBox, Field, Select, Spinner, cx, inputCls } from './ui'

const regions = [
  'ap-south-1', 'ap-south-2', 'ap-southeast-1', 'ap-southeast-2', 'ap-northeast-1', 'ap-northeast-2', 'ap-east-1',
  'us-east-1', 'us-east-2', 'us-west-1', 'us-west-2', 'ca-central-1', 'sa-east-1',
  'eu-west-1', 'eu-west-2', 'eu-west-3', 'eu-central-1', 'eu-central-2', 'eu-north-1', 'eu-south-1', 'me-south-1', 'me-central-1', 'af-south-1',
]

export function AddClusterDialog({ onClose, onAdded }: { onClose: () => void; onAdded: () => void }) {
  const profiles = useResource(api.awsProfiles, [])
  const [profile, setProfile] = useState('')
  const [region, setRegion] = useState('ap-south-1')
  const [roleArn, setRoleArn] = useState('')
  const [clusters, setClusters] = useState<string[]>()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState<'find' | 'add'>()
  const [error, setError] = useState<Error>()
  const [added, setAdded] = useState<{ context: string; version: string }>()

  const find = async () => {
    setBusy('find')
    setError(undefined)
    setClusters(undefined)
    setName('')
    try {
      const r = await api.eksClusters(region, profile, roleArn.trim())
      setClusters(r.clusters)
      if (r.clusters.length === 1) setName(r.clusters[0])
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(undefined)
    }
  }
  const add = async () => {
    setBusy('add')
    setError(undefined)
    try {
      const r = await api.addCluster({ provider: 'eks', name, region, profile, roleArn: roleArn.trim() })
      setAdded(r)
      onAdded()
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 backdrop-blur-sm" role="dialog" aria-modal="true">
      <div className="w-full max-w-lg rounded-xl border border-line bg-panel shadow-xl">
        <div className="flex items-center justify-between border-b border-line px-5 py-4">
          <h2 className="font-semibold">Add an EKS cluster</h2>
          <button onClick={onClose} className="rounded-md p-1 text-muted hover:bg-panel-2" aria-label="Close">
            <X className="size-4" />
          </button>
        </div>

        {added ? (
          <div className="space-y-4 px-5 py-6">
            <div className="flex items-start gap-3 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-800 dark:text-emerald-200">
              <CircleCheck className="mt-0.5 size-4 shrink-0" />
              <div>
                <p className="font-semibold">Connected to {name}</p>
                <p>
                  Kubernetes <span className="font-mono">{added.version}</span>. It now appears in the fleet as <span className="font-mono">{added.context}</span>.
                </p>
              </div>
            </div>
            <div className="flex justify-end">
              <Button variant="primary" onClick={onClose}>
                Done
              </Button>
            </div>
          </div>
        ) : (
          <>
            <div className="space-y-4 px-5 py-5">
              <p className="text-sm text-muted">
                Jin connects with the AWS credentials available to the server. No kubeconfig or access keys are needed. The identity needs an EKS access
                entry on the cluster.
              </p>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="AWS profile">
                  <Select value={profile} onChange={setProfile}>
                    <option value="">Default credentials</option>
                    {profiles.data?.profiles.map((p) => (
                      <option key={p} value={p}>
                        {p}
                      </option>
                    ))}
                  </Select>
                </Field>
                <Field label="Region">
                  <Select value={region} onChange={setRegion}>
                    {regions.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </Select>
                </Field>
              </div>
              <Field label="Role to assume (optional)" hint="For clusters in another account: arn:aws:iam::<account>:role/<name>">
                <input value={roleArn} onChange={(e) => setRoleArn(e.target.value)} placeholder="arn:aws:iam::123456789012:role/jin" className={cx(inputCls, 'font-mono')} />
              </Field>
              <Button onClick={find} loading={busy === 'find'} icon={<Search className="size-4" />}>
                Find clusters
              </Button>

              {clusters && clusters.length === 0 && <p className="text-sm text-muted">No EKS clusters in {region} for these credentials.</p>}
              {!!clusters?.length && (
                <div className="max-h-48 overflow-y-auto rounded-lg border border-line">
                  {clusters.map((c) => (
                    <label key={c} className={cx('flex cursor-pointer items-center gap-3 px-4 py-2.5 text-sm hover:bg-panel-2', name === c && 'bg-brand/5')}>
                      <input type="radio" name="cluster" checked={name === c} onChange={() => setName(c)} className="accent-brand" />
                      <span className="font-mono">{c}</span>
                    </label>
                  ))}
                </div>
              )}
              {busy === 'add' && <Spinner label="Connecting to the cluster…" />}
              {error && <ErrorBox error={error} title="Could not add the cluster" />}
            </div>
            <div className="flex justify-end gap-2 border-t border-line px-5 py-4">
              <Button onClick={onClose}>Cancel</Button>
              <Button variant="primary" onClick={add} disabled={!name} loading={busy === 'add'} icon={<Plus className="size-4" />}>
                Add and test connection
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
