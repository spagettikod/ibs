import { FormEvent, useState } from 'react'
import { api, NodeInfo } from '../api'

type JoinToken = { token: string; address: string; configured_address: string }
import { ago, Card, Copy, Counted, ErrorBox, fmtBytes, Meter, Modal, Status, useLoader, useTick, Version } from '../ui'

export default function NodesPage() {
  const { data, error, reload } = useLoader(() => api<NodeInfo[]>('GET', '/api/nodes'), ['nodes', 'replication', 'data'])
  useTick(5000)
  const token = useLoader(() => api<JoinToken>('GET', '/api/nodes/join-token'))
  const [editing, setEditing] = useState<NodeInfo>()

  const rotate = async () => {
    if (!confirm('Create a new join token? The old token stops working for new nodes. Nodes that already joined keep working.')) return
    await api('POST', '/api/nodes/join-token')
    token.reload()
  }
  const remove = async (n: NodeInfo) => {
    if (!confirm(`Remove node "${n.name}"? Blocks stored on it are fetched back from the copies on other sites; blocks not copied yet are lost.`)) return
    await api('DELETE', `/api/nodes/${n.id}`).catch((e) => alert(e.message))
    reload()
  }

  const t = token.data
  const current = data?.find((n) => n.local)?.metadata_generation ?? 0
  const manager = t?.address || '<manager-host>:7443'
  const cmd = t
    ? `docker run -d --name ibs-node --restart unless-stopped \\
  -v /srv/ibs-node:/data -p 7480:7480 \\
  -e IBS_MANAGER=${manager} \\
  -e IBS_STORAGE_LIMIT=500G \\
  ibs node ${t.token}`
    : ''

  return (
    <div className="page">
      <header className="page-head"><h1>Nodes</h1></header>
      <p className="muted">
        Storage nodes add capacity to this site. The manager spreads blocks across all online nodes, weighted by free space.
        If a node stays offline longer than the grace period, its blocks are restored on the other nodes from copies on the
        IBS; when it returns, its outdated copies are removed.
      </p>
      <details className="card howto">
        <summary>
          <h2>Add a node</h2>
          <span className="muted small">Show instructions</span>
        </summary>
        {t && (
          <div className="stack">
            <p className="muted small">Run this on the machine that should become a node:</p>
            <pre className="code">{cmd}</pre>
            <div className="row">
              <Copy text={cmd} label="Copy command" />
              <Copy text={t.token} label="Copy token" />
              <button className="btn small" onClick={rotate}>New token</button>
            </div>
            <table className="params">
              <tbody>
                <tr>
                  <td className="mono">IBS_MANAGER</td>
                  <td>
                    Address of this manager as the node reaches it: host and the manager's port 7443, on your local network.
                    {!t.configured_address && <> Taken from how you opened this page; set it below if nodes reach the manager differently.</>}
                  </td>
                </tr>
                <tr>
                  <td className="mono">IBS_STORAGE_LIMIT</td>
                  <td>
                    Optional. How much of the node's disk IBS may use, e.g. <code>500G</code> or <code>2T</code>. Leave the line
                    out to share all free space, or to set the limit here on the Nodes page instead. When set, it takes
                    precedence over the Nodes page. The same setting works on the manager; <code>0</code> means it stores
                    nothing itself.
                  </td>
                </tr>
                <tr>
                  <td className="mono">node &lt;token&gt;</td>
                  <td>Contains this manager's key and the join secret, so the node and the manager use mutual TLS from the first connection. Only needed on the first start; a new token doesn't affect nodes that already joined.</td>
                </tr>
                <tr>
                  <td className="mono">-v …:/data</td>
                  <td>Where the node keeps its blocks and identity.</td>
                </tr>
                <tr>
                  <td className="mono">-p 7480:7480</td>
                  <td>The manager connects to the node on this port, at the address the node registers from. Add <code>-e IBS_NODE_ADVERTISE=host:7480</code> only if that address isn't reachable (NAT).</td>
                </tr>
              </tbody>
            </table>
            <ManagerAddress data={t} onSaved={token.reload} />
          </div>
        )}
      </details>
      <ErrorBox error={error} />
      <Card>
        <table>
          <thead><tr><th>Node</th><th>Status</th><th>Version</th><th>Address</th><th>Blocks</th><th>Storage for IBS</th><th>Shared</th><th>Metadata copy</th><th /></tr></thead>
          <tbody>
            {data?.map((n) => (
              <tr key={n.id}>
                <td>{n.name} {n.local && <span className="tag">manager</span>}<div className="muted mono small">{n.id}</div></td>
                <td>
                  <Status online={n.online} label={n.lost ? 'lost' : undefined} />
                  {!n.online && <div className="muted small">seen {ago(n.last_seen)}</div>}
                  {n.lost && <div className="small warn-text">Restoring its blocks on other nodes from the IBS</div>}
                  {n.quota < 0 && n.blocks + n.held_blocks > 0 && n.online && (
                    <div className="small warn-text">Shares nothing: moving its {(n.blocks + n.held_blocks).toLocaleString()} blocks to other nodes</div>
                  )}
                  {n.stranded > 0 && (
                    <div className="small warn-text" title="These blocks are not copied to other sites yet.">
                      {n.stranded} blocks exist only here
                    </div>
                  )}
                </td>
                <Version v={n.version} />
                <td className="mono small">{n.address || '—'}</td>
                <td><Counted ok={n.counted}>{n.blocks.toLocaleString()}
                  {n.held_blocks > 0 && <div className="muted small">+ {n.held_blocks.toLocaleString()} copies kept for other sites</div>}</Counted></td>
                <td className="meter-cell">
                  <Counted ok={n.counted}><Meter used={n.used} total={n.used + n.free} />
                  <span className="muted small">{fmtBytes(n.used)} used · {fmtBytes(n.free)} free</span></Counted>
                  {n.quota > 0 && n.used >= n.quota && <div className="small warn-text">Over the limit, no new blocks</div>}
                </td>
                <td>
                  {n.quota < 0 ? 'Nothing' : n.quota ? `Up to ${fmtBytes(n.quota)}` : `All free space: ${fmtBytes(n.used + n.disk_free)}`}
                  <div className="muted small">
                    {n.quota > 0 && n.quota > n.used + n.disk_free && `IBS can use ${fmtBytes(n.used + n.disk_free)} of it; `}
                    {fmtBytes(n.disk_free)} empty on the disk{n.limit_from_env ? ', set with IBS_STORAGE_LIMIT' : ''}
                  </div>
                </td>
                <td><MetadataCopy node={n} current={current} /></td>
                <td className="right">
                  <div className="row-end">
                    {!n.limit_from_env && <button className="btn small" onClick={() => setEditing(n)}>Change share</button>}
                    {!n.local && <button className="btn small danger" onClick={() => remove(n)}>Remove</button>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
      {editing && <LimitDialog node={editing} onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); reload() }} />}
    </div>
  )
}

function LimitDialog({ node, onClose, onSaved }: { node: NodeInfo; onClose: () => void; onSaved: () => void }) {
  const GiB = 1024 ** 3
  const [mode, setMode] = useState<'whole' | 'size' | 'nothing'>(node.quota < 0 ? 'nothing' : node.quota > 0 ? 'size' : 'whole')
  const [size, setSize] = useState(node.quota > 0 ? `${Math.round((node.quota / GiB) * 100) / 100}G` : '')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (mode === 'nothing' && node.blocks > 0 && !confirm(`Move the ${node.blocks} blocks on ${node.name} to other nodes? Make sure they have room.`)) return
    setBusy(true)
    setError(undefined)
    try {
      await api('PUT', `/api/nodes/${node.id}/limit`, { limit: mode === 'whole' ? '' : mode === 'nothing' ? '0' : size.trim() })
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal title={`Storage on ${node.name}`} onClose={onClose}>
      <form className="stack" onSubmit={save}>
        <p className="muted small">
          How much of this node's disk IBS may use, for your files and other sites' copies together. IBS uses{' '}
          {fmtBytes(node.used)} on it now and the disk has {fmtBytes(node.disk_free)} empty, so IBS can use at most{' '}
          <b>{fmtBytes(node.used + node.disk_free)}</b>.
        </p>
        <label className="check"><input type="radio" checked={mode === 'whole'} onChange={() => setMode('whole')} /><span>All free space <small className="muted">— grows and shrinks with what else is on the disk</small></span></label>
        <label className="check">
          <input type="radio" checked={mode === 'size'} onChange={() => setMode('size')} />
          <span className="stack">
            Up to
            <input className="mono" value={size} placeholder="500G" disabled={mode !== 'size'} onChange={(e) => setSize(e.target.value)} />
            <small className="muted">For example 500G or 2T. A limit below what's used deletes nothing; the node just gets no new data.</small>
          </span>
        </label>
        <label className="check">
          <input type="radio" checked={mode === 'nothing'} onChange={() => setMode('nothing')} />
          <span>Nothing <small className="muted">— its data moves to the other nodes, e.g. before retiring it</small></span>
        </label>
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={busy || (mode === 'size' && !size.trim())}>{busy ? 'Saving…' : 'Save'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function ManagerAddress({ data, onSaved }: { data: JoinToken; onSaved: () => void }) {
  const [value, setValue] = useState(data.configured_address)
  const [error, setError] = useState<string>()
  const save = async (e: FormEvent) => {
    e.preventDefault()
    try {
      await api('PUT', '/api/nodes/address', { address: value })
      setError(undefined)
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  return (
    <form className="inline-form node-address" onSubmit={save}>
      <label>
        Manager address for nodes (IBS_MANAGER)
        <input value={value} placeholder={data.address || 'host:7443'} onChange={(e) => setValue(e.target.value)} />
      </label>
      <button className="btn" disabled={value === data.configured_address}>Save</button>
      <p className="muted small">
        {data.configured_address
          ? 'Used in the command above.'
          : `Not set: the command uses ${data.address || 'the address of this page'}.`}
      </p>
      {error && <div className="error small">{error}</div>}
    </form>
  )
}

/** How current a node's copy of the site metadata is. */
function MetadataCopy({ node, current }: { node: NodeInfo; current: number }) {
  if (node.local) return <span className="muted small">Original</span>
  if (node.metadata_generation < 0) return <span className="muted small">{node.online ? 'Pending' : '—'}</span>
  if (node.metadata_generation >= current) return <span className="pill ok">Up to date</span>
  return <span className="pill warn" title={`Version ${node.metadata_generation} of ${current}`}>Updating</span>
}
