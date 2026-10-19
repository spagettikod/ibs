import { useState } from 'react'
import { api, CopyCheck, Scrub, ScrubError, ScrubInfo } from '../api'
import { ago, ask, Card, Empty, ErrorBox, fmtBytes, fmtDuration, Meter, useLoader, useTick } from '../ui'

const INTERVALS = [7, 14, 30, 60, 90]

const WHAT: Record<ScrubError['what'], string> = {
  block: 'Block',
  remote: 'Copy on another site',
  held: 'Copy kept for another site',
  database: 'Database',
  copy: 'Copy of the database',
  node: 'Storage node',
}

const PHASE: Record<Scrub['phase'], string> = {
  database: 'Checking the database',
  own: 'Checking this site\'s blocks here and on other sites',
  copies: 'Checking copies on sites that were unreachable',
  held: 'Checking copies kept for other sites that they haven\'t checked lately',
}

/** When a running scrub should be done, from its pace so far; rough, as the
 * pace depends on block sizes and on the other sites. */
export function scrubEta(sc: Scrub): { left: number; at: Date } | undefined {
  const done = sc.checked + sc.copies
  const spent = Date.now() / 1000 - sc.started
  if (sc.state !== 'running' || done < 50 || spent < 30 || done >= sc.total) return undefined
  const left = (spent * (sc.total - done)) / done
  return { left, at: new Date(Date.now() + left * 1000) }
}

/** A date and time, shorter for today. */
export function when(unix: number) {
  const d = new Date(unix * 1000)
  const today = new Date().toDateString() === d.toDateString()
  return today ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

export function scrubPercent(sc: Scrub) {
  return sc.total > 0 ? Math.min(100, Math.floor((100 * (sc.checked + sc.copies)) / sc.total)) : 0
}

/** One line on how a scrub went. */
export function scrubResult(sc: Scrub) {
  if (sc.failed > 0) return `${sc.failed} ${sc.failed === 1 ? 'problem' : 'problems'} couldn't be repaired`
  const fixed = sc.repaired > 0 ? `${sc.repaired} ${sc.repaired === 1 ? 'problem' : 'problems'} found and repaired` : ''
  if (sc.unchecked > 0) {
    return `Not complete: ${sc.unchecked.toLocaleString()} ${sc.unchecked === 1 ? 'copy' : 'copies'} on other sites weren't checked (a site was down)${fixed ? '; ' + fixed : ''}`
  }
  return fixed || 'No problems found'
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
  const eta = cur ? scrubEta(cur) : undefined
  const shown = cur ?? data.last

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Scrub</h1>
          <p className="muted small">
            A scrub reads every block of this site's files and checks it against its checksum, like a ZFS scrub: here, and
            the copies on other sites, which must prove they hold the exact same bytes. Damaged blocks are repaired from
            another copy. Reads are always checked too, scrub or not.
          </p>
        </div>
      </header>
      <ErrorBox error={error} />

      <Card title="Status" actions={
        <div className="row">
          {!cur && <button className="btn small primary" onClick={() => act('start')}>Start scrub</button>}
          {cur?.state === 'running' && <button className="btn small" onClick={() => act('pause')}>Pause</button>}
          {cur?.state === 'paused' && <button className="btn small primary" onClick={() => act('resume')}>Resume</button>}
          {cur && <button className="btn small danger" onClick={async () => (await ask('Stop the scrub', 'The next scrub starts from the beginning.', { ok: 'Stop', danger: true })) && act('stop')}>Stop</button>}
        </div>
      }>
        <ErrorBox error={actionError} />
        {cur ? (
          <div className="stack">
            <div className="scrub-progress">
              <b>{cur.state === 'paused' ? 'Paused' : PHASE[cur.phase]} · {scrubPercent(cur)}%
                {eta && <span className="muted"> · about {fmtDuration(eta.left)} left, done around {when(eta.at.getTime() / 1000)}</span>}</b>
              <Meter used={cur.checked + cur.copies} total={cur.total} />
              <span className="muted small">
                {(cur.checked + cur.copies).toLocaleString()} of {cur.total.toLocaleString()} checked
                ({cur.checked.toLocaleString()} here, {cur.copies.toLocaleString()} copies on other sites), {fmtBytes(cur.bytes)} of data ·
                started {when(cur.started)} {cur.manual ? 'by hand' : 'on schedule'} · {scrubResult(cur)}
              </span>
            </div>
          </div>
        ) : data.last ? (
          <p>
            Last scrub {ago(data.last.finished ?? data.last.started)}{data.last.state === 'stopped' ? ' (stopped)' : ''}:{' '}
            <b className={data.last.failed > 0 ? 'bad-text' : data.last.unchecked > 0 ? 'warn-text' : ''}>{scrubResult(data.last)}</b>.
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
                  <td title={`${new Date(h.started * 1000).toLocaleString()}, ${ago(h.started)}`}>
                    {new Date(h.started * 1000).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' })}
                    <div className="muted small">{h.manual ? 'by hand' : 'on schedule'}</div></td>
                  <td>{h.finished ? fmtDuration(h.finished - h.started) : h.state === 'paused' ? 'paused' : 'running'}</td>
                  <td className="num">{fmtBytes(h.bytes)}<div className="muted small">{h.checked.toLocaleString()} here{h.copies > 0 && `, ${h.copies.toLocaleString()} elsewhere`}</div></td>
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
  const { data, error } = useLoader(() => api<CopyCheck[]>('GET', '/api/scrub/copies'), ['scrub', 'replication'])
  return (
    <Card title="Your copies on other sites">
      <p className="muted small">
        This site's scrub checks its copies on other sites too: they must prove they hold the exact same bytes.
      </p>
      <ErrorBox error={error} />
      {!data ? null : data.length === 0 ? <Empty>No copies on other sites yet.</Empty> : (
        <table>
          <thead><tr><th>Site</th><th className="num">Copies</th><th>Last checked</th><th>Result</th></tr></thead>
          <tbody>
            {data.map((c) => (
              <tr key={c.site_id}>
                <td>{c.name}</td>
                <td className="num">{c.copies.toLocaleString()}</td>
                <td title={c.scrubbed ? ago(c.scrubbed) : undefined}>{c.scrubbed ? when(c.scrubbed) : <span className="muted">not yet</span>}</td>
                <td>{!c.scrubbed ? '—' : c.bad > 0
                  ? <span className="warn-text">{c.bad.toLocaleString()} missing or damaged, copied again from this site</span>
                  : `${c.checked.toLocaleString()} checked, all identical`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  )
}
