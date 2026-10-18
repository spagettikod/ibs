import { FormEvent, useEffect, useRef, useState } from 'react'
import { api, Bucket, ObjectInfo, StorableCapacity } from '../api'
import { subscribe } from '../events'
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
