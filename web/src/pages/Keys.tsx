import { FormEvent, useState } from 'react'
import { AccessKey, api, State } from '../api'
import { Card, Copy, Empty, ErrorBox, fmtDate, useLoader } from '../ui'

export default function KeysPage() {
  const { data, error, reload } = useLoader(() => api<AccessKey[]>('GET', '/api/keys'), ['data'])
  const [name, setName] = useState('')
  const [created, setCreated] = useState<AccessKey>()

  const create = async (e: FormEvent) => {
    e.preventDefault()
    const k = await api<AccessKey>('POST', '/api/keys', { name })
    setCreated(k)
    setName('')
    reload()
  }
  const remove = async (k: AccessKey) => {
    if (!confirm(`Delete access key ${k.id}? Clients using it stop working.`)) return
    await api('DELETE', `/api/keys/${k.id}`)
    reload()
  }

  const state = useLoader(() => api<State>('GET', '/api/state'))
  const s3Port = state.data?.s3_port || '9000'
  const endpoint = `${window.location.protocol}//${window.location.hostname}:${s3Port}`
  const example = created
    ? `export AWS_ACCESS_KEY_ID=${created.id}
export AWS_SECRET_ACCESS_KEY=${created.secret}
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url ${endpoint} s3 ls`
    : ''

  return (
    <div className="page">
      <header className="page-head"><h1>Access keys</h1></header>
      <p className="muted">S3 clients authenticate with an access key using AWS Signature Version 4. The S3 endpoint is <span className="mono">{endpoint}</span>.</p>
      <Card title="Create access key">
        <form className="inline-form" onSubmit={create}>
          <label>Description<input value={name} onChange={(e) => setName(e.target.value)} placeholder="backup script" /></label>
          <button className="btn primary">Create</button>
        </form>
        {created && (
          <div className="stack secret-box">
            <p><strong>Copy the secret now.</strong> It is not shown again.</p>
            <div className="kv"><span>Access key ID</span><code>{created.id}</code><Copy text={created.id} /></div>
            <div className="kv"><span>Secret access key</span><code>{created.secret}</code><Copy text={created.secret!} /></div>
            <pre className="code">{example}</pre>
            <div className="row"><Copy text={example} label="Copy example" /><button className="btn small" onClick={() => setCreated(undefined)}>Done</button></div>
          </div>
        )}
      </Card>
      <ErrorBox error={error} />
      <Card>
        {data?.length === 0 ? (
          <Empty>No access keys yet.</Empty>
        ) : (
          <table>
            <thead><tr><th>Access key ID</th><th>Description</th><th>Created</th><th /></tr></thead>
            <tbody>
              {data?.map((k) => (
                <tr key={k.id}>
                  <td className="mono">{k.id}</td>
                  <td>{k.name || <span className="muted">—</span>}</td>
                  <td>{fmtDate(k.created)}</td>
                  <td className="right"><button className="btn small danger" onClick={() => remove(k)}>Delete</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  )
}
