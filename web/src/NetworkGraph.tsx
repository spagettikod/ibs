import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, NetSite, RelayNode } from './api'
import { ago, fmtMs, fmtSpeed, shortId, uptime } from './ui'

type SiteNode = { id: string; name: string; self: boolean; online: boolean; upSince: number; free: number; x: number; y: number }
type RelayPos = RelayNode & { x: number; y: number }
type Edge = { key: string; a: string; b: string; link: Link }

type Props = {
  selfId: string
  selfName: string
  selfUpSince: number
  sites: NetSite[]
  links: Link[]
  relays: RelayNode[]
  compact?: boolean
}

function kindLabel(l: Link) {
  if (!l.online || !l.kind) return 'Not connected'
  if (l.kind === 'quic') return 'Direct (QUIC)'
  if (l.kind === 'direct') return 'Direct (TCP)'
  return l.relay_name ? `Via relay · ${l.relay_name}` : 'Via relay'
}

/** Two letters for a site: initials of its name's parts, e.g. "site-a" → "SA". */
function initials(name: string) {
  const parts = name.split(/[^\p{L}\p{N}]+/u).filter(Boolean)
  if (parts.length >= 2) return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
  return name.slice(0, 2).toUpperCase()
}

/** Width of the element in CSS pixels, so the map is laid out at real size. */
function useWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(720)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(([e]) => setWidth(Math.round(e.contentRect.width)))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return { ref, width }
}

function edgeClass(l: Link) {
  if (!l.online || !l.kind) return 'edge offline'
  return l.kind === 'relay' ? 'edge relayed' : 'edge direct'
}

/**
 * Map of the IBS: sites on a ring (this site on top), relays in the middle.
 * Direct connections are solid lines; relayed ones are dashed curves through
 * their relay. There are no labels: pointing at a site or relay shows its
 * name, and selecting a site or connection shows its details below.
 */
export default function NetworkGraph({ selfId, selfName, selfUpSince, sites, links, relays, compact }: Props) {
  const [selected, setSelected] = useState<string>()
  const [hover, setHover] = useState<string>() // node or 'relay:<id>' under the pointer: its name is shown
  const { ref, width } = useWidth()
  const W = Math.max(width, 300)
  const narrow = W < 520
  const H = compact ? (narrow ? 300 : 340) : narrow ? 380 : 460
  const cx = W / 2
  const cy = H / 2

  const { nodes, relayPos, edges, uplinks } = useMemo(() => {
    const byId = new Map<string, SiteNode>()
    const add = (id: string, name: string, online: boolean, upSince: number, free: number, self = false) => {
      if (!byId.has(id)) byId.set(id, { id, name, self, online, upSince, free, x: 0, y: 0 })
    }
    add(selfId, selfName, true, selfUpSince, 0, true)
    for (const s of sites) add(s.site_id, s.name, s.online, s.up_since, s.free)
    for (const l of links) {
      if (!l.to.startsWith('relay:')) add(l.to, shortId(l.to), false, 0, 0)
      add(l.from, shortId(l.from), false, 0, 0)
    }
    const others = [...byId.values()].filter((n) => !n.self).sort((a, b) => a.name.localeCompare(b.name))
    const ring = [byId.get(selfId)!, ...others]
    // An ellipse that fills the box, leaving room for names below nodes.
    const rx = Math.max(Math.min(cx - 56, 300), 60)
    const ry = cy - 52
    ring.forEach((n, i) => {
      const angle = -Math.PI / 2 + (2 * Math.PI * i) / Math.max(ring.length, 1)
      n.x = cx + rx * Math.cos(angle)
      n.y = cy + ry * Math.sin(angle)
    })

    const rr = relays.length > 1 ? 56 : 0
    const relayPos: RelayPos[] = relays.map((r, i) => {
      const angle = Math.PI / 2 + (2 * Math.PI * i) / relays.length
      return { ...r, x: cx + rr * Math.cos(angle), y: cy + rr * Math.sin(angle) }
    })

    // One edge per pair of sites: this site's own report wins, otherwise the newest.
    const pairs = new Map<string, Edge>()
    const uplinks: Link[] = []
    for (const l of links) {
      if (l.kind === 'uplink') {
        uplinks.push(l)
        continue
      }
      const [a, b] = [l.from, l.to].sort()
      const key = `${a}|${b}`
      const cur = pairs.get(key)
      const better = !cur || (l.from === selfId && cur.link.from !== selfId) ||
        (cur.link.from !== selfId && l.updated > cur.link.updated)
      if (better) pairs.set(key, { key, a: l.from, b: l.to, link: l })
    }
    return { nodes: [...byId.values()], relayPos, edges: [...pairs.values()], uplinks }
  }, [selfId, selfName, selfUpSince, sites, links, relays, cx, cy])

  const node = (id: string) => nodes.find((n) => n.id === id)
  const relayOf = (id?: string) => relayPos.find((r) => r.id === id)

  // Geometry of an edge: straight for direct, a curve through the relay otherwise.
  const geometry = (e: Edge) => {
    const a = node(e.a)!
    const b = node(e.b)!
    const r = e.link.kind === 'relay' ? relayOf(e.link.relay) : undefined
    if (!r) {
      return { d: `M${a.x},${a.y}L${b.x},${b.y}`, lx: (a.x + b.x) / 2, ly: (a.y + b.y) / 2 }
    }
    // Control point so the curve passes through the relay at its middle.
    const qx = 2 * r.x - (a.x + b.x) / 2
    const qy = 2 * r.y - (a.y + b.y) / 2
    const t = e.a === selfId ? 0.72 : e.b === selfId ? 0.28 : 0.5 // label away from this site and the relay
    const lx = (1 - t) ** 2 * a.x + 2 * (1 - t) * t * qx + t ** 2 * b.x
    const ly = (1 - t) ** 2 * a.y + 2 * (1 - t) * t * qy + t ** 2 * b.y
    return { d: `M${a.x},${a.y}Q${qx},${qy} ${b.x},${b.y}`, lx, ly }
  }

  const sel = selected
  const selEdge = edges.find((e) => e.key === sel)
  const selNode = nodes.find((n) => n.id === sel)
  const selRelay = relayPos.find((r) => 'relay:' + r.id === sel)

  return (
    <div className="netgraph" ref={ref}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label="Map of the known universe">
        {uplinks.map((u) => {
          const a = node(u.from)
          const r = relayOf(u.relay)
          if (!a || !r) return null
          return <line key={u.from + u.to} x1={a.x} y1={a.y} x2={r.x} y2={r.y} className={`uplink ${u.online ? '' : 'offline'}`} />
        })}
        {edges.map((e) => {
          const g = geometry(e)
          const mine = e.a === selfId || e.b === selfId
          return (
            <g key={e.key} className={`${edgeClass(e.link)} ${mine ? 'mine' : ''} ${sel === e.key ? 'selected' : ''}`}
              onClick={() => setSelected(e.key)}>
              <path d={g.d} className="hit" />
              <path d={g.d} className="line" />
            </g>
          )
        })}
        {relayPos.map((r) => (
          <g key={r.id} className={`relay-node ${sel === 'relay:' + r.id ? 'selected' : ''}`} transform={`translate(${r.x},${r.y})`}
            onClick={() => setSelected('relay:' + r.id)} onMouseEnter={() => setHover('relay:' + r.id)} onMouseLeave={() => setHover(undefined)}>
            <rect x={-14} y={-14} width={28} height={28} rx={6} transform="rotate(45)" />
            <text textAnchor="middle" y={4} className="relay-glyph">R</text>
          </g>
        ))}
        {nodes.map((n) => (
          <g key={n.id} className={`site-node ${n.self ? 'self' : ''} ${n.online ? '' : 'offline'} ${sel === n.id ? 'selected' : ''}`}
            transform={`translate(${n.x},${n.y})`} onClick={() => setSelected(n.id)}
            onMouseEnter={() => setHover(n.id)} onMouseLeave={() => setHover(undefined)}>
            <circle r={n.self ? 24 : 20} />
            <text textAnchor="middle" y={5} className="initials">{initials(n.name)}</text>
          </g>
        ))}
        {(() => {
          // The name of the site or relay under the pointer, above it.
          const n = hover && !hover.startsWith('relay:') ? node(hover) : undefined
          const r = hover?.startsWith('relay:') ? relayPos.find((x) => 'relay:' + x.id === hover) : undefined
          const at = n ?? r
          if (!at) return null
          const text = n ? n.name + (n.self ? ' (this site)' : '') : r!.name
          const w = 16 + text.length * 6.8
          const y = at.y - (n ? (n.self ? 24 : 20) : 18) - 18
          return (
            <g className="node-tip" transform={`translate(${Math.min(Math.max(at.x, w / 2 + 2), W - w / 2 - 2)},${Math.max(y, 13)})`} pointerEvents="none">
              <rect x={-w / 2} y={-12} width={w} height={24} rx={6} />
              <text textAnchor="middle" y={4}>{text}</text>
            </g>
          )
        })()}
      </svg>

      <div className="netgraph-legend">
        <span><i className="sw direct" />Direct</span>
        <span><i className="sw relayed" />Via relay</span>
        <span><i className="sw offline" />Not connected</span>
        <span><i className="sw uplink" />Site to relay</span>
        <span className="muted">Select a site or connection for details</span>
      </div>

      {(selEdge || selNode || selRelay) && (
        <div className="netgraph-detail">
          {selEdge && (
            <>
              <strong>{node(selEdge.a)?.name} ↔ {node(selEdge.b)?.name}</strong>
              <span>{kindLabel(selEdge.link)}</span>
              <span>Ping {fmtMs(selEdge.link.rtt_ms)}</span>
              <span>Speed {fmtSpeed(selEdge.link.bps)}</span>
              <span className="muted">reported by {node(selEdge.link.from)?.name} {ago(selEdge.link.updated)}</span>
            </>
          )}
          {selNode && (
            <>
              <strong>{selNode.name}{selNode.self ? ' (this site)' : ''}</strong>
              <span>{selNode.online ? `Online, up ${uptime(selNode.upSince)}` : 'Offline'}</span>
              <span>{edges.filter((e) => (e.a === selNode.id || e.b === selNode.id) && e.link.online && e.link.kind).length} connections</span>
              <span className="muted mono">{shortId(selNode.id)}</span>
            </>
          )}
          {selRelay && (
            <>
              <strong>Relay {selRelay.name}</strong>
              <span>{uplinks.filter((u) => u.relay === selRelay.id && u.online).length} sites connected</span>
              <span>{edges.filter((e) => e.link.kind === 'relay' && e.link.relay === selRelay.id && e.link.online).length} relayed connections</span>
            </>
          )}
          <button className="btn small ghost" onClick={() => setSelected(undefined)}>Close</button>
        </div>
      )}
    </div>
  )
}
