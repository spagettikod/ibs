import { ReactNode, useEffect, useRef, useState } from 'react'
import { api, State, UpdatesInfo, VersionInfo } from '../api'
import { ago, ask, Card, Copy, Empty, ErrorBox, fmtDate, Modal, useLoader, useSubmit } from '../ui'

/** Settings → Updates: versions, where to look for new ones, uploads and keys. */
export default function UpdatesSection() {
  const { data, error, reload } = useLoader(() => api<UpdatesInfo>('GET', '/api/updates'), ['peers'], 30000)
  const [msg, setMsg] = useState<string>()
  const [actionError, setActionError] = useState<string>()
  const [dialog, setDialog] = useState<'upload' | 'key'>()
  const [notes, setNotes] = useState<VersionInfo>()
  const [checking, setChecking] = useState(false)
  const [checked, setChecked] = useState<string>()
  const [restarting, setRestarting] = useState<string>()

  // After Install: wait for IBS to restart, then reload into the new
  // version. If the old version is back instead, the new one didn't start.
  const startedBefore = useRef<number>()
  useEffect(() => {
    if (!restarting) return
    let stopped = false
    let timer = 0
    const since = Date.now()
    const poll = async () => {
      try {
        const s = await api<State>('GET', '/api/state')
        if (stopped) return
        if (s.version === restarting) {
          window.location.reload()
          return
        }
        if (s.started !== startedBefore.current || Date.now() - since > 120_000) {
          setRestarting(undefined)
          setActionError(`IBS ${restarting} didn't start, so the launcher went back to ${s.version}. The site's log says why.`)
          reload()
          return
        }
      } catch {
        // restarting
      }
      if (!stopped) timer = window.setTimeout(poll, 1500)
    }
    timer = window.setTimeout(poll, 1500)
    return () => { stopped = true; window.clearTimeout(timer) }
  }, [restarting]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!data) return <ErrorBox error={error} />

  const act = async (f: () => Promise<unknown>, done?: string) => {
    setActionError(undefined)
    setMsg(undefined)
    try {
      await f()
      if (done) setMsg(done)
      reload()
    } catch (err) {
      setActionError((err as Error).message)
    }
  }
  const checkNow = async () => {
    setChecking(true)
    setChecked(undefined)
    try {
      const r = await api<UpdatesInfo>('POST', '/api/updates/check')
      if (!r.github_error) setChecked(r.newer ? `IBS ${r.newer} is available.` : 'No newer version.')
      reload()
    } catch (err) {
      setActionError((err as Error).message)
    } finally {
      setChecking(false)
    }
  }
  const install = async (v: VersionInfo) => {
    const older = !v.newer
    if (!(await ask(`${older ? 'Roll back' : 'Update'} to ${v.version}`, 'IBS restarts with it, and the storage nodes follow one at a time.',
      { ok: older ? 'Roll back' : 'Update' }))) return
    act(async () => {
      startedBefore.current = (await api<State>('GET', '/api/state')).started
      await api('POST', '/api/updates/install', { version: v.version })
      setRestarting(v.version)
    })
  }

  const removeOld = async () => {
    const { remove, keep, fallback } = data.old
    const list = (vs: string[]) => vs.length === 1 ? vs[0] : vs.slice(0, -1).join(', ') + ' and ' + vs[vs.length - 1]
    const kept = [
      keep ? `${keep} is kept, so you can roll back to it.`
        : 'None of them can be rolled back to (a newer version changed the database, or they don\'t work with your other sites), so all of them go.',
      fallback && fallback !== keep && `${fallback} is kept too until ${data.running} has run long enough to count as working: if ${data.running} fails, IBS goes back to it.`,
    ].filter(Boolean).join(' ')
    if (!(await ask('Remove old versions', `Removes ${list(remove)} from this site. ${kept}`, { ok: remove.length === 1 ? `Remove ${remove[0]}` : `Remove ${remove.length} versions`, danger: true }))) return
    act(() => api('POST', '/api/updates/remove-old'), `Removed ${list(remove)}.`)
  }

  return (
    <>
      {data.newer && (
        <div className="notice warn">
          IBS {data.newer} is available. {data.versions.some((v) => v.version === data.newer)
            ? 'It is ready to install below.'
            : 'It runs on other sites; see below how to get it.'}
        </div>
      )}
      <Card title="Versions" actions={
        <div className="row">
          {data.github && (
            <button className="btn small" disabled={checking || !!data.github_retry} title={data.github_retry ? "GitHub's limit is used up" : undefined}
              onClick={checkNow}>{checking ? 'Checking…' : 'Check now'}</button>
          )}
          {data.old.remove.length > 0 && <button className="btn small" onClick={removeOld}>Remove old versions</button>}
          <button className="btn small" onClick={() => setDialog('upload')}>Upload a version</button>
        </div>}>
        <p className="muted small">
          This site runs <b>IBS {data.running}</b>{data.launcher_version ? `, started by IBS launcher ${data.launcher_version}` : ''}.{' '}
          {data.launcher ? 'Install a newer version, or roll back to an older one that still works with this site and the IBS.'
            : 'It was started without the IBS launcher, so it can\'t switch versions here: update its image instead.'}
        </p>
        {data.github && (
          <p className="muted small">
            {checking ? 'Asking GitHub for new versions…'
              : data.github_checked ? `GitHub last checked ${Date.now() / 1000 - data.github_checked < 10 ? 'just now' : ago(data.github_checked)}.${checked ? ' ' + checked : ''}`
              : 'GitHub not checked yet.'}
          </p>
        )}
        {data.github && data.github_retry
            ? <div className="small warn-text">
                Can't check GitHub for new versions until {new Date(data.github_retry * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}: {data.github_error}. Your other sites still pass new versions on.
              </div>
            : data.github && data.github_error && <div className="small warn-text">Last check failed: {data.github_error}</div>}
        <ErrorBox error={actionError} />
        {msg && <div className="success">{msg}</div>}
        {restarting && <div className="notice">Restarting into IBS {restarting}… This page reloads when it's back.</div>}
        {data.versions.length + data.seen.length === 0 ? <Empty>No versions installed yet.</Empty> : (
          <table>
            <thead><tr><th>Version</th><th>Signed by</th><th>Released</th><th>Status</th><th /></tr></thead>
            <tbody>
              {data.versions.map((v) => (
                <tr key={v.version}>
                  <td className="mono">{v.version}{v.sites?.length ? <div className="muted small">Runs on {v.sites.join(', ')}</div> : null}</td>
                  <td>{v.key_name || <span className="muted">unnamed key</span>}<div className="muted mono small">{v.key_id}</div></td>
                  <td>{v.released ? fmtDate(new Date(v.released * 1000).toISOString()) : '—'}</td>
                  <td>
                    {v.current ? <span className="pill ok">Running</span>
                      : v.bad ? <span className="pill warn">Failed to start</span>
                      : v.newer && v.beta && !data.betas ? <span className="muted">Newer beta</span>
                      : v.newer ? <span className="pill warn">Newer</span>
                      : <span className="muted">Older</span>}
                    {v.blocker && <div className="small warn-text">Can't run: {v.blocker}</div>}
                  </td>
                  <td className="right">
                    <div className="row-end">
                      {v.notes && <button className="btn small ghost" onClick={() => setNotes(v)}>Release notes</button>}
                      {!v.current && data.launcher && (
                        <button className={`btn small ${v.newer ? 'primary' : ''}`} disabled={!!v.blocker} title={v.blocker || undefined}
                          onClick={() => install(v)}>{v.newer ? 'Install' : 'Roll back'}</button>
                      )}
                      {!v.current && (
                        <button className="btn small danger" onClick={async () => (await ask(`Remove ${v.version}`, 'It is deleted from this site. You can get it again from GitHub, another site or a file.', { ok: 'Remove', danger: true })) &&
                          act(() => api('DELETE', `/api/updates/versions/${encodeURIComponent(v.version)}`))}>Remove</button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
              {data.seen.map((v) => (
                <tr key={v.version}>
                  <td className="mono">{v.version}<div className="muted small">Runs on {v.sites.join(', ')}</div></td>
                  <td colSpan={3}>
                    {!v.trusted ? <span className="small warn-text">Signed with a key this site doesn't trust (or that site doesn't share it). Add the key below, or get the version from GitHub or as a file.</span>
                      : v.error ? <span className="small warn-text">Fetching it from {v.sites.join(', ')} failed: {v.error}. IBS tries again every minute.</span>
                      : <span className="muted">Fetching it from {v.sites.join(', ')}…</span>}
                  </td>
                  <td />
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <Card title="Where to look for new versions">
        <div className="stack">
          <label className="check">
            <input type="checkbox" checked={data.github} onChange={(e) => act(() => api('PUT', '/api/updates/settings', { github: e.target.checked }))} />
            <span>
              Check GitHub for new IBS releases
              <small className="muted">
                {' '}New versions are downloaded and checked, then installed when you choose (or automatically, if turned on below).
              </small>
            </span>
          </label>
          {data.github && (
            <label className="inline">
              Check
              <select value={data.check_minutes} onChange={(e) => act(() => api('PUT', '/api/updates/settings', { check_minutes: Number(e.target.value) }))}>
                <option value={15}>every 15 minutes</option>
                <option value={360}>every 6 hours</option>
                <option value={1440}>once a day</option>
              </select>
            </label>
          )}
          <label className="check">
            <input type="checkbox" checked={data.betas} onChange={(e) => act(() => api('PUT', '/api/updates/settings', { betas: e.target.checked }))} />
            <span>
              Include betas
              <small className="muted">
                {' '}Betas count as new versions: they are downloaded, from GitHub and other sites, and announced like
                other releases. Off: only stable releases are. Uploaded betas can always be installed.
              </small>
            </span>
          </label>
          <label className="check">
            <input type="checkbox" checked={data.auto_install} onChange={(e) => act(() => api('PUT', '/api/updates/settings', { auto_install: e.target.checked }))} />
            <span>
              Install new versions automatically
              <small className="muted">
                {' '}As soon as one is downloaded, if it works with this site and the IBS. The site restarts with it and its
                storage nodes follow. A version that fails to start is undone, and not installed again.
              </small>
            </span>
          </label>
          <p className="muted small">
            Sites in the IBS always tell each other which versions they run, so this site also learns about new versions
            there. Versions signed with a key this site trusts are fetched from them; nothing is ever installed by another
            site.
          </p>
        </div>
      </Card>

      <KeysCard data={data} onChange={reload} onAdd={() => setDialog('key')} />
      {notes && (
        <Modal title={`What's new in IBS ${notes.version}`} onClose={() => setNotes(undefined)}>
          <ReleaseNotes text={notes.notes} />
        </Modal>
      )}
      {dialog === 'upload' && <UploadDialog onClose={() => setDialog(undefined)} onDone={(v) => { setDialog(undefined); setMsg(`${v} added. It can be installed below.`); reload() }} />}
      {dialog === 'key' && <KeyDialog onClose={() => setDialog(undefined)} onDone={() => { setDialog(undefined); reload() }} />}
    </>
  )
}

function UploadDialog({ onClose, onDone }: { onClose: () => void; onDone: (version: string) => void }) {
  const file = useRef<HTMLInputElement>(null)
  const [chosen, setChosen] = useState(false)
  const { busy, error, submit } = useSubmit(async () => {
    const f = file.current?.files?.[0]
    if (!f) return
    const fd = new FormData()
    fd.append('file', f)
    onDone((await api<{ version: string }>('POST', '/api/updates/upload', fd)).version)
  })
  return (
    <Modal title="Upload a version" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <p className="muted small">
          An <span className="mono">.ibsrelease</span> file, for example from the GitHub release page or built and signed
          yourself (see DEVELOPMENT.md). It must be signed with one of the release keys.
        </p>
        <input ref={file} type="file" accept=".ibsrelease" onChange={(e) => setChosen(!!e.target.files?.length)} />
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!chosen || busy}>{busy ? 'Uploading…' : 'Upload'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function KeyDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [key, setKey] = useState('')
  const [name, setName] = useState('')
  const { busy, error, submit } = useSubmit(async () => { await api('POST', '/api/updates/keys', { key: key.trim(), name }); onDone() })
  return (
    <Modal title="Add a release key" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <label>
          Public key
          <input autoFocus className="mono" value={key} placeholder="ibspub1:…" onChange={(e) => setKey(e.target.value)} />
          <small className="muted">From <span className="mono">openssl pkey -pubout | ibs-release pubkey</span>, or copied from another site's release keys.</small>
        </label>
        <label>
          Name
          <input value={name} placeholder="My builds" onChange={(e) => setName(e.target.value)} />
          <small className="muted">Optional. Shown with the versions signed with it.</small>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!key.trim() || busy}>{busy ? 'Adding…' : 'Add key'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function KeysCard({ data, onChange, onAdd }: { data: UpdatesInfo; onChange: () => void; onAdd: () => void }) {
  const [error, setError] = useState<string>()
  return (
    <Card title="Release keys" actions={<button className="btn small" onClick={onAdd}>Add key</button>}>
      <p className="muted small">
        Versions are only accepted when signed with one of these keys. The built-in key signs the official IBS releases
        and can't be removed. Add your own key to run versions you build and sign yourself; other sites need it too to
        fetch those versions from this one. A key can be removed once no version signed with it is left.
      </p>
      <table>
        <thead><tr><th>Name</th><th>Key</th><th /></tr></thead>
        <tbody>
          {data.keys.map((k) => (
            <tr key={k.id}>
              <td>
                {k.name || <span className="muted">unnamed</span>} {k.default && <span className="tag">built in</span>}
                {k.env && <span className="tag">IBS_TRUSTED_KEYS</span>}
                <div className="muted mono small">{k.id}</div>
                {k.signed.length > 0 && <div className="muted small">Signs {k.signed.join(', ')}</div>}
              </td>
              <td className="mono small key-cell">{k.key}</td>
              <td className="right">
                <div className="row-end">
                  <Copy text={k.key} />
                  {!k.default && (() => {
                    const why = k.env ? 'Set with IBS_TRUSTED_KEYS: remove it there and restart'
                      : k.signed.length ? `Remove ${k.signed.join(', ')} first` : undefined
                    return <button className="btn small danger" disabled={!!why} title={why}
                      onClick={async () => (await ask(`Stop trusting ${k.name || k.id}`, 'Versions signed with it can no longer be added.', { ok: 'Stop trusting', danger: true })) &&
                        api('DELETE', `/api/updates/keys/${k.id}`).then(onChange, (e) => setError(e.message))}>Remove</button>
                  })()}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <ErrorBox error={error} />
    </Card>
  )
}

/** **bold** and `code` in a line of release notes. */
function inline(text: string) {
  return text.split(/(\*\*[^*]+\*\*|`[^`]+`)/).map((part, i) =>
    part.startsWith('**') ? <b key={i}>{part.slice(2, -2)}</b> : part.startsWith('`') ? <code key={i}>{part.slice(1, -1)}</code> : part)
}

/**
 * Release notes from the changelog: "### New" and "### Fixed" sections of
 * bullets; what to update and which versions it works with come last.
 */
function ReleaseNotes({ text }: { text: string }) {
  // Lines wrap in the changelog: join them into headings, bullets and paragraphs.
  const blocks: { kind: 'h' | 'li' | 'p'; text: string }[] = []
  let open = false // the last block continues on the next line
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (!line) { open = false; continue }
    if (raw.startsWith('### ')) { blocks.push({ kind: 'h', text: line.slice(4) }); open = false }
    else if (raw.startsWith('- ')) { blocks.push({ kind: 'li', text: line.slice(2) }); open = true }
    else if (open) blocks[blocks.length - 1].text += ' ' + line
    else { blocks.push({ kind: 'p', text: line }); open = true }
  }
  const compat = (b: { text: string }) => /^\*\*(What to update|Works with)/.test(b.text)
  const out: ReactNode[] = []
  let items: string[] = []
  const flush = () => {
    if (items.length) out.push(<ul key={out.length}>{items.map((t, i) => <li key={i}>{inline(t)}</li>)}</ul>)
    items = []
  }
  for (const b of blocks.filter((b) => !compat(b))) {
    if (b.kind === 'li') { items.push(b.text); continue }
    flush()
    out.push(b.kind === 'h' ? <h3 key={out.length}>{b.text}</h3> : <p key={out.length}>{inline(b.text)}</p>)
  }
  flush()
  return (
    <div className="release-notes">
      {out}
      {blocks.filter(compat).map((b, i) => <p key={'c' + i} className="muted small">{inline(b.text)}</p>)}
    </div>
  )
}
