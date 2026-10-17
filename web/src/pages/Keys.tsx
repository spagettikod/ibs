import { FormEvent, useState } from 'react'
import { AccessKey, api, Bucket, State } from '../api'
import { Card, Copy, Empty, ErrorBox, fmtDate, Modal, useLoader } from '../ui'

type KeyForm = { name: string; access: 'full' | 'read'; buckets: string[] | null }

/** What a key may do, in a few words. */
function accessText(k: { access: string; buckets: string[] | null }) {
  const what = k.access === 'read' ? 'Read only' : 'Full'
  if (!k.buckets) return `${what}, all buckets`
  return `${what}, ${k.buckets.length === 1 ? 'bucket' : 'buckets'} ${k.buckets.join(', ')}`
}

/** The settings of a key: description, access, buckets. */
function KeyFields({ form, setForm, buckets }: { form: KeyForm; setForm: (f: KeyForm) => void; buckets: string[] }) {
  const toggle = (b: string, on: boolean) => {
    const set = new Set(form.buckets ?? [])
    if (on) set.add(b)
    else set.delete(b)
    setForm({ ...form, buckets: [...set].sort() })
  }
  return (
    <div className="stack">
      <label>Description<input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="backup script" /></label>
      <label>
        Access
        <select value={form.access} onChange={(e) => setForm({ ...form, access: e.target.value as KeyForm['access'] })}>
          <option value="full">Full: read, upload and delete</option>
          <option value="read">Read only: list and download</option>
        </select>
      </label>
      <div className="stack">
        <label className="check">
          <input type="radio" checked={form.buckets === null} onChange={() => setForm({ ...form, buckets: null })} />
          <span>All buckets, also ones created later</span>
        </label>
        <label className="check">
          <input type="radio" checked={form.buckets !== null} onChange={() => setForm({ ...form, buckets: form.buckets ?? [] })} />
          <span>Only these buckets</span>
        </label>
        {form.buckets !== null && (
          buckets.length === 0 ? <p className="muted small">No buckets yet: create one on the Buckets page first.</p> : (
            <div className="stack key-buckets">
              {buckets.map((b) => (
                <label key={b} className="check">
                  <input type="checkbox" checked={form.buckets!.includes(b)} onChange={(e) => toggle(b, e.target.checked)} />
                  <span className="mono">{b}</span>
                </label>
              ))}
            </div>
          )
        )}
      </div>
    </div>
  )
}

export default function KeysPage() {
  const { data, error, reload } = useLoader(() => api<AccessKey[]>('GET', '/api/keys'), ['data'])
  const bucketList = useLoader(() => api<Bucket[]>('GET', '/api/buckets'), ['data'])
  const buckets = (bucketList.data ?? []).map((b) => b.name)
  const [form, setForm] = useState<KeyForm>({ name: '', access: 'full', buckets: null })
  const [created, setCreated] = useState<AccessKey>()
  const [createError, setCreateError] = useState<string>()
  const [editing, setEditing] = useState<{ id: string; form: KeyForm }>()
  const [editError, setEditError] = useState<string>()

  const create = async (e: FormEvent) => {
    e.preventDefault()
    setCreateError(undefined)
    try {
      const k = await api<AccessKey>('POST', '/api/keys', form)
      setCreated(k)
      setForm({ name: '', access: 'full', buckets: null })
      reload()
    } catch (err) {
      setCreateError((err as Error).message)
    }
  }
  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (!editing) return
    setEditError(undefined)
    try {
      await api('PUT', `/api/keys/${editing.id}`, editing.form)
      setEditing(undefined)
      reload()
    } catch (err) {
      setEditError((err as Error).message)
    }
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
      <p className="muted">
        S3 clients authenticate with an access key using AWS Signature Version 4. A key can be limited to reading, and to
        some buckets. The S3 endpoint is <span className="mono">{endpoint}</span>.
      </p>
      <Card title="Create access key">
        <form className="stack narrow" onSubmit={create}>
          <KeyFields form={form} setForm={setForm} buckets={buckets} />
          <ErrorBox error={createError} />
          <div><button className="btn primary">Create</button></div>
        </form>
        {created && (
          <div className="stack secret-box">
            <p><strong>Copy the secret now.</strong> It is not shown again.</p>
            <div className="kv"><span>Access key ID</span><code>{created.id}</code><Copy text={created.id} /></div>
            <div className="kv"><span>Secret access key</span><code>{created.secret}</code><Copy text={created.secret!} /></div>
            <div className="kv"><span>May</span><span>{accessText(created)}</span></div>
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
            <thead><tr><th>Access key ID</th><th>Description</th><th>May</th><th>Created</th><th /></tr></thead>
            <tbody>
              {data?.map((k) => (
                <tr key={k.id}>
                  <td className="mono">{k.id}</td>
                  <td>{k.name || <span className="muted">—</span>}</td>
                  <td>{accessText(k)}</td>
                  <td>{fmtDate(k.created)}</td>
                  <td className="right">
                    <div className="row-end">
                      <button className="btn small" onClick={() => { setEditError(undefined); setEditing({ id: k.id, form: { name: k.name, access: k.access, buckets: k.buckets } }) }}>Edit</button>
                      <button className="btn small danger" onClick={() => remove(k)}>Delete</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      {editing && (
        <Modal title={`Access key ${editing.id}`} onClose={() => setEditing(undefined)}>
          <form className="stack" onSubmit={save}>
            <KeyFields form={editing.form} setForm={(f) => setEditing({ ...editing, form: f })} buckets={buckets} />
            <p className="muted small">The secret stays the same; clients keep working with the new limits.</p>
            <ErrorBox error={editError} />
            <div className="row"><button className="btn primary">Save</button><button type="button" className="btn" onClick={() => setEditing(undefined)}>Cancel</button></div>
          </form>
        </Modal>
      )}
    </div>
  )
}
