import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, NetSite, RelayNode } from './api'
import { ago, fmtMs, fmtSpeed, shortId, uptime } from './ui'

type SiteNode = { id: string; name: string; self: boolean; online: boolean; upSince: number; free: number; x: number; y: number }
type RelayPos = RelayNode & { x: number; y: number }
type Edge = { key: string; a: string; b: string; link: Link }

/** Copy traffic between this site and each other site, bytes per second. */
export type Rates = Record<string, { out: number; in: number }>

export function ratesOf(peers: { site_id: string; out_bps: number; in_bps: number }[]): Rates {
  return Object.fromEntries(peers.map((p) => [p.site_id, { out: p.out_bps, in: p.in_bps }]))
}

type Props = {
  selfId: string
  selfName: string
  selfUpSince: number
  sites: NetSite[]
  links: Link[]
  relays: RelayNode[]
  compact?: boolean
  rates?: Rates
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

const relayClearance = 46 // how far a direct line stays from a relay it doesn't use

/**
 * A direct connection: a straight line, or, when that would pass a relay, a
 * curve around it, so it never looks like it goes through the relay.
 */
function directPath(a: { x: number; y: number }, b: { x: number; y: number }, relays: { x: number; y: number }[]) {
  const mx = (a.x + b.x) / 2
  const my = (a.y + b.y) / 2
  const dx = b.x - a.x
  const dy = b.y - a.y
  const len = Math.hypot(dx, dy) || 1
  let nx = -dy / len
  let ny = dx / len
  // The relay closest to the line, if it's in the way.
  let near: { x: number; y: number } | undefined
  let nearDist = relayClearance
  for (const r of relays) {
    const t = Math.max(0, Math.min(1, ((r.x - a.x) * dx + (r.y - a.y) * dy) / (len * len)))
    const d = Math.hypot(a.x + t * dx - r.x, a.y + t * dy - r.y)
    if (d < nearDist) { near = r; nearDist = d }
  }
  if (!near) return { d: `M${a.x},${a.y}L${b.x},${b.y}`, lx: mx, ly: my }
  // Bend to the side away from the relay (below it when it's right on the line).
  let side = (near.x - mx) * nx + (near.y - my) * ny
  if (Math.abs(side) < 1 ? ny < 0 : side > 0) { nx = -nx; ny = -ny; side = -side }
  const k = 2 * (relayClearance + side) // side <= 0: the curve's middle ends up relayClearance from the relay
  const qx = mx + nx * k
  const qy = my + ny * k
  return { d: `M${a.x},${a.y}Q${qx},${qy} ${b.x},${b.y}`, lx: mx + (nx * k) / 2, ly: my + (ny * k) / 2 }
}

function edgeClass(l: Link) {
  if (!l.online || !l.kind) return 'edge offline'
  return l.kind === 'relay' ? 'edge relayed' : 'edge direct'
}

/**
 * Map of the IBS: sites on a ring (this site on top, or on the left when
 * there are two), relays in the middle. Direct connections are solid lines
 * that bend around relays in their way; relayed ones are dashed curves
 * through their relay. There are no labels: pointing at a site or relay shows its
 * name, and selecting a site or connection shows its details below. While
 * copies move between this site and another, dots run along their connection
 * in the direction they go; pointing at it shows the speed.
 */
export default function NetworkGraph({ selfId, selfName, selfUpSince, sites, links, relays, compact, rates = {} }: Props) {
  const [selected, setSelected] = useState<string>()
  const [hoverEdge, setHoverEdge] = useState<string>()
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
    // Two sites side by side, more around a ring starting at the top.
    const start = ring.length === 2 ? Math.PI : -Math.PI / 2
    ring.forEach((n, i) => {
      const angle = start + (2 * Math.PI * i) / Math.max(ring.length, 1)
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
  // Sites with a connection through a relay, as "site|relay".
  const throughRelay = new Set(edges.filter((e) => e.link.online && e.link.kind === 'relay' && e.link.relay)
    .flatMap((e) => [`${e.a}|${e.link.relay}`, `${e.b}|${e.link.relay}`]))
  const relayOf = (id?: string) => relayPos.find((r) => r.id === id)

  // Geometry of an edge: straight for direct, a curve through the relay otherwise.
  const geometry = (e: Edge) => {
    const a = node(e.a)!
    const b = node(e.b)!
    const r = e.link.kind === 'relay' ? relayOf(e.link.relay) : undefined
    if (!r) return directPath(a, b, relayPos)
    // Control point so the curve passes through the relay at its middle.
    const qx = 2 * r.x - (a.x + b.x) / 2
    const qy = 2 * r.y - (a.y + b.y) / 2
    const t = e.a === selfId ? 0.72 : e.b === selfId ? 0.28 : 0.5 // label away from this site and the relay
    const lx = (1 - t) ** 2 * a.x + 2 * (1 - t) * t * qx + t ** 2 * b.x
    const ly = (1 - t) ** 2 * a.y + 2 * (1 - t) * t * qy + t ** 2 * b.y
    return { d: `M${a.x},${a.y}Q${qx},${qy} ${b.x},${b.y}`, lx, ly }
  }

  // Copy traffic on an edge of this site, from a to b and back.
  const flow = (e: Edge) => {
    const other = e.a === selfId ? e.b : e.b === selfId ? e.a : undefined
    const r = other ? rates[other] : undefined
    if (!r) return { ab: 0, ba: 0 }
    return e.a === selfId ? { ab: r.out, ba: r.in } : { ab: r.in, ba: r.out }
  }
  const reduceMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches

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
          // A connection through the relay already runs through it.
          if (!a || !r || throughRelay.has(`${u.from}|${u.relay}`)) return null
          return <line key={u.from + u.to} x1={a.x} y1={a.y} x2={r.x} y2={r.y} className={`uplink ${u.online ? '' : 'offline'}`} />
        })}
        {edges.map((e) => {
          const g = geometry(e)
          const mine = e.a === selfId || e.b === selfId
          return (
            <g key={e.key} className={`${edgeClass(e.link)} ${mine ? 'mine' : ''} ${sel === e.key ? 'selected' : ''}`}
              onClick={() => setSelected(e.key)} onMouseEnter={() => setHoverEdge(e.key)} onMouseLeave={() => setHoverEdge(undefined)}>
              <path d={g.d} className="hit" />
              <path d={g.d} className="line" />
              {(['ab', 'ba'] as const).map((dir) => flow(e)[dir] > 0 && [0, 1, 2].map((i) => (
                <circle key={dir + i} r={5} className="flow" pointerEvents="none">
                  {reduceMotion ? null : (
                    <animateMotion dur="1.8s" begin={`${-i * 0.6}s`} repeatCount="indefinite" path={g.d}
                      keyPoints={dir === 'ab' ? '0;1' : '1;0'} keyTimes="0;1" calcMode="linear" />
                  )}
                </circle>
              )))}
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
        {edges.map((e) => {
          // While copies move: how fast, and to which site, on the connection.
          const f = flow(e)
          const lines = [
            f.ab > 0 && `${fmtSpeed(f.ab)} → ${node(e.b)?.name}`,
            f.ba > 0 && `${fmtSpeed(f.ba)} → ${node(e.a)?.name}`,
          ].filter(Boolean) as string[]
          if (lines.length === 0) return null
          const g = geometry(e)
          const w = 16 + Math.max(...lines.map((l) => l.length)) * 6.6
          const h = 8 + lines.length * 15
          return (
            <g key={'flow' + e.key} className="edge-label" pointerEvents="none"
              transform={`translate(${Math.min(Math.max(g.lx, w / 2 + 2), W - w / 2 - 2)},${g.ly + 16})`}>
              <rect x={-w / 2} y={-h / 2} width={w} height={h} rx={6} />
              {lines.map((l, i) => <text key={i} textAnchor="middle" y={-h / 2 + 15 + i * 15}>{l}</text>)}
            </g>
          )
        })}
        {(() => {
          // Pointing at a connection: how fast copies move over it now.
          const e = !hover && hoverEdge ? edges.find((x) => x.key === hoverEdge) : undefined
          if (!e) return null
          const f = flow(e)
          const parts = [
            f.ab > 0 && `${fmtSpeed(f.ab)} to ${node(e.b)?.name}`,
            f.ba > 0 && `${fmtSpeed(f.ba)} to ${node(e.a)?.name}`,
          ].filter(Boolean) as string[]
          const text = parts.length ? 'Copying ' + parts.join(', ') + ` (line ${fmtSpeed(e.link.bps)})` : kindLabel(e.link)
          const g = geometry(e)
          const w = 16 + text.length * 6.8
          return (
            <g className="node-tip" transform={`translate(${Math.min(Math.max(g.lx, w / 2 + 2), W - w / 2 - 2)},${Math.max(g.ly - 18, 13)})`} pointerEvents="none">
              <rect x={-w / 2} y={-12} width={w} height={24} rx={6} />
              <text textAnchor="middle" y={4}>{text}</text>
            </g>
          )
        })()}
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
        {Object.values(rates).some((r) => r.out > 0 || r.in > 0) && <span><i className="sw flow" />Copies on their way</span>}
        <span className="muted">Select a site or connection for details</span>
      </div>

      {(selEdge || selNode || selRelay) && (
        <div className="netgraph-detail">
          {selEdge && (
            <>
              <strong>{node(selEdge.a)?.name} ↔ {node(selEdge.b)?.name}</strong>
              <span>{kindLabel(selEdge.link)}</span>
              <span>Ping {fmtMs(selEdge.link.rtt_ms)}</span>
              <span title="What the connection can carry, measured on large transfers">Line speed {fmtSpeed(selEdge.link.bps)}</span>
              {(flow(selEdge).ab > 0 || flow(selEdge).ba > 0) && (
                <span title="What actually arrives, over the last 10 seconds; less than the line speed, as each copy is also read and stored">Copying {[flow(selEdge).ab > 0 && `${fmtSpeed(flow(selEdge).ab)} to ${node(selEdge.b)?.name}`,
                  flow(selEdge).ba > 0 && `${fmtSpeed(flow(selEdge).ba)} to ${node(selEdge.a)?.name}`].filter(Boolean).join(', ')}</span>
              )}
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
