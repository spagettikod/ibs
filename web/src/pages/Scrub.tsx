import { useState } from 'react'
import { api, CopyCheck, Scrub, ScrubError, ScrubInfo } from '../api'
import { ago, Card, Empty, ErrorBox, fmtBytes, fmtDuration, Meter, useLoader, useTick } from '../ui'

const INTERVALS = [7, 14, 30, 60, 90]

const WHAT: Record<ScrubError['what'], string> = {
  block: 'Block',
  held: 'Copy kept for another site',
  database: 'Database',
  copy: 'Copy of the database',
  node: 'Storage node',
}

const PHASE: Record<Scrub['phase'], string> = {
  database: 'Checking the database',
  own: 'Checking this site\'s blocks',
  held: 'Checking copies kept for other sites',
}

export function scrubPercent(sc: Scrub) {
  return sc.total > 0 ? Math.min(100, Math.floor((100 * sc.checked) / sc.total)) : 0
}

/** One line on how a scrub went. */
export function scrubResult(sc: Scrub) {
  if (sc.failed > 0) return `${sc.failed} ${sc.failed === 1 ? 'problem' : 'problems'} couldn't be repaired`
  if (sc.repaired > 0) return `${sc.repaired} ${sc.repaired === 1 ? 'problem' : 'problems'} found and repaired`
  return 'No problems found'
}

function inDays(unix: number) {
  const s = unix - Date.now() / 1000
  return s <= 0 ? 'soon' : `in ${fmtDuration(s)}`
}

export default function ScrubPage() {
  const { data, error, reload } = useLoader(() => api<ScrubInfo>('GET', '/api/scrub'), ['scrub'], 30000)
  const [actionError, setActionError] = useState<string>()
  useTick()
  if (!data) return <ErrorBox error={error} />
  const act = (path: string, body?: unknown) => {
    setActionError(undefined)
    api(path === 'settings' ? 'PUT' : 'POST', `/api/scrub/${path}`, body).then(reload, (e) => setActionError(e.message))
  }
  const cur = data.current
  const shown = cur ?? data.last

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Scrub</h1>
          <p className="muted small">
            A scrub reads every block this site stores and checks it against its checksum, like a ZFS scrub. Damaged
            blocks are repaired from copies on other sites. Reads are always checked too, scrub or not.
          </p>
        </div>
      </header>
      <ErrorBox error={error} />

      <Card title="Status" actions={
        <div className="row">
          {!cur && <button className="btn small primary" onClick={() => act('start')}>Start scrub</button>}
          {cur?.state === 'running' && <button className="btn small" onClick={() => act('pause')}>Pause</button>}
          {cur?.state === 'paused' && <button className="btn small primary" onClick={() => act('resume')}>Resume</button>}
          {cur && <button className="btn small danger" onClick={() => confirm('Stop the scrub? It starts from the beginning next time.') && act('stop')}>Stop</button>}
        </div>
      }>
        <ErrorBox error={actionError} />
        {cur ? (
          <div className="stack">
            <div className="scrub-progress">
              <b>{cur.state === 'paused' ? 'Paused' : PHASE[cur.phase]} · {scrubPercent(cur)}%</b>
              <Meter used={cur.checked} total={cur.total} />
              <span className="muted small">
                {cur.checked.toLocaleString()} of {cur.total.toLocaleString()} blocks checked, {fmtBytes(cur.bytes)} read ·
                started {ago(cur.started)} {cur.manual ? 'by hand' : 'on schedule'} · {scrubResult(cur)}
              </span>
            </div>
          </div>
        ) : data.last ? (
          <p>
            Last scrub {ago(data.last.finished ?? data.last.started)}{data.last.state === 'stopped' ? ' (stopped)' : ''}:{' '}
            <b className={data.last.failed > 0 ? 'bad-text' : ''}>{scrubResult(data.last)}</b>.
          </p>
        ) : (
          <p className="muted">No scrub has run yet.</p>
        )}
        <div className="row scrub-schedule">
          <label className="inline">
            Scrub
            <select value={data.interval_days} onChange={(e) => act('settings', { interval_days: Number(e.target.value) })}>
              {INTERVALS.map((d) => <option key={d} value={d}>every {d} days</option>)}
              {!INTERVALS.includes(data.interval_days) && data.interval_days > 0 && <option value={data.interval_days}>every {data.interval_days} days</option>}
              <option value={0}>only by hand</option>
            </select>
          </label>
          {data.next_due && !cur ? <span className="muted small">Next scrub {inDays(data.next_due)}.</span> : null}
        </div>
      </Card>

      <Card title={cur ? 'Problems found so far' : 'Problems found in the last scrub'}>
        {!shown ? <Empty>No scrub has run yet.</Empty> : data.errors.length === 0 ? <Empty>None{cur ? ' so far' : ''}.</Empty> : (
          <table>
            <thead><tr><th>Found</th><th>What</th><th>Where</th><th>Problem</th><th>Result</th></tr></thead>
            <tbody>
              {data.errors.map((e, i) => (
                <tr key={i}>
                  <td className="nowrap">{ago(e.at)}</td>
                  <td>{WHAT[e.what]}{e.block_id && <div className="muted mono small" title={e.block_id}>{e.block_id.slice(0, 16)}…</div>}</td>
                  <td>{e.place || '—'}</td>
                  <td>{e.problem}</td>
                  <td>
                    <Outcome e={e} />
                    {e.detail && <div className="muted small">{e.detail}</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <CopiesCard />

      <Card title="History">
        {data.history.length === 0 ? <Empty>No scrubs yet.</Empty> : (
          <table>
            <thead><tr><th>Started</th><th>Took</th><th className="num">Checked</th><th>Result</th></tr></thead>
            <tbody>
              {data.history.map((h) => (
                <tr key={h.id}>
                  <td>{ago(h.started)}<div className="muted small">{h.manual ? 'by hand' : 'on schedule'}</div></td>
                  <td>{h.finished ? fmtDuration(h.finished - h.started) : h.state === 'paused' ? 'paused' : 'running'}</td>
                  <td className="num">{fmtBytes(h.bytes)}<div className="muted small">{h.checked.toLocaleString()} blocks</div></td>
                  <td>{h.state === 'stopped' ? <span className="muted">Stopped · </span> : null}{scrubResult(h)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  )
}

function Outcome({ e }: { e: ScrubError }) {
  switch (e.outcome) {
    case 'repaired': return <span className="pill ok">repaired</span>
    case 'replaced': return <span className="pill ok">new copy coming</span>
    case 'skipped': return <span className="pill warn">not checked</span>
    default: return e.resolved ? <span className="pill ok">repaired later</span> : <span className="pill bad">not repaired</span>
  }
}

function CopiesCard() {
  const { data, error, reload } = useLoader(() => api<CopyCheck[]>('GET', '/api/scrub/copies'), [], 300000)
  return (
    <Card title="Your copies on other sites" actions={<button className="btn small" onClick={reload}>Refresh</button>}>
      <p className="muted small">
        Each site scrubs what it stores, including the copies it keeps for this site, and replaces damaged ones from
        this site.
      </p>
      <ErrorBox error={error} />
      {!data ? <p className="muted">Asking the other sites…</p> : data.length === 0 ? <Empty>No copies on other sites yet.</Empty> : (
        <table>
          <thead><tr><th>Site</th><th className="num">Copies</th><th>Last scrub there</th><th>Result</th></tr></thead>
          <tbody>
            {data.map((c) => (
              <tr key={c.site_id}>
                <td>{c.name}</td>
                <td className="num">{c.copies.toLocaleString()}</td>
                <td>{c.error ? <span className="muted">{c.error}</span> : c.scrubbed ? ago(c.scrubbed) : 'not yet'}{c.running && <div className="muted small">a scrub is running</div>}</td>
                <td>{c.error || !c.scrubbed ? '—' : c.damaged > 0
                  ? <span className="warn-text">{c.damaged} damaged, replaced from this site</span>
                  : `${c.checked.toLocaleString()} checked, all intact`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  )
}
