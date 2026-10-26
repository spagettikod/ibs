import { FormEvent, useEffect, useRef, useState } from 'react'
import { api, Bucket, ObjectInfo, StorableCapacity } from '../api'
import { subscribe } from '../events'
import { ask, Card, Counted, Empty, ErrorBox, fmtBytes, fmtDate, Spinner, tell, useLoader } from '../ui'

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
  const [replicas, setReplicas] = useState(3)
  const capacity = useLoader(() => api<StorableCapacity>('GET', `/api/capacity?replicas=${replicas}`), ['data', 'nodes', 'peers', 'replication'])
  useEffect(() => { capacity.reload() }, [replicas]) // eslint-disable-line react-hooks/exhaustive-deps
  const [name, setName] = useState('')
  const [formError, setFormError] = useState<string>()
  const setCopies = async (b: Bucket, n: number) => {
    if (n < b.replicas && !(await ask(`Fewer copies of ${b.name}`, n === 1
      ? "Its files will only be at this site: if the site is lost, they're lost too. The copies on other sites are deleted."
      : 'The copies on other sites that are no longer needed are deleted.', { ok: `Keep ${n}`, danger: true }))) return
    try {
      await api('PUT', `/api/buckets/${encodeURIComponent(b.name)}`, { replicas: n })
      reload()
    } catch (err) {
      tell("Couldn't change the copies", (err as Error).message)
    }
  }

  const create = async (e: FormEvent) => {
    e.preventDefault()
    try {
      await api('POST', '/api/buckets', { name, replicas })
      setName('')
      setFormError(undefined)
      reload()
    } catch (err) {
      setFormError((err as Error).message)
    }
  }
  const remove = async (b: Bucket) => {
    if (!(await ask(`Delete bucket ${b.name}`, 'The bucket must be empty.', { ok: 'Delete', danger: true }))) return
    try {
      await api('DELETE', `/api/buckets/${encodeURIComponent(b.name)}`)
      reload()
    } catch (err) {
      tell("Couldn't delete the bucket", (err as Error).message)
    }
  }

  return (
    <div className="page">
      <header className="page-head"><h1>Buckets</h1></header>
      <Card title="Create bucket">
        <form className="inline-form" onSubmit={create}>
          <label>Name<input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="my-bucket" /></label>
          <label>Copies<CopiesSelect value={replicas} onChange={setReplicas} /></label>
          <button className="btn primary" disabled={!name}>Create</button>
        </form>
        {replicas === 1
          ? <p className="small warn-text">Files are only at this site: if it's lost, they're lost too.</p>
          : capacity.data && <StorableHint c={capacity.data} want={replicas - 1} />}
        <p className="muted small">
          Each file is kept here, and encrypted with the bucket's key on other sites in the IBS, wherever there is room.
          The number of copies can be changed later.
        </p>
        <ErrorBox error={formError} />
      </Card>
      <ErrorBox error={error} />
      <Card>
        {data && data.length === 0 ? (
          <Empty>No buckets yet.</Empty>
        ) : (
          <table>
            <thead><tr><th>Bucket</th><th>Objects</th><th>Size</th><th>Copies</th><th>Created</th><th /></tr></thead>
            <tbody>
              {data?.map((b) => (
                <tr key={b.name}>
                  <td><a href="#" onClick={(e) => { e.preventDefault(); go(b.name) }}>{b.name}</a></td>
                  <td><Counted ok={b.counted}>{b.objects.toLocaleString()}</Counted>
                    {b.activity && <> <Spinner title={b.activity === 'writing' ? 'Files arriving…' : 'Files being deleted…'} /></>}</td>
                  <td><Counted ok={b.counted}>{fmtBytes(b.size)}</Counted></td>
                  <td><CopiesSelect value={b.replicas} onChange={(n) => setCopies(b, n)} />
                    <Counted ok={b.counted}><ReplicationStatus b={b} /></Counted></td>
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

/** The number of copies of a bucket's files, this site's included. */
function CopiesSelect({ value, onChange }: { value: number; onChange: (n: number) => void }) {
  return (
    <select value={value} onChange={(e) => onChange(Number(e.target.value))}>
      <option value={1}>1: only this site</option>
      <option value={2}>2: here and one other site</option>
      <option value={3}>3: here and two other sites</option>
    </select>
  )
}

/** How much more this site can store right now, with want copies on other sites. */
function StorableHint({ c, want }: { c: StorableCapacity; want: number }) {
  const short = c.limited_by === 'sites'
  const copies = (n: number) => `${n} ${n === 1 ? 'copy' : 'copies'}`
  let detail: string
  if (c.sites === 0) {
    detail = `Files have no copies elsewhere yet: no other site is online. They're stored here, and the copies are made when other sites are available.`
  } else if (short) {
    detail = `Files get ${copies(c.copies)} on other sites for now, not ${want}: only ${c.sites} other site is online. The rest is made when another site is available.`
  } else if (c.limited_by === 'ibs') {
    detail = `Limited by the space other sites offer for the ${copies(want)}. This site alone has room for ${fmtBytes(c.local)}.`
  } else {
    detail = `Limited by this site's space. Other sites have room for the ${copies(want)}.`
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

/** Whether a bucket's files have their copies on other sites. */
function ReplicationStatus({ b }: { b: Bucket }) {
  if (b.replicas === 1) return null
  if (b.short) return <div className="small warn-text">Not possible: the IBS has too few sites</div>
  if (b.pending > 0) return <div className="small muted">{b.pending.toLocaleString()} blocks being copied</div>
  return <div className="small muted">all in place</div>
}

// A folder of the browser in the chosen order: all subfolders (with the first
// page), then a page of files. latest: unix ms. next: offset of the next page, 0 at the end.
type FolderEntry = { prefix: string; counted: boolean; objects: number; bytes: number; latest: number }
type Listing = { folders: FolderEntry[]; objects: ObjectInfo[]; files: number; next: number }
const PAGE = 2000
const MAX_PAGE = 10000 // the most the site sends at once

type SortKey = 'name' | 'size' | 'modified'
type Sort = { key: SortKey; desc: boolean }

/** The browser's sort order, remembered in this browser. */
function useSort(): [Sort, (k: SortKey) => void] {
  const [sort, setSort] = useState<Sort>(() => {
    try {
      const s = JSON.parse(localStorage.getItem('ibs-browser-sort') ?? '')
      if (['name', 'size', 'modified'].includes(s.key)) return { key: s.key, desc: !!s.desc }
    } catch {
      // none saved, or storage unavailable
    }
    return { key: 'name', desc: false }
  })
  const choose = (key: SortKey) => {
    const next = { key, desc: sort.key === key ? !sort.desc : key !== 'name' } // size and date: largest and newest first
    setSort(next)
    try {
      localStorage.setItem('ibs-browser-sort', JSON.stringify(next))
    } catch {
      // not remembered; fine
    }
  }
  return [sort, choose]
}

function Browser({ bucket, prefix }: { bucket: string; prefix: string }) {
  const [sort, chooseSort] = useSort()
  // The site sorts the whole folder; more pages follow in the same order. A
  // reload fetches as many files as are shown, so loaded pages stay.
  const url = (offset: number, limit: number) => `/api/buckets/${encodeURIComponent(bucket)}/browse?prefix=${encodeURIComponent(prefix)}` +
    `&sort=${sort.key}&desc=${sort.desc ? 1 : 0}&offset=${offset}&limit=${limit}`
  const want = useRef(PAGE)
  const shown = useRef(0)
  const { data, error, reload } = useLoader(async () => {
    const l = await api<Listing>('GET', url(0, PAGE))
    while (l.next > 0 && l.objects.length < want.current) {
      const more = await api<Listing>('GET', url(l.next, Math.min(MAX_PAGE, want.current - l.objects.length)))
      l.objects.push(...more.objects)
      l.next = more.next
    }
    shown.current = l.objects.length
    return l
  })
  const [loadingMore, setLoadingMore] = useState(false)
  const [uploading, setUploading] = useState<string>()
  const [uploadError, setUploadError] = useState<string>()
  const fileInput = useRef<HTMLInputElement>(null)

  useEffect(() => {
    want.current = PAGE
    reload()
  }, [bucket, prefix, sort.key, sort.desc, reload])
  // Follow changes made elsewhere, but not for very long listings: fetching
  // them again on every change would keep the site busy.
  useEffect(() => subscribe((changed) => {
    if (changed.includes('data') && shown.current <= MAX_PAGE) reload()
  }), [reload])

  const load = async (all: boolean) => {
    want.current = all ? Infinity : (data?.objects.length ?? 0) + PAGE
    setLoadingMore(true)
    try {
      await reload()
    } finally {
      setLoadingMore(false)
    }
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
    if (!(await ask(`Delete ${o.key.slice(prefix.length)}`, "It is deleted here and on every other site, and can't be brought back.", { ok: 'Delete', danger: true }))) return
    await api('DELETE', `/api/buckets/${encodeURIComponent(bucket)}/objects?key=${encodeURIComponent(o.key)}`).catch((e) => tell("Couldn't delete the file", e.message))
    reload()
  }

  const removeFolder = async (p: string) => {
    const base = `/api/buckets/${encodeURIComponent(bucket)}/folder?prefix=${encodeURIComponent(p)}`
    try {
      const size = await api<{ objects: number; bytes: number }>('GET', base)
      if (!(await ask(`Delete folder ${p.slice(prefix.length)}`,
        `It holds ${size.objects.toLocaleString()} ${size.objects === 1 ? 'file' : 'files'} (${fmtBytes(size.bytes)}). ` +
        "They are deleted here and on every other site, and can't be brought back.", { ok: 'Delete', danger: true }))) return
      await api('DELETE', base)
    } catch (e) {
      tell("Couldn't delete the folder", (e as Error).message)
    }
    reload()
  }

  const crumbs = prefix.split('/').filter(Boolean)
  const folders = data?.folders ?? []
  const objects = data?.objects ?? []
  const next = data?.next ?? 0
  const header = (key: SortKey, label: string) => (
    <th className="sortable" aria-sort={sort.key === key ? (sort.desc ? 'descending' : 'ascending') : 'none'}>
      <button type="button" className="th-sort" onClick={() => chooseSort(key)}>
        {label}{sort.key === key && <span aria-hidden="true">{sort.desc ? ' ↓' : ' ↑'}</span>}
      </button>
    </th>
  )

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
        {data && objects.length === 0 && folders.length === 0 ? (
          <Empty>This {prefix ? 'folder' : 'bucket'} is empty. Upload files here or with any S3 client.</Empty>
        ) : (
          <table>
            <thead><tr>{header('name', 'Name')}{header('size', 'Size')}{header('modified', 'Modified')}<th /></tr></thead>
            <tbody>
              {folders.map((f) => (
                <tr key={f.prefix}>
                  <td><a href="#" onClick={(e) => { e.preventDefault(); go(bucket, f.prefix) }}>📁 {f.prefix.slice(prefix.length)}</a></td>
                  <td><Counted ok={f.objects > 0}>{fmtBytes(f.bytes)}
                    <div className="muted small">{f.objects.toLocaleString()} {f.objects === 1 ? 'file' : 'files'}</div></Counted></td>
                  <td title="The newest change to a file in this folder, at any depth">
                    <Counted ok={f.counted}>{new Date(f.latest).toLocaleString()}</Counted></td>
                  <td className="right row-end"><button className="btn small danger" onClick={() => removeFolder(f.prefix)}>Delete</button></td>
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
        {next > 0 && data && (
          <div className="more">
            <span className="muted small">{objects.length.toLocaleString()} of {data.files.toLocaleString()} files shown. </span>
            <button className="btn" onClick={() => load(false)} disabled={loadingMore}>Next {Math.min(PAGE, data.files - objects.length).toLocaleString()}</button>{' '}
            <button className="btn" onClick={() => load(true)} disabled={loadingMore}>{loadingMore ? 'Loading…' : 'Load all'}</button>
          </div>
        )}
      </Card>
    </div>
  )
}
