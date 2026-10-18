import { FormEvent, useEffect, useRef, useState } from 'react'
import { api, Bucket, ObjectInfo, StorableCapacity } from '../api'
import { Card, Counted, Empty, ErrorBox, fmtBytes, fmtDate, Spinner, useLoader } from '../ui'

function route(): { bucket?: string; prefix: string } {
  const parts = window.location.hash.replace(/^#\/?/, '').split('/')
  if (parts[0] !== 'buckets' || !parts[1]) return { prefix: '' }
  return { bucket: decodeURIComponent(parts[1]), prefix: decodeURIComponent(parts.slice(2).join('/')) }
}

function go(bucket?: string, prefix = '') {
  window.location.hash = bucket ? `#/buckets/${encodeURIComponent(bucket)}/${prefix.split('/').map(encodeURIComponent).join('/')}` : '#/buckets'
}

export default function BucketsPage() {
  const [r, setR] = useState(route)
  useEffect(() => {
    const onHash = () => setR(route())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])
  return r.bucket ? <Browser bucket={r.bucket} prefix={r.prefix} /> : <BucketList />
}

function BucketList() {
  const { data, error, reload } = useLoader(() => api<Bucket[]>('GET', '/api/buckets'), ['data', 'replication', 'peers'])
  const capacity = useLoader(() => api<StorableCapacity>('GET', '/api/capacity'), ['data', 'nodes', 'peers', 'replication'])
  const sites = capacity.data?.sites
  const [name, setName] = useState('')
  const [formError, setFormError] = useState<string>()

  const create = async (e: FormEvent) => {
    e.preventDefault()
    try {
      await api('POST', '/api/buckets', { name })
      setName('')
      setFormError(undefined)
      reload()
    } catch (err) {
      setFormError((err as Error).message)
    }
  }
  const remove = async (b: Bucket) => {
    if (!confirm(`Delete bucket "${b.name}"? It must be empty.`)) return
    try {
      await api('DELETE', `/api/buckets/${encodeURIComponent(b.name)}`)
      reload()
    } catch (err) {
      alert((err as Error).message)
    }
  }

  return (
    <div className="page">
      <header className="page-head"><h1>Buckets</h1></header>
      <Card title="Create bucket">
        <form className="inline-form" onSubmit={create}>
          <label>Name<input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="my-bucket" /></label>
          <button className="btn primary" disabled={!name}>Create</button>
        </form>
        {capacity.data && <StorableHint c={capacity.data} />}
        <p className="muted small">
          Every file is kept three times: here, and encrypted with the bucket's key on two other sites in the IBS, wherever
          there is room.
        </p>
        <ErrorBox error={formError} />
      </Card>
      <ErrorBox error={error} />
      <Card>
        {data && data.length === 0 ? (
          <Empty>No buckets yet.</Empty>
        ) : (
          <table>
            <thead><tr><th>Bucket</th><th>Objects</th><th>Size</th><th>Copies on other sites</th><th>Created</th><th /></tr></thead>
            <tbody>
              {data?.map((b) => (
                <tr key={b.name}>
                  <td><a href="#" onClick={(e) => { e.preventDefault(); go(b.name) }}>{b.name}</a></td>
                  <td><Counted ok={b.counted}>{b.objects.toLocaleString()}</Counted>
                    {b.activity && <> <Spinner title={b.activity === 'writing' ? 'Files arriving…' : 'Files being deleted…'} /></>}</td>
                  <td><Counted ok={b.counted}>{fmtBytes(b.size)}</Counted></td>
                  <td><Counted ok={b.counted}><ReplicationStatus b={b} sites={sites} /></Counted></td>
                  <td>{fmtDate(b.created)}</td>
                  <td className="right"><button className="btn small danger" onClick={() => remove(b)}>Delete</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  )
}

/** How much more this site can store right now, with its copies on other sites. */
function StorableHint({ c }: { c: StorableCapacity }) {
  const short = c.limited_by === 'sites'
  const want = 2
  let detail: string
  if (c.sites === 0) {
    detail = `Warning: no other site is online, so files have no copies elsewhere yet. They're stored here, and the copies are made when other sites are available.`
  } else if (short) {
    detail = `Warning: files should have ${want} copies on other sites, but only ${c.sites} other site is online. Files are stored with ${c.copies} copy for now; the second is made when another site is available.`
  } else if (c.limited_by === 'ibs') {
    detail = `Limited by the space other sites offer for the ${want} copies. This site alone has room for ${fmtBytes(c.local)}.`
  } else {
    detail = `Limited by this site's space. Other sites have room for the ${want} copies.`
  }
  return (
    <div className={`storable ${short ? 'warn' : ''}`}>
      <div>
        <span className="muted small">Can be stored right now (estimate)</span>
        <div className="storable-value">{fmtBytes(c.storable)}</div>
        {short && <span className="small warn-text">{c.copies} of {want} copies</span>}
      </div>
      <p className="small">{detail}</p>
    </div>
  )
}

function ReplicationStatus({ b, sites }: { b: Bucket; sites?: number }) {
  if (sites !== undefined && b.replication > sites) {
    return (
      <span className="pill warn" title={`Only ${sites} other site${sites === 1 ? ' is' : 's are'} online; the missing copies are made when more sites are available.`}>
        {sites} of {b.replication}
      </span>
    )
  }
  if (b.pending > 0) return <span className="pill warn">{b.pending} blocks pending</span>
  return <span className="pill ok">{b.replication} of {b.replication}</span>
}

type FolderInfo = { objects: number; bytes: number; latest: number } // latest: unix ms, -1 while recalculated
type Listing = { objects: ObjectInfo[]; prefixes: string[]; folders: Record<string, FolderInfo>; truncated: boolean; next: string }

function Browser({ bucket, prefix }: { bucket: string; prefix: string }) {
  const [extra, setExtra] = useState<ObjectInfo[]>([])
  const [next, setNext] = useState<string>()
  const { data, error, reload } = useLoader(
    () => api<Listing>('GET', `/api/buckets/${encodeURIComponent(bucket)}/objects?prefix=${encodeURIComponent(prefix)}`),
    ['data'],
  )
  const [uploading, setUploading] = useState<string>()
  const [uploadError, setUploadError] = useState<string>()
  const fileInput = useRef<HTMLInputElement>(null)

  useEffect(() => {
    setExtra([])
    setNext(undefined)
    reload()
  }, [bucket, prefix, reload])
  useEffect(() => setNext(data?.truncated ? data.next : undefined), [data])

  const loadMore = async () => {
    if (!next) return
    const more = await api<Listing>('GET', `/api/buckets/${encodeURIComponent(bucket)}/objects?prefix=${encodeURIComponent(prefix)}&marker=${encodeURIComponent(next)}`)
    setExtra((x) => [...x, ...more.objects])
    setNext(more.truncated ? more.next : undefined)
  }

  const upload = async (files: FileList | null) => {
    if (!files?.length) return
    setUploadError(undefined)
    for (const f of Array.from(files)) {
      setUploading(f.name)
      const fd = new FormData()
      fd.append('file', f, f.name)
      try {
        await api('POST', `/api/buckets/${encodeURIComponent(bucket)}/upload?prefix=${encodeURIComponent(prefix)}`, fd)
      } catch (err) {
        setUploadError(`${f.name}: ${(err as Error).message}`)
        break
      }
    }
    setUploading(undefined)
    if (fileInput.current) fileInput.current.value = ''
    reload()
  }

  const remove = async (o: ObjectInfo) => {
    if (!confirm(`Delete "${o.key}"? It is deleted here and on every other site, and can't be brought back.`)) return
    await api('DELETE', `/api/buckets/${encodeURIComponent(bucket)}/objects?key=${encodeURIComponent(o.key)}`).catch((e) => alert(e.message))
    reload()
  }

  const removeFolder = async (p: string) => {
    const base = `/api/buckets/${encodeURIComponent(bucket)}/folder?prefix=${encodeURIComponent(p)}`
    try {
      const size = await api<{ objects: number; bytes: number }>('GET', base)
      if (!confirm(`Delete the folder "${p.slice(prefix.length)}" with ${size.objects.toLocaleString()} ${size.objects === 1 ? 'file' : 'files'} (${fmtBytes(size.bytes)})?\n\n` +
        'They are deleted here and on every other site, and can\'t be brought back.')) return
      await api('DELETE', base)
    } catch (e) {
      alert((e as Error).message)
    }
    reload()
  }

  const crumbs = prefix.split('/').filter(Boolean)
  const objects = [...(data?.objects ?? []), ...extra]

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="crumbs">
            <a href="#/buckets">Buckets</a>
            <span>/</span>
            <a href="#" onClick={(e) => { e.preventDefault(); go(bucket) }}>{bucket}</a>
            {crumbs.map((c, i) => (
              <span key={i}>
                <span>/</span>
                <a href="#" onClick={(e) => { e.preventDefault(); go(bucket, crumbs.slice(0, i + 1).join('/') + '/') }}>{c}</a>
              </span>
            ))}
          </div>
        </div>
        <div className="row">
          <input ref={fileInput} type="file" multiple hidden onChange={(e) => upload(e.target.files)} />
          <button className="btn primary" disabled={!!uploading} onClick={() => fileInput.current?.click()}>
            {uploading ? `Uploading ${uploading}…` : 'Upload files'}
          </button>
        </div>
      </header>
      <ErrorBox error={error ?? uploadError} />
      <Card>
        {data && objects.length === 0 && data.prefixes.length === 0 ? (
          <Empty>This {prefix ? 'folder' : 'bucket'} is empty. Upload files here or with any S3 client.</Empty>
        ) : (
          <table>
            <thead><tr><th>Name</th><th>Size</th><th>Modified</th><th /></tr></thead>
            <tbody>
              {data?.prefixes.map((p) => (
                <tr key={p}>
                  <td><a href="#" onClick={(e) => { e.preventDefault(); go(bucket, p) }}>📁 {p.slice(prefix.length)}</a></td>
                  <td><Counted ok={!!data?.folders?.[p]}>{fmtBytes(data?.folders?.[p]?.bytes ?? 0)}
                    <div className="muted small">{(data?.folders?.[p]?.objects ?? 0).toLocaleString()} {data?.folders?.[p]?.objects === 1 ? 'file' : 'files'}</div></Counted></td>
                  <td title="The newest change to a file in this folder, at any depth">
                    <Counted ok={(data?.folders?.[p]?.latest ?? -1) >= 0}>{new Date(data?.folders?.[p]?.latest ?? 0).toLocaleString()}</Counted></td>
                  <td className="right row-end"><button className="btn small danger" onClick={() => removeFolder(p)}>Delete</button></td>
                </tr>
              ))}
              {objects.map((o) => (
                <tr key={o.key}>
                  <td className="name-cell" title={o.key}>{o.key.slice(prefix.length)}</td>
                  <td>{fmtBytes(o.size)}</td>
                  <td>{fmtDate(o.modified)}</td>
                  <td className="right row-end">
                    <a className="btn small" href={`/api/buckets/${encodeURIComponent(bucket)}/download?key=${encodeURIComponent(o.key)}`}>Download</a>
                    <button className="btn small danger" onClick={() => remove(o)}>Delete</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {next && <div className="more"><button className="btn" onClick={loadMore}>Load more</button></div>}
      </Card>
    </div>
  )
}
