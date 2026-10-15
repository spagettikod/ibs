import L from 'leaflet'
import { FormEvent, useEffect, useRef, useState } from 'react'
import { api, Location, Settings, State, TLSSettings, TLSStatus } from '../api'
import { baseMap } from '../GeoMap'
import UpdatesSection from './Updates'
import { draftFrom, emptyDraft, HTTPSDraft, HTTPSFields, settingsFrom } from '../HTTPSForm'
import { ago, Card, ErrorBox, MIN_PASSWORD, Modal, PasswordHint, useLoader } from '../ui'

/** The Settings sections, shown as a submenu under Settings in the sidebar. */
export const SETTINGS_SECTIONS = [
  { id: 'general', label: 'General' },
  { id: 'location', label: 'Location' },
  { id: 'https', label: 'HTTPS' },
  { id: 'updates', label: 'Updates' },
] as const

export function settingsSection(): string {
  const s = window.location.hash.replace(/^#\/?/, '').split('/')[1]
  return SETTINGS_SECTIONS.some((x) => x.id === s) ? s : 'general'
}

export default function SettingsPage() {
  const [section, setSection] = useState(settingsSection())
  useEffect(() => {
    const onHash = () => setSection(settingsSection())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])
  const title = SETTINGS_SECTIONS.find((x) => x.id === section)?.label
  return (
    <div className="page">
      <header className="page-head"><h1>Settings <span className="muted">· {title}</span></h1></header>
      {section === 'general' && <SiteSettings section="general" />}
      {section === 'location' && <LocationCard />}
      {section === 'https' && <HTTPSCard />}
      {section === 'updates' && <UpdatesSection />}
    </div>
  )
}

/** One section of the site settings; Save stores the whole settings object. */
function SiteSettings({ section }: { section: 'general' }) {
  const { data, error } = useLoader(() => api<Settings>('GET', '/api/settings'))
  const state = useLoader(() => api<State>('GET', '/api/state'))
  const [form, setForm] = useState<Settings>()
  const [msg, setMsg] = useState<string>()
  const [saveError, setSaveError] = useState<string>()

  useEffect(() => {
    if (data) setForm(data)
  }, [data])

  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (!form) return
    setMsg(undefined)
    setSaveError(undefined)
    try {
      const saved = await api<Settings>('PUT', '/api/settings', form)
      setForm(saved)
      setMsg('Saved')
      window.dispatchEvent(new Event('ibs:renamed')) // the sidebar and tab title show the name
    } catch (err) {
      setSaveError((err as Error).message)
    }
  }

  if (!form) return <ErrorBox error={error} />
  return (
    <>
      <Card>
        <form className="stack narrow" onSubmit={save}>
          {section === 'general' && (
            <label>Site name<input value={form.site_name} onChange={(e) => setForm({ ...form, site_name: e.target.value })} /></label>
          )}
          {section === 'general' && (
            <label>
              Space for other sites' copies
              <select value={form.peer_share} onChange={(e) => setForm({ ...form, peer_share: Number(e.target.value) })}>
                {[25, 50, 75, 100].concat([25, 50, 75, 100].includes(form.peer_share) ? [] : [form.peer_share]).map((p) => (
                  <option key={p} value={p}>{p}% of this site's storage{p === 50 ? ' (default)' : ''}</option>
                ))}
              </select>
              <small className="muted">
                The copies this site keeps for other sites may use at most this much. The rest stays free for this site's own
                files, which can use all of the storage.
              </small>
            </label>
          )}
          <ErrorBox error={saveError} />
          {msg && <div className="success">{msg}</div>}
          <button className="btn primary">Save</button>
        </form>
      </Card>
      {section === 'general' && <PasswordCard />}
      {section === 'general' && (
        <Card title="Identity">
          <p className="muted small">The site ID is derived from the site key, which comes from the recovery phrase. Peers recognize this site by it.</p>
          <p className="mono">{state.data?.site_id}</p>
        </Card>
      )}
    </>
  )
}

function LocationCard() {
  const el = useRef<HTMLDivElement>(null)
  const map = useRef<L.Map>()
  const pinLayer = useRef<L.LayerGroup>()
  const [saved, setSaved] = useState<Location | null>()
  const [pin, setPin] = useState<Location>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api<{ location: Location | null }>('GET', '/api/location').then((r) => setSaved(r.location), (e) => setError(e.message))
  }, [])
  useEffect(() => {
    if (!el.current || saved === undefined || map.current) return
    const m = baseMap(el.current)
    map.current = m
    pinLayer.current = L.layerGroup().addTo(m)
    if (saved) m.setView([saved.lat, saved.lon], 10)
    else m.setView([30, 10], 2)
    m.on('click', (e: L.LeafletMouseEvent) => setPin({ lat: e.latlng.lat, lon: e.latlng.lng }))
    return () => { m.remove(); map.current = undefined }
  }, [saved === undefined])
  useEffect(() => {
    const g = pinLayer.current
    if (!g) return
    g.clearLayers()
    const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim()
    if (saved && !pin) {
      L.circle([saved.lat, saved.lon], { radius: 1000, color: accent, weight: 1, fillOpacity: 0.12 }).addTo(g)
      L.circleMarker([saved.lat, saved.lon], { radius: 7, color: '#fff', weight: 2, fillColor: accent, fillOpacity: 1 })
        .bindTooltip('Shown here', { direction: 'top', offset: [0, -8] }).addTo(g)
    }
    if (pin) L.marker([pin.lat, pin.lon], { icon: L.divIcon({ className: 'map-pin', iconSize: [16, 16] }) }).addTo(g)
  }, [saved, pin])

  const save = async () => {
    if (!pin) return
    setBusy(true)
    setError(undefined)
    try {
      const r = await api<{ location: Location }>('PUT', '/api/location', pin)
      setPin(undefined)
      setSaved(r.location)
      map.current?.setView([r.location.lat, r.location.lon], 10)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const remove = async () => {
    if (!confirm('Remove this site from the map? Other sites will no longer see where it is.')) return
    await api('DELETE', '/api/location').catch((e) => setError(e.message))
    setSaved(null)
    setPin(undefined)
  }
  const near = () => navigator.geolocation?.getCurrentPosition(
    (p) => map.current?.setView([p.coords.latitude, p.coords.longitude], 11),
    () => setError('This browser could not tell where you are; zoom in on the map instead.'))

  return (
    <Card>
      <div className="stack">
        <p className="muted small">
          Optional. Click the map roughly where this site is, then save. Every site in the IBS sees this spot, so pick a
          general area such as your town or neighbourhood, not your address. The IBS can then be shown on a map (IBS
          network → Map).
        </p>
        <div ref={el} className="geo-map picker" />
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={!pin || busy} onClick={save}>{busy ? 'Saving…' : 'Save location'}</button>
          {pin && <button className="btn" onClick={() => setPin(undefined)}>Cancel</button>}
          {!pin && 'geolocation' in navigator && <button className="btn" onClick={near}>Near me</button>}
          {saved && !pin && <button className="btn danger" onClick={remove}>Remove from the map</button>}
        </div>
        <p className="muted small">
          {pin ? 'Pin placed. Save to share it.' : saved ? 'The blue dot is what other sites see.' : 'No location set: this site is not on the map.'}
        </p>
      </div>
    </Card>
  )
}

function HTTPSCard() {
  const { data, reload } = useLoader(() => api<TLSStatus>('GET', '/api/tls'), [], 5000)
  const [editing, setEditing] = useState(false)
  const [error, setError] = useState<string>()
  if (!data) return null
  const on = data.domains.length > 0
  const overHTTPS = window.location.protocol === 'https:'
  const name = data.domains.find((d) => !d.startsWith('*.'))
  const httpsURL = name && `https://${name}${window.location.port ? ':' + window.location.port : ''}/#/settings`
  const days = data.not_after ? Math.floor((new Date(data.not_after).getTime() - Date.now()) / 86400000) : undefined
  const save = async (s: TLSSettings) => {
    setError(undefined)
    try {
      await api('PUT', '/api/tls', s)
      reload()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  return (
    <Card actions={!data.from_env && (
      <div className="row">
        {on && <button className="btn small danger" onClick={() => confirm('Turn off HTTPS? The web interface and S3 go back to plain HTTP.') && save({ domains: [], require_https: false })}>Turn off</button>}
        <button className={`btn small${on ? '' : ' primary'}`} onClick={() => setEditing(true)}>{on ? 'Change' : 'Set up HTTPS'}</button>
      </div>
    )}>
      {!on ? (
        <p className="muted small">
          Off: the web interface and S3 use plain HTTP. With HTTPS, this manager gets a free Let's Encrypt certificate
          and renews it by itself. No port needs to be open.
        </p>
      ) : (
        <div className="stack">
          <dl className="facts">
            <dt>Names</dt><dd className="mono">{data.domains.join(', ')}</dd>
            <dt>Certificate</dt>
            <dd>
              {data.active
                ? <>From {data.issuer}, valid until {new Date(data.not_after!).toLocaleDateString()} ({days} days). Renewed automatically.</>
                : data.error ? <span className="muted">Not issued yet.</span>
                : <span className="muted">Being requested; this usually takes a minute.</span>}
            </dd>
            <dt>DNS provider</dt><dd>{data.provider}</dd>
          </dl>
          {data.error && <ErrorBox error={`Last attempt ${ago(new Date(data.error_time!).getTime() / 1000)}: ${data.error}`} />}
          {data.active && !overHTTPS && httpsURL && (
            <p className="small">Open the web interface over HTTPS: <a href={httpsURL}>{httpsURL.replace('/#/settings', '')}</a></p>
          )}
          {data.active && (
            <label className="check">
              <input type="checkbox" checked={data.require_https} disabled={data.from_env || (!data.require_https && !overHTTPS)}
                onChange={(e) => save({ ...data, require_https: e.target.checked })} />
              <span>
                Require HTTPS
                <small className="muted">
                  {' '}Plain HTTP is redirected (web interface) or refused (S3).
                  {!overHTTPS && !data.require_https && ' Open this page over HTTPS first, so you know that address works.'}
                </small>
              </span>
            </label>
          )}
          {data.from_env && <p className="muted small">Set with IBS_TLS_* environment variables; change them there.</p>}
        </div>
      )}
      <ErrorBox error={error} />
      {editing && <HTTPSDialog status={data} onClose={() => setEditing(false)} onSaved={() => { setEditing(false); reload() }} />}
    </Card>
  )
}

function HTTPSDialog({ status, onClose, onSaved }: { status: TLSStatus; onClose: () => void; onSaved: () => void }) {
  const [draft, setDraft] = useState<HTTPSDraft>(status.domains.length > 0 ? draftFrom(status) : emptyDraft)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      await api('PUT', '/api/tls', settingsFrom(draft, status.require_https))
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal title="HTTPS" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <HTTPSFields value={draft} onChange={setDraft} secretsSet={draft.provider === status.provider ? status.secrets_set : []} />
        <ErrorBox error={error} />
        <div className="row">
          <button className="btn primary" disabled={busy || !draft.domains.trim() || !draft.provider}>{busy ? 'Saving…' : 'Save'}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  )
}

function PasswordCard() {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [msg, setMsg] = useState<string>()
  const [error, setError] = useState<string>()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setMsg(undefined)
    setError(undefined)
    try {
      await api('POST', '/api/password', { current, new: next })
      setCurrent('')
      setNext('')
      setMsg('Password changed')
    } catch (err) {
      setError((err as Error).message)
    }
  }
  return (
    <Card title="Admin password">
      <form className="stack narrow" onSubmit={submit}>
        <label>Current password<input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} /></label>
        <label>New password<input type="password" value={next} onChange={(e) => setNext(e.target.value)} /><PasswordHint value={next} /></label>
        <ErrorBox error={error} />
        {msg && <div className="success">{msg}</div>}
        <button className="btn" disabled={!current || next.length < MIN_PASSWORD}>Change password</button>
      </form>
    </Card>
  )
}
