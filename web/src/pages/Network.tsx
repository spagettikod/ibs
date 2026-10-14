import { useEffect, useState } from 'react'
import { api, Dashboard, PathInfo, RelayInfo, Settings } from '../api'
import { NetworkView, useNetworkView, ViewSwitch } from '../GeoMap'
import { ago, Card, Copy, Empty, ErrorBox, fmtBytes, fmtMs, fmtSpeed, Modal, shortId, Status, uptime, useLoader, useSubmit, useTick, Version } from '../ui'

export function PathBadge({ path, online }: { path: PathInfo; online: boolean }) {
  if (!online || !path.kind) return <span className="path none">Not connected</span>
  if (path.kind === 'relay') {
    return (
      <span className="path-wrap">
        <span className="path relay">Via relay</span>
        {path.relay_name && <span className="muted small mono">{path.relay_name}</span>}
      </span>
    )
  }
  return <span className="path">Direct{path.kind === 'quic' ? '' : ' (TCP)'}</span>
}

type Dialog = { kind: 'address' } | { kind: 'leave' } | { kind: 'invite' } | { kind: 'join' } | { kind: 'relay' } | { kind: 'move'; relay: RelayInfo }

export default function NetworkPage() {
  const { data, error, reload } = useLoader(() => api<Dashboard>('GET', '/api/dashboard'), ['peers', 'replication', 'data'], 5000)
  useTick()
  const [dialog, setDialog] = useState<Dialog>()
  const [view, setView] = useNetworkView()
  const close = () => setDialog(undefined)
  const done = () => { setDialog(undefined); reload() }

  const removeRelay = async (r: RelayInfo) => {
    if (!confirm(`Stop using relay ${r.name}? Sites that can only reach this site through it will lose the connection.`)) return
    await api('DELETE', `/api/relays/${r.id}`).catch((e) => alert(e.message))
    reload()
  }

  const peers = data?.peers ?? []
  const relays = data?.relays ?? []
  const peerIds = new Set(peers.map((p) => p.site_id))
  const indirect = (data?.network ?? []).filter((n) => !peerIds.has(n.site_id))
  const reachable = !!data && (!!data.site.peer_advertise || relays.length > 0)

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>The Known Universe</h1>
          <p className="muted small">Sites connect directly when they can, otherwise through a relay.</p>
        </div>
        <div className="page-actions">
          <button className="btn primary" onClick={() => setDialog({ kind: 'invite' })}>Invite a site</button>
          <button className="btn" onClick={() => setDialog({ kind: 'join' })}>Join a site</button>
          <button className="btn" onClick={() => setDialog({ kind: 'relay' })}>Add relay</button>
        </div>
      </header>
      <ErrorBox error={error} />

      {data && data.network.length > 0 && (
        <Card actions={<ViewSwitch view={view} onChange={setView} />}>
          <NetworkView view={view} self={{ id: data.site.site_id, name: data.site.name, location: data.site.location }}
            upSince={data.site.up_since} sites={data.network} links={data.links ?? []} relays={data.relay_nodes ?? []} />
        </Card>
      )}

      <Card title="Connected sites">
        {peers.length === 0 ? (
          <Empty>
            No connected sites yet. Use <b>Invite a site</b> and give the invite to the other site, or <b>Join a site</b> with
            an invite you received.
          </Empty>
        ) : (
          <table>
            <thead>
              <tr><th>Site</th><th>Version</th><th>Connection</th><th>Ping</th><th>Speed</th><th>Our data there</th><th>Their data here</th></tr>
            </thead>
            <tbody>
              {peers.map((p) => (
                <tr key={p.site_id}>
                  <td>
                    {p.name}
                    <div className="muted small"><Status online={p.online} /> {p.online ? `up ${uptime(p.up_since)}` : `seen ${ago(p.last_seen)}`}</div>
                  </td>
                  <Version v={p.version} />
                  <td>
                    <PathBadge path={p.path} online={p.online} />
                    {p.path.kind === 'relay' && p.path.direct_error && <div className="muted small" title={p.path.direct_error}>Direct: not possible yet</div>}
                  </td>
                  <td className="num">{p.online ? fmtMs(p.path.rtt_ms) : '—'}</td>
                  <td className="num">{p.online ? fmtSpeed(p.path.bps) : '—'}</td>
                  <td>{fmtBytes(p.bytes_out)}<div className="muted small">{p.replicas_out} blocks</div></td>
                  <td>{fmtBytes(p.held_bytes)}<div className="muted small">{p.held_blocks} blocks</div></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <Card title="Relays">
        {relays.length === 0 ? (
          <Empty>
            No relays. A relay lets other sites reach this one without open ports.{' '}
            {data?.site.peer_advertise ? <>This site has a direct address instead: <span className="mono">{data.site.peer_advertise}</span>.</> : <>Add one to connect to other sites.</>}
          </Empty>
        ) : (
          <table>
            <thead><tr><th>Relay</th><th>Status</th><th>Version</th><th>Ping</th><th>This site as seen by the relay</th><th /></tr></thead>
            <tbody>
              {relays.map((r) => (
                <tr key={r.id}>
                  <td>{r.name}<div className="muted mono small">{r.address}</div></td>
                  <td>
                    <Status online={r.connected} label={r.connected ? 'connected' : 'disconnected'} />
                    {!r.connected && r.error && <div className="muted small">{r.error}</div>}
                  </td>
                  <Version v={r.version} />
                  <td className="num">{r.connected ? fmtMs(r.rtt_ms) : '—'}</td>
                  <td className="mono small">{r.public || '—'}</td>
                  <td className="right row-end">
                    <button className="btn small" onClick={() => setDialog({ kind: 'move', relay: r })}>Edit address</button>
                    <button className="btn small danger" onClick={() => removeRelay(r)}>Remove</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <Card title="Direct address" actions={<button className="btn small" onClick={() => setDialog({ kind: 'address' })}>Edit</button>}>
        {data?.site.peer_advertise
          ? <p>Other sites can also reach this site at <span className="mono">{data.site.peer_advertise}</span>.</p>
          : <p className="muted small">None. Other sites reach this site through its relays, and connect directly on their own when the routers allow it.</p>}
        {data?.site.web_host && (
          <p className="muted small">
            Other sites also try <span className="mono">{data.site.web_host}:7443</span>, the name this page was opened with.
            That's a guess at this site's address on your network; set a direct address if it's wrong.
          </p>
        )}
      </Card>

      {indirect.length > 0 && (
        <Card title="Other sites in the IBS">
          <p className="muted small">Known through the sites this one is connected to. This site connects to them on its own; until then they're listed here.</p>
          <table>
            <thead><tr><th>Site</th><th>Status</th><th>Version</th><th>Uptime</th><th>Remaining storage</th><th>Last seen</th></tr></thead>
            <tbody>
              {indirect.map((n) => (
                <tr key={n.site_id}>
                  <td>{n.name} <span className="muted mono small">{shortId(n.site_id)}</span></td>
                  <td><Status online={n.online} /></td>
                  <Version v={n.version} />
                  <td>{uptime(n.up_since, n.online)}</td>
                  <td>{fmtBytes(n.free)}</td>
                  <td>{ago(n.last_seen)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}

      {peers.length > 0 && (
        <Card title="Leave the IBS">
          <p className="muted small">
            Takes this site out of the IBS. Every site forgets it and deletes the copies it keeps for this site, and this
            site deletes the copies it keeps for them. Your own files stay here. To come back, join with a new invite.
          </p>
          <button className="btn small danger" onClick={() => setDialog({ kind: 'leave' })}>Leave the IBS</button>
        </Card>
      )}

      {dialog?.kind === 'address' && <AddressDialog current={data?.site.peer_advertise ?? ''} onClose={close} onDone={done} />}
      {dialog?.kind === 'leave' && <LeaveDialog sites={peers.length} onClose={close} onDone={done} />}
      {dialog?.kind === 'invite' && <InviteDialog reachable={reachable} onClose={close} onAddRelay={() => setDialog({ kind: 'relay' })} />}
      {dialog?.kind === 'join' && <JoinDialog onClose={close} onDone={done} />}
      {dialog?.kind === 'relay' && <RelayDialog onClose={close} onDone={done} />}
      {dialog?.kind === 'move' && <MoveRelayDialog relay={dialog.relay} onClose={close} onDone={done} />}
    </div>
  )
}

/** Runs an action for a dialog form, keeping its busy and error state. */

function InviteDialog({ reachable, onClose, onAddRelay }: { reachable: boolean; onClose: () => void; onAddRelay: () => void }) {
  const [invite, setInvite] = useState<string>()
  const { busy, error, submit } = useSubmit(async () => setInvite((await api<{ token: string }>('POST', '/api/peers/invite')).token))
  useEffect(() => { if (reachable) submit() }, []) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <Modal title="Invite a site" onClose={onClose}>
      {!reachable ? (
        <>
          <p className="muted">Other sites cannot reach this site yet. Add a relay first, or set a direct address (on this page).</p>
          <div className="row"><button className="btn primary" onClick={onAddRelay}>Add relay</button></div>
        </>
      ) : (
        <>
          <p className="muted small">
            Give this invite to the other site; they paste it under <b>Join a site</b>. It brings that site into the IBS:
            it connects to every site here, not just this one. It works once and is valid for 24 hours.
          </p>
          {invite && <textarea readOnly rows={5} className="mono" value={invite} onFocus={(e) => e.target.select()} />}
          {busy && <p className="muted small">Creating invite…</p>}
          <ErrorBox error={error} />
          <div className="row">
            {invite && <Copy text={invite} label="Copy invite" />}
            <button className="btn" onClick={onClose}>Done</button>
          </div>
        </>
      )}
    </Modal>
  )
}

function JoinDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [token, setToken] = useState('')
  const { busy, error, submit } = useSubmit(async () => { await api('POST', '/api/peers/join', { token: token.trim() }); onDone() })
  return (
    <Modal title="Join a site" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <label>
          Invite
          <textarea autoFocus rows={5} className="mono" placeholder="ibs1.…" value={token} onChange={(e) => setToken(e.target.value)} />
          <small className="muted">Create it on the other site under <b>Invite a site</b>.</small>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!token.trim() || busy}>{busy ? 'Connecting…' : 'Connect'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function RelayDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [address, setAddress] = useState('')
  const [token, setToken] = useState('')
  const { busy, error, submit } = useSubmit(async () => { await api('POST', '/api/relays', { address: address.trim(), token: token.trim() }); onDone() })
  return (
    <Modal title="Add relay" onClose={onClose}>
      <p className="muted small">
        A relay (<span className="mono">ibs-relay</span>) runs on a machine with one open port, yours or another IBS
        member's. This site keeps an outgoing connection to it, so it needs no open ports itself. Joining a site with an
        invite adds that site's relays automatically.
      </p>
      <form className="stack" onSubmit={submit}>
        <label>
          Relay address
          <input autoFocus className="mono" placeholder="relay.example.com:7444" value={address} onChange={(e) => setAddress(e.target.value)} />
          <small className="muted">The relay's public host name or IP, port 7444 by default.</small>
        </label>
        <label>
          Relay token
          <textarea rows={3} className="mono" placeholder="ibsr2.…" value={token} onChange={(e) => setToken(e.target.value)} />
          <small className="muted">Printed in the relay's log when it starts; ask whoever runs the relay.</small>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!address.trim() || !token.trim() || busy}>{busy ? 'Connecting…' : 'Add relay'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function MoveRelayDialog({ relay, onClose, onDone }: { relay: RelayInfo; onClose: () => void; onDone: () => void }) {
  const [address, setAddress] = useState(relay.address)
  const { busy, error, submit } = useSubmit(async () => { await api('PATCH', `/api/relays/${relay.id}`, { address: address.trim() }); onDone() })
  return (
    <Modal title="Edit relay address" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <label>
          Relay address
          <input autoFocus className="mono" value={address} onChange={(e) => setAddress(e.target.value)} />
          <small className="muted">Use this when the relay moved to a new host or IP. Its key stays the same.</small>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!address.trim() || address.trim() === relay.address || busy}>{busy ? 'Saving…' : 'Save'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function LeaveDialog({ sites, onClose, onDone }: { sites: number; onClose: () => void; onDone: () => void }) {
  const [confirmText, setConfirmText] = useState('')
  const { busy, error, submit } = useSubmit(async () => { await api('POST', '/api/ibs/leave'); onDone() })
  return (
    <Modal title="Leave the IBS" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <p>
          This site is connected to {sites} {sites === 1 ? 'site' : 'sites'}. If it leaves:
        </p>
        <ul className="small">
          <li>Your files have no copies on other sites anymore: they are only here.</li>
          <li>The copies of other sites' data kept here are deleted.</li>
          <li>Sites that are offline now are told when they come back.</li>
        </ul>
        <label>
          Type <b>leave</b> to confirm
          <input value={confirmText} onChange={(e) => setConfirmText(e.target.value)} autoFocus />
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn danger" disabled={busy || confirmText.trim().toLowerCase() !== 'leave'}>{busy ? 'Leaving…' : 'Leave the IBS'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function AddressDialog({ current, onClose, onDone }: { current: string; onClose: () => void; onDone: () => void }) {
  const [address, setAddress] = useState(current)
  const { busy, error, submit } = useSubmit(async () => {
    const settings = await api<Settings>('GET', '/api/settings')
    await api('PUT', '/api/settings', { ...settings, peer_advertise: address.trim() })
    onDone()
  })
  return (
    <Modal title="Direct address" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <p className="muted small">
          Optional. Only needed when other sites can reach this site's port 7443 at a fixed address: on the same network,
          over a VPN such as WireGuard or Tailscale, or through a port forward. This site can't find that address by itself
          (inside Docker it only sees its container's address), so enter it here. Invites carry it, and every site in the IBS
          learns it as a way to reach this site. Without one, this site still offers the name this page is opened with as a
          guess.
        </p>
        <label>
          Address
          <input className="mono" value={address} placeholder="192.168.1.20:7443" onChange={(e) => setAddress(e.target.value)} autoFocus />
          <small className="muted">Host or IP and port 7443, as the other sites can reach it.</small>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={busy || address.trim() === current}>{busy ? 'Saving…' : 'Save'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}
