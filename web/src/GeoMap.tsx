import L from 'leaflet'
import 'leaflet/dist/leaflet.css'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, Location, NetSite } from './api'
import NetworkGraph from './NetworkGraph'

// Map tiles come from OpenStreetMap; only the map images are fetched, nothing
// about the IBS is sent there. Shown in greyscale (inverted in dark mode).
const TILES = 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
const ATTRIBUTION = '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>'

const css = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim()

export function baseMap(el: HTMLElement, opts: L.MapOptions = {}) {
  const map = L.map(el, { zoomControl: true, attributionControl: true, worldCopyJump: true, ...opts })
  L.tileLayer(TILES, { maxZoom: 18, attribution: ATTRIBUTION }).addTo(map)
  return map
}

type Props = {
  self: { id: string; name: string; location?: Location }
  sites: NetSite[]
  links: Link[]
  compact?: boolean
}

/** The IBS on a map: sites at their (approximate) places, links between them. */
export default function GeoMap({ self, sites, links, compact }: Props) {
  const el = useRef<HTMLDivElement>(null)
  const map = useRef<L.Map>()
  const layer = useRef<L.LayerGroup>()
  const all = useMemo(() => [{ site_id: self.id, name: self.name, location: self.location, online: true, self: true },
    ...sites.map((s) => ({ ...s, self: false }))], [self, sites])
  const placed = all.filter((s) => s.location)
  const missing = all.filter((s) => !s.location)

  useEffect(() => {
    if (!el.current) return
    map.current = baseMap(el.current, { scrollWheelZoom: !compact })
    layer.current = L.layerGroup().addTo(map.current)
    return () => { map.current?.remove(); map.current = undefined }
  }, [compact])

  // Draw sites and links; fit the view only when the set of placed sites changes.
  const placedKey = placed.map((s) => s.site_id).sort().join(',')
  useEffect(() => {
    const m = map.current, g = layer.current
    if (!m || !g) return
    g.clearLayers()
    const pos = new Map(placed.map((s) => [s.site_id, [s.location!.lat, s.location!.lon] as L.LatLngTuple]))
    const seen = new Set<string>()
    for (const l of links) {
      if (l.kind === 'uplink') continue
      const key = [l.from, l.to].sort().join('|')
      const a = pos.get(l.from), b = pos.get(l.to)
      if (seen.has(key) || !a || !b) continue
      seen.add(key)
      const relayed = l.kind === 'relay'
      const up = l.online && !!l.kind
      L.polyline([a, b], {
        color: !up ? css('--muted') : relayed ? css('--warn') : css('--ok'),
        weight: l.from === self.id || l.to === self.id ? 3 : 2,
        dashArray: !up ? '2 6' : relayed ? '8 6' : undefined,
        opacity: up ? 0.9 : 0.6,
      }).bindTooltip(`${!up ? 'Not connected' : relayed ? `Via relay${l.relay_name ? ' ' + l.relay_name : ''}` : 'Direct'}`).addTo(g)
    }
    for (const s of placed) {
      L.circleMarker(pos.get(s.site_id)!, {
        radius: s.self ? 9 : 7, weight: 2, color: '#fff', fillOpacity: 1,
        fillColor: s.self ? css('--accent') : s.online ? css('--ok') : css('--muted'),
      }).bindTooltip(s.self ? `${s.name} (this site)` : s.name, { permanent: true, direction: 'top', offset: [0, -8], className: 'map-label' })
        .addTo(g)
    }
    // A small area around each site: it is shown, not exactly located.
    if (!compact) {
      for (const s of placed) L.circle(pos.get(s.site_id)!, { radius: 1000, weight: 0, fillColor: css('--muted'), fillOpacity: 0.12, interactive: false }).addTo(g)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [placed, links, self.id, compact])

  useEffect(() => {
    const m = map.current
    if (!m) return
    const pts = placed.map((s) => [s.location!.lat, s.location!.lon] as L.LatLngTuple)
    if (pts.length === 0) m.setView([30, 10], 2)
    else if (pts.length === 1) m.setView(pts[0], 8)
    else m.fitBounds(L.latLngBounds(pts), { padding: [40, 40], maxZoom: 10 })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [placedKey])

  return (
    <div className="geo">
      <div ref={el} className={`geo-map ${compact ? 'compact' : ''}`} />
      {missing.length > 0 && (
        <p className="muted small">
          Not on the map: {missing.map((s) => s.self ? `${s.name} (this site, set it under Settings → Location)` : s.name).join(', ')}.
        </p>
      )}
    </div>
  )
}

const VIEW_KEY = 'ibs.networkView'
function storedView(): 'diagram' | 'map' {
  try {
    return localStorage.getItem(VIEW_KEY) === 'map' ? 'map' : 'diagram'
  } catch {
    return 'diagram'
  }
}

/** Switch between the diagram and the map; the choice is remembered in this browser. */
export function useNetworkView() {
  const [view, setView] = useState(storedView)
  const set = (v: 'diagram' | 'map') => {
    setView(v)
    try { localStorage.setItem(VIEW_KEY, v) } catch { /* private mode */ }
    window.dispatchEvent(new Event('ibs:network-view'))
  }
  useEffect(() => {
    const on = () => setView(storedView())
    window.addEventListener('ibs:network-view', on)
    return () => window.removeEventListener('ibs:network-view', on)
  }, [])
  return [view, set] as const
}

export function ViewSwitch({ view, onChange }: { view: 'diagram' | 'map'; onChange: (v: 'diagram' | 'map') => void }) {
  return (
    <div className="tabs view-switch">
      <button type="button" className={view === 'diagram' ? 'active' : ''} onClick={() => onChange('diagram')}>Diagram</button>
      <button type="button" className={view === 'map' ? 'active' : ''} onClick={() => onChange('map')}>Map</button>
    </div>
  )
}

type ViewProps = Props & { relays: { id: string; name: string }[]; upSince: number; view: 'diagram' | 'map' }

/** The IBS as a diagram or on a map. */
export function NetworkView({ view, self, sites, links, relays, upSince, compact }: ViewProps) {
  return view === 'map'
    ? <GeoMap self={self} sites={sites} links={links} compact={compact} />
    : <NetworkGraph compact={compact} selfId={self.id} selfName={self.name} selfUpSince={upSince} sites={sites} links={links} relays={relays} />
}
