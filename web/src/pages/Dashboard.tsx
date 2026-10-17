import { api, Dashboard, ScrubInfo, TLSStatus, UpdatesInfo } from '../api'
import { NetworkView, useNetworkView, ViewSwitch } from '../GeoMap'
import { ratesOf } from '../NetworkGraph'
import { ago, Card, ErrorBox, fmtBytes, fmtDuration, fmtSpeed, Meter, Stat, uptime, useLoader, useTick } from '../ui'
import { scrubPercent, scrubResult } from './Scrub'

type Item = { level: 'bad' | 'warn' | 'info'; text: string; href: string; link: string }

/** What needs attention, most serious first. */
function attention(d: Dashboard, tls?: TLSStatus, updates?: UpdatesInfo, scrub?: ScrubInfo): Item[] {
  const out: Item[] = []
  if (scrub && scrub.unresolved > 0) {
    out.push({ level: 'bad', text: `The scrub found ${scrub.unresolved} ${scrub.unresolved === 1 ? 'problem' : 'problems'} it couldn't repair`, href: '#/scrub', link: 'Scrub' })
  }
  if (updates?.newer) {
    const ready = updates.versions.some((v) => v.version === updates.newer)
    out.push({ level: 'warn', text: `IBS ${updates.newer} is available${ready ? '' : ' (it runs on other sites)'}; this site runs ${updates.running}`, href: '#/settings/updates', link: 'Updates' })
  }
  if (tls && (tls.domains?.length ?? 0) > 0) {
    const days = tls.not_after ? (new Date(tls.not_after).getTime() - Date.now()) / 86400000 : undefined
    if (tls.error && (days === undefined || days < 14)) {
      out.push({ level: days === undefined || days < 3 ? 'bad' : 'warn', text: days === undefined
        ? 'No HTTPS certificate yet: the last request failed'
        : `The HTTPS certificate expires in ${Math.floor(days)} days and renewing it failed`, href: '#/settings', link: 'Settings' })
    }
  }
  const r = d.replication
  const c = d.ibs_capacity
  for (const n of d.nodes ?? []) {
    if (n.lost) out.push({ level: 'bad', text: `Storage node ${n.name} is lost; its blocks are being restored on other nodes`, href: '#/nodes', link: 'Nodes' })
    else if (!n.online) out.push({ level: 'warn', text: `Storage node ${n.name} is offline (seen ${ago(n.last_seen)})`, href: '#/nodes', link: 'Nodes' })
  }
  if (r.missing_locally > 0) {
    out.push({ level: 'warn', text: `${r.missing_locally} blocks are being restored to this site from other sites`, href: '#/nodes', link: 'Nodes' })
  }
  // Other sites' copies make way for this site's own files, so they count as room.
  const room = d.site.free + d.site.held_bytes
  if (d.site.capacity > 0 && room < d.site.capacity * 0.05) {
    out.push({ level: 'bad', text: `Storage is almost full: ${fmtBytes(room)} left for this site's files`, href: '#/nodes', link: 'Nodes' })
  }
  for (const rl of d.relays ?? []) {
    if (!rl.connected) out.push({ level: 'warn', text: `Relay ${rl.name} is not connected`, href: '#/network', link: 'The Known Universe' })
  }
  for (const p of d.peers ?? []) {
    if (!p.online) out.push({ level: 'warn', text: `Site ${p.name} is offline (seen ${ago(p.last_seen)})`, href: '#/network', link: 'The Known Universe' })
  }
  if (c.sites > 0 && c.sites < c.copies) {
    out.push({ level: 'warn', text: `Files have ${c.sites} of ${c.copies} copies on other sites: only ${c.sites} other ${c.sites === 1 ? 'site is' : 'sites are'} online`, href: '#/network', link: 'The Known Universe' })
  } else if (r.no_room > 0 || (!c.fits && r.under_replicated > 0)) {
    out.push({ level: 'warn', text: `The IBS is running out of space: ${r.under_replicated} blocks have fewer copies on other sites than they should. They get them back when there is room.`, href: '#/network', link: 'The Known Universe' })
  } else if (r.under_replicated > 0) {
    out.push({ level: 'info', text: `${r.under_replicated} blocks are being copied to other sites`, href: '#/buckets', link: 'Buckets' })
  }
  if (!d.site.peer_advertise && (d.relays ?? []).length === 0) {
    out.push({ level: 'warn', text: 'Other sites cannot reach this site: add a relay', href: '#/network', link: 'The Known Universe' })
  } else if ((d.peers ?? []).length === 0) {
    out.push({ level: 'warn', text: 'No backups yet: this site isn\'t connected to another site, so no copies are made', href: '#/network', link: 'The Known Universe' })
  }
  const rank = { bad: 0, warn: 1, info: 2 }
  return out.sort((a, b) => rank[a.level] - rank[b.level])
}

/**
 * Shown while copies are being made or received: where they go, how fast,
 * what's left and about how long it takes, or why copying waits.
 */
function Copying({ d }: { d: Dashboard }) {
  const r = d.replication
  const peers = d.peers ?? []
  const sending = peers.filter((p) => p.out_bps > 0)
  const receiving = peers.filter((p) => p.in_bps > 0)
  const out = sending.reduce((n, p) => n + p.out_bps, 0)
  if (r.copying === 0 && sending.length === 0 && receiving.length === 0) return null
  const waiting = r.copying > 0 && out === 0
    ? !peers.some((p) => p.online) ? 'Waiting: no other site is online'
      : r.no_room > 0 ? 'Waiting: the other sites are full'
      : 'Starting…'
    : undefined
  return (
    <Card title="Copying">
      <div className="copying">
        {r.copying > 0 && (
          <>
            <div className="copying-head">
              <b>{r.copying.toLocaleString()} blocks</b>
              <span className="muted">{fmtBytes(r.copying_bytes)} left to copy to other sites</span>
              {out > 0 && <span>{fmtSpeed(out)} · about {fmtDuration(r.copying_bytes / out)} left</span>}
              {waiting && <span className="warn-text">{waiting}</span>}
            </div>
            <Meter used={r.copies_made} total={r.copies_possible} />
          </>
        )}
        {(sending.length > 0 || receiving.length > 0) && (
          <ul>
            {sending.map((p) => <li key={'o' + p.site_id}><span className="pulse" />To {p.name}<span className="muted">{fmtSpeed(p.out_bps)}</span></li>)}
            {receiving.map((p) => <li key={'i' + p.site_id}><span className="pulse" />From {p.name}, its copies<span className="muted">{fmtSpeed(p.in_bps)}</span></li>)}
          </ul>
        )}
      </div>
    </Card>
  )
}

function Health({ items }: { items: Item[] }) {
  const problems = items.filter((i) => i.level !== 'info')
  const tone = problems.some((i) => i.level === 'bad') ? 'bad' : problems.length > 0 ? 'warn' : 'ok'
  const title = problems.length === 0
    ? 'Everything is fine'
    : `${problems.length} ${problems.length === 1 ? 'thing needs' : 'things need'} attention`
  return (
    <Card>
      <div className="health">
        <div className="health-title">
          <span className={`health-icon ${tone}`}>{tone === 'ok' ? '✓' : '!'}</span>
          {title}
        </div>
        {items.length > 0 && (
          <ul>
            {items.map((i, k) => (
              <li key={k}>
                <span className={`dot ${i.level}`} />
                <span>{i.text}</span>
                <a href={i.href}>{i.link} →</a>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Card>
  )
}

export default function DashboardPage() {
  const { data, error } = useLoader(() => api<Dashboard>('GET', '/api/dashboard'), ['data', 'peers', 'nodes', 'replication'])
  const scrub = useLoader(() => api<ScrubInfo>('GET', '/api/scrub'), ['scrub'], 60000)
  const [view, setView] = useNetworkView()
  const updates = useLoader(() => api<UpdatesInfo>('GET', '/api/updates'), ['peers'], 60000)
  const tls = useLoader(() => api<TLSStatus>('GET', '/api/tls'), [], 60000)
  useTick()
  if (!data) return <ErrorBox error={error} />
  const { site, replication: r } = data
  const nodes = data.nodes ?? []
  const network = data.network ?? []
  const used = site.capacity - site.free
  const online = network.filter((n) => n.online).length + 1
  // Of the copies the other sites can take now: with fewer sites than copies,
  // the rest wait for another site (the health list says so).
  const copied = r.copies_possible === 0 ? 100 : Math.floor((100 * r.copies_made) / r.copies_possible)
  // Copies per file on other sites: wanted, and what the online sites allow.
  const want = data.ibs_capacity.copies
  const have = Math.min(data.ibs_capacity.sites, want)

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>{site.name}</h1>
          <p className="muted small">Up {uptime(site.up_since)} · site <span className="mono">{site.site_id.slice(0, 8)}</span></p>
        </div>
      </header>
      <ErrorBox error={error} />

      <Health items={attention(data, tls.data, updates.data, scrub.data)} />
      <Copying d={data} />

      <div className="tiles">
        <Stat label="Your files" value={fmtBytes(site.object_bytes)}
          sub={`${site.objects.toLocaleString()} ${site.objects === 1 ? 'object' : 'objects'} in ${site.buckets} ${site.buckets === 1 ? 'bucket' : 'buckets'}, ${r.blocks.toLocaleString()} ${r.blocks === 1 ? 'block' : 'blocks'}`} />
        <Stat label="Copies on other sites" value={r.copying > 0 ? `${copied}%` : `${have} of ${want}`}
          tone={r.under_replicated === 0 ? 'ok' : 'warn'}
          sub={r.copying > 0 ? `copying, ${r.copying.toLocaleString()} blocks left`
            : r.under_replicated > 0 && have < want ? `per file; the next needs ${want - have === 1 ? 'another site' : 'more sites'}`
            : r.under_replicated > 0 ? `per file; ${r.under_replicated.toLocaleString()} blocks wait for room`
            : 'per file, all in place'} />
        <Stat label="Storage" value={`${fmtBytes(site.free)} free`}
          sub={<><Meter used={used} total={site.capacity} />{fmtBytes(used)} of {fmtBytes(site.capacity)} used
            {site.held_bytes > 0 ? `, ${fmtBytes(site.held_bytes)} of it other sites' copies` : ''}</>} />
        <Stat label="The IBS" value={`${online} of ${network.length + 1}`} sub="sites online" />
        <ScrubTile info={scrub.data} />
      </div>

      <div className="overview">
        <Card title="Nodes" actions={<a className="btn small" href="#/nodes">Manage</a>}>
          <ul className="node-list">
            {nodes.map((n) => (
              <li key={n.id}>
                <div className="row1">
                  <span className={`status-dot ${n.online ? '' : 'off'}`} />
                  <span>{n.name}</span>
                  <span className="muted">{n.quota < 0 ? 'shares nothing' : `${fmtBytes(n.free)} free`}</span>
                </div>
                <Meter used={n.used} total={n.used + n.free} />
              </li>
            ))}
          </ul>
        </Card>
        <Card title="The Known Universe" actions={<div className="row"><ViewSwitch view={view} onChange={setView} /><a className="btn small" href="#/network">Details</a></div>}>
          {network.length === 0 ? (
            <p className="muted small">Not connected to other sites yet.</p>
          ) : (
            <NetworkView compact view={view} self={{ id: site.site_id, name: site.name, location: site.location }}
              upSince={site.up_since} sites={network} links={data.links ?? []} relays={data.relay_nodes ?? []} rates={ratesOf(data.peers ?? [])} />
          )}
        </Card>
      </div>
    </div>
  )
}

function ScrubTile({ info }: { info?: ScrubInfo }) {
  if (!info) return <Stat label="Scrub" value="…" />
  const cur = info.current
  if (cur) {
    return (
      <a className="tile-link" href="#/scrub">
        <Stat label="Scrub" value={cur.state === 'paused' ? `Paused at ${scrubPercent(cur)}%` : `${scrubPercent(cur)}%`}
          tone={cur.failed > 0 ? 'bad' : undefined}
          sub={<><Meter used={cur.checked} total={cur.total} />{cur.checked.toLocaleString()} of {cur.total.toLocaleString()} blocks checked</>} />
      </a>
    )
  }
  const last = info.last
  return (
    <a className="tile-link" href="#/scrub">
      <Stat label="Scrub" value={!last ? 'Not yet' : info.unresolved > 0 ? 'Problems' : last.repaired > 0 ? 'Repaired' : 'Clean'}
        tone={!last ? undefined : info.unresolved > 0 ? 'bad' : 'ok'}
        sub={last ? `${scrubResult(last)}, ${ago(last.finished ?? last.started)}` : 'No scrub has run yet'} />
    </a>
  )
}
