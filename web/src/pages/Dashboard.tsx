import { api, Dashboard, ScrubInfo, TLSStatus, UpdatesInfo } from '../api'
import { NetworkView, useNetworkView, ViewSwitch } from '../GeoMap'
import { ratesOf } from '../NetworkGraph'
import { ago, Card, ErrorBox, fmtBytes, fmtDuration, fmtSpeed, Meter, Spinner, Stat, uptime, useLoader, useTick } from '../ui'
import { scrubEta, scrubPercent, scrubResult } from './Scrub'

type Item = { level: 'bad' | 'warn' | 'info'; text: string; href: string; link: string }

/** What needs attention, most serious first. */
function attention(d: Dashboard, tls?: TLSStatus, updates?: UpdatesInfo, scrub?: ScrubInfo): Item[] {
  const out: Item[] = []
  // Until the figures are counted (right after a start), leave out what
  // depends on them rather than warn about zeros.
  const counted = d.counted
  if (scrub && scrub.unresolved > 0) {
    out.push({ level: 'bad', text: `Some files can't be read: the scrub found ${scrub.unresolved} damaged ${scrub.unresolved === 1 ? 'block' : 'blocks'} with no intact copy left`, href: '#/scrub', link: 'Scrub' })
  }
  if (scrub?.last && scrub.last.unchecked > 0 && !scrub.current) {
    out.push({ level: 'warn', text: `Not all copies were checked: ${scrub.last.unchecked.toLocaleString()} copies on other sites were skipped in the last scrub because their site was down`, href: '#/scrub', link: 'Scrub' })
  }
  if (updates?.newer) {
    const ready = updates.versions.some((v) => v.version === updates.newer)
    out.push({ level: 'warn', text: `IBS ${updates.newer} is available${ready ? '' : ' (it runs on other sites)'}; this site runs ${updates.running}`, href: '#/settings/updates', link: 'Updates' })
  }
  if (tls && (tls.domains?.length ?? 0) > 0) {
    const days = tls.not_after ? (new Date(tls.not_after).getTime() - Date.now()) / 86400000 : undefined
    if (tls.error && (days === undefined || days < 14)) {
      out.push({ level: days === undefined || days < 3 ? 'bad' : 'warn', text: days === undefined
        ? 'HTTPS doesn\'t work yet: getting a certificate failed'
        : `HTTPS stops working in ${Math.floor(days)} days: renewing the certificate failed`, href: '#/settings', link: 'Settings' })
    }
  }
  const r = d.replication
  const c = d.ibs_capacity
  for (const n of d.nodes ?? []) {
    if (n.lost) out.push({ level: 'bad', text: `Files on storage node ${n.name} are being restored on your other nodes: the node has been gone too long`, href: '#/nodes', link: 'Nodes' })
    else if (!n.online) out.push({ level: 'warn', text: `Files on storage node ${n.name} are read from other sites for now: the node is offline (seen ${ago(n.last_seen)})`, href: '#/nodes', link: 'Nodes' })
  }
  if (counted && r.missing_locally > 0) {
    out.push({ level: 'warn', text: `Some of your files are read from other sites until they're back here: ${fmtBytes(r.missing_bytes)} ` +
      `(${r.missing_locally.toLocaleString()} blocks) is being restored from the copies there`, href: '#/nodes', link: 'Nodes' })
  }
  // Other sites' copies make way for this site's own files, so they count as
  // room; those of offline sites don't, they can't be pushed away.
  const room = d.site.free + d.site.held_bytes - d.site.held_offline
  if (counted && d.site.capacity > 0 && room < d.site.capacity * 0.05) {
    out.push({ level: 'bad', text: `Uploads will soon fail: only ${fmtBytes(room)} is left for this site's files`, href: '#/nodes', link: 'Nodes' })
  }
  for (const rl of d.relays ?? []) {
    if (!rl.connected) out.push({ level: 'warn', text: `Sites that reach this site only through relay ${rl.name} can't connect: the relay isn't connected`, href: '#/network', link: 'The Known Universe' })
  }
  for (const p of d.peers ?? []) {
    if (!p.online) out.push({ level: 'warn', text: `Your copies on ${p.name} can't be reached, and new copies wait for it: the site is offline (seen ${ago(p.last_seen)})`, href: '#/network', link: 'The Known Universe' })
  }
  if (!counted) {
    // nothing yet
  } else if (c.sites > 0 && c.sites < c.copies) {
    out.push({ level: 'warn', text: `Your files aren't fully protected: they have ${c.sites} of ${c.copies} copies on other sites, because only ${c.sites} other ${c.sites === 1 ? 'site is' : 'sites are'} online`, href: '#/network', link: 'The Known Universe' })
  } else if (r.no_room > 0 || (!c.fits && r.under_replicated > 0)) {
    out.push({ level: 'warn', text: `Your files aren't fully protected: ${r.under_replicated.toLocaleString()} blocks lack copies because the IBS is running out of space. They get them when there's room.`, href: '#/network', link: 'The Known Universe' })
  } else if (r.under_replicated > 0) {
    out.push({ level: 'info', text: `${r.under_replicated.toLocaleString()} blocks aren't protected yet: they're being copied to other sites`, href: '#/buckets', link: 'Buckets' })
  }
  if (!d.site.peer_advertise && (d.relays ?? []).length === 0) {
    out.push({ level: 'warn', text: 'Your files get no copies: other sites can\'t reach this site. Add a relay.', href: '#/network', link: 'The Known Universe' })
  } else if ((d.peers ?? []).length === 0) {
    out.push({ level: 'warn', text: 'Your files get no copies: this site isn\'t connected to another site yet', href: '#/network', link: 'The Known Universe' })
  }
  const rank = { bad: 0, warn: 1, info: 2 }
  return out.sort((a, b) => rank[a.level] - rank[b.level])
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
  // Room for this site's files: free space, and other sites' copies that make
  // way for them (not those of offline sites, which can't be pushed away).
  // Blocks still being fetched back from other sites (after a restore) count
  // as used: the room shown is what's left once they are all here.
  const giving = site.held_bytes - site.held_offline
  const restoring = r.missing_bytes ?? 0
  const roomForMine = Math.max(site.free + giving - restoring, 0)
  const mine = Math.min(Math.max(site.capacity - site.free - site.held_bytes, 0) + restoring, site.capacity)
  const online = network.filter((n) => n.online).length + 1
  // Of the copies the other sites can take now: with fewer sites than copies,
  // the rest wait for another site (the health list says so).
  const copied = r.copies_possible === 0 ? 100 : Math.floor((100 * r.copies_made) / r.copies_possible)
  // Copies moving now: ours to other sites, theirs to this one.
  const sendingBps = (data.peers ?? []).reduce((n, p) => n + p.out_bps, 0)
  const receivingBps = (data.peers ?? []).reduce((n, p) => n + p.in_bps, 0)
  const copyWait = !(data.peers ?? []).some((p) => p.online) ? 'waiting: no other site is online'
    : r.no_room > 0 ? 'waiting: the other sites are full' : 'starting…'
  // Files arriving or being deleted right now; the figures catch up within seconds.
  const act = data.activity
  const busy = act?.writing && act?.deleting ? 'Files arriving and being deleted…'
    : act?.writing ? 'Files arriving…' : act?.deleting ? 'Files being deleted…' : ''
  // After a restore: your files' blocks coming back from the copies on other sites.
  const restoreNote = restoring > 0 && (data.restore_bps > 0
    ? <>Restoring from other sites: {fmtBytes(restoring)} left · {fmtSpeed(data.restore_bps)} · about {fmtDuration(restoring / data.restore_bps)}</>
    : <>Restoring from other sites: {fmtBytes(restoring)} left{(data.peers ?? []).some((p) => p.online) ? '' : ', waiting for another site to come online'}</>)
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

      <div className="tiles">
        {!data.counted ? (
          <>
            <Stat label="Your files" value={<Spinner />} sub="counting…" />
            <Stat label="Copies on other sites"
              value={<>{r.copying > 0 ? `${copied}%` : `${have} of ${want}`}{sendingBps > 0 && <> <Spinner title="Copying to other sites" /></>}</>}
              tone={r.under_replicated === 0 ? 'ok' : 'warn'}
              sub={r.copying > 0 ? <>
                <Meter used={r.copies_made} total={r.copies_possible} />
                {sendingBps > 0
                  ? <>{fmtBytes(r.copying_bytes)} left · {fmtSpeed(sendingBps)} · about {fmtDuration(r.copying_bytes / sendingBps)}</>
                  : <span className="warn-text">{copyWait}</span>}</>
                : r.under_replicated > 0 && have < want ? `per file; the next needs ${want - have === 1 ? 'another site' : 'more sites'}`
                : r.under_replicated > 0 ? `per file; ${r.under_replicated.toLocaleString()} blocks wait for room`
                : 'per file, all in place'} />
            <Stat label="Room for your files" value={<Spinner />} sub="counting…" />
          </>
        ) : (
          <>
            <Stat label="Your files" value={<>{fmtBytes(site.object_bytes)}{(busy || restoreNote) && <> <Spinner title={busy || 'Restoring from other sites'} /></>}</>}
              sub={<>{busy && <div className="accent-text">{busy}</div>}
                {restoreNote && <div className="accent-text">{restoreNote}</div>}
                {`${site.objects.toLocaleString()} ${site.objects === 1 ? 'object' : 'objects'} in ${site.buckets} ${site.buckets === 1 ? 'bucket' : 'buckets'}, ${r.blocks.toLocaleString()} ${r.blocks === 1 ? 'block' : 'blocks'}`}</>} />
            <Stat label="Copies on other sites" value={r.copying > 0 ? `${copied}%` : `${have} of ${want}`}
              tone={r.under_replicated === 0 ? 'ok' : 'warn'}
              sub={r.copying > 0 ? `copying, ${r.copying.toLocaleString()} blocks left`
                : r.under_replicated > 0 && have < want ? `per file; the next needs ${want - have === 1 ? 'another site' : 'more sites'}`
                : r.under_replicated > 0 ? `per file; ${r.under_replicated.toLocaleString()} blocks wait for room`
                : 'per file, all in place'} />
            <Stat label="Room for your files" value={<>{fmtBytes(roomForMine)}{(busy || restoreNote) && <> <Spinner title={busy || 'Restoring from other sites'} /></>}</>}
              sub={<><Meter used={mine} total={site.capacity} />
                {restoring > 0 && <div>once the {fmtBytes(restoring)} being restored is back</div>}
                {site.held_bytes > 0
                  ? <>{fmtBytes(site.free)} empty, {fmtBytes(giving)} other sites' copies that make way for yours
                    {site.held_offline > 0 && `; ${fmtBytes(site.held_offline)} held for offline sites`}</>
                  : <>{fmtBytes(mine)} of {fmtBytes(site.capacity)} used by your files</>}</>} />
          </>
        )}
        <a className="tile-link" href="#/network"><Stat label="The IBS"
          value={<>{`${online} of ${network.length + 1}`}{receivingBps > 0 && <> <Spinner title="Other sites' copies arriving" /></>}</>}
          sub={receivingBps > 0 ? <>sites online<div className="accent-text">their copies arriving, {fmtSpeed(receivingBps)}</div></> : 'sites online'} /></a>
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
          sub={<><Meter used={cur.checked + cur.copies} total={cur.total} />{(cur.checked + cur.copies).toLocaleString()} of {cur.total.toLocaleString()} checked
            {scrubEta(cur) && <div>about {fmtDuration(scrubEta(cur)!.left)} left</div>}</>} />
      </a>
    )
  }
  const last = info.last
  return (
    <a className="tile-link" href="#/scrub">
      <Stat label="Scrub" value={!last ? 'Not yet' : info.unresolved > 0 ? 'Problems' : last.state === 'stopped' ? 'Stopped'
        : last.unchecked > 0 ? 'Not complete' : last.repaired > 0 ? 'Repaired' : 'Clean'}
        tone={!last ? undefined : info.unresolved > 0 ? 'bad' : last.state === 'stopped' || last.unchecked > 0 ? 'warn' : 'ok'}
        sub={!last ? 'No scrub has run yet' : last.state === 'stopped'
          ? `Stopped ${ago(last.finished ?? last.started)} at ${scrubPercent(last)}%; start a new one on the Scrub page`
          : `${scrubResult(last)}, ${ago(last.finished ?? last.started)}`} />
    </a>
  )
}
