import { FormEvent, ReactNode, useCallback, useEffect, useRef, useState } from 'react'
import { ALL_TOPICS, onStatus, subscribe, Topic } from './events'

export { ALL_TOPICS }

export function fmtBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1)
  const v = n / 1024 ** i
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

export function fmtDuration(seconds: number): string {
  if (seconds < 60) return `${Math.max(0, Math.floor(seconds))}s`
  const m = Math.floor(seconds / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h ${m % 60}m`
  return `${Math.floor(h / 24)}d ${h % 24}h`
}

export function ago(unix: number): string {
  if (!unix) return 'never'
  return `${fmtDuration(Date.now() / 1000 - unix)} ago`
}

export function uptime(upSince: number, online = true): string {
  if (!online || !upSince) return '—'
  return fmtDuration(Date.now() / 1000 - upSince)
}

export function fmtDate(iso: string): string {
  return new Date(iso).toLocaleString()
}

export function shortId(id: string) {
  return id.slice(0, 8)
}

/**
 * Load data and reload it whenever the server announces a change to one of
 * the given topics. Reloads never overlap: changes arriving during a load
 * trigger exactly one more load afterwards.
 */
export function useLoader<T>(load: () => Promise<T>, topics: Topic[] = [], refreshMs = 0) {
  const [data, setData] = useState<T | undefined>()
  const [error, setError] = useState<string>()
  const loadRef = useRef(load)
  loadRef.current = load
  const state = useRef({ running: false, again: false })
  const reload = useCallback(async () => {
    if (state.current.running) {
      state.current.again = true
      return
    }
    state.current.running = true
    try {
      do {
        state.current.again = false
        try {
          setData(await loadRef.current())
          setError(undefined)
        } catch (e) {
          setError((e as Error).message)
        }
      } while (state.current.again)
    } finally {
      state.current.running = false
    }
  }, [])
  const topicKey = topics.join(',')
  useEffect(() => {
    reload()
    if (!topicKey) return
    const wanted = new Set(topicKey.split(','))
    return subscribe((changed) => {
      if (changed.some((t) => wanted.has(t))) reload()
    })
  }, [reload, topicKey])
  useEffect(() => {
    if (!refreshMs) return
    const t = setInterval(reload, refreshMs)
    return () => clearInterval(t)
  }, [reload, refreshMs])
  return { data, error, reload }
}

export function fmtMs(ms: number): string {
  if (!ms) return '—'
  return ms < 10 ? `${ms.toFixed(1)} ms` : `${Math.round(ms)} ms`
}

export function fmtSpeed(bps: number): string {
  return bps > 0 ? `${fmtBytes(bps)}/s` : '—'
}

/** Re-render periodically so relative times and uptimes stay current. */
export function useTick(ms = 1000) {
  const [, setN] = useState(0)
  useEffect(() => {
    const t = setInterval(() => setN((n) => n + 1), ms)
    return () => clearInterval(t)
  }, [ms])
}

/** Whether the live update stream is connected. */
export function useLive() {
  const [live, setLive] = useState(false)
  useEffect(() => onStatus(setLive), [])
  return live
}

export function Card({ title, actions, children }: { title?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="card">
      {(title || actions) && (
        <header className="card-head">
          <h2>{title}</h2>
          <div className="row">{actions}</div>
        </header>
      )}
      {children}
    </section>
  )
}

export function Stat({ label, value, sub, tone }: { label: string; value: ReactNode; sub?: ReactNode; tone?: 'ok' | 'warn' | 'bad' }) {
  return (
    <div className={`stat ${tone ?? ''}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

/** Shown in place of a figure that's still being counted. */
export function Spinner({ title = 'Counting…' }: { title?: string }) {
  return <span className="spinner" title={title} aria-label={title} />
}

/** A figure, or a spinner while it's being counted. */
export function Counted({ ok, children }: { ok: boolean; children: ReactNode }) {
  return ok ? <>{children}</> : <Spinner />
}

export function Status({ online, label }: { online: boolean; label?: string }) {
  return <span className={`pill ${online ? 'ok' : 'bad'}`}>{label ?? (online ? 'online' : 'offline')}</span>
}

export function Meter({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0
  return (
    <div className="meter" title={`${pct.toFixed(1)}% used`}>
      <div className="meter-fill" style={{ width: `${pct}%` }} />
    </div>
  )
}

export function ErrorBox({ error }: { error?: string }) {
  if (!error) return null
  return <div className="error">{error}</div>
}

/** copyText puts text on the clipboard. Browsers only offer the clipboard API on
 *  HTTPS and localhost; a site opened over plain HTTP on the LAN uses the older
 *  copy command instead. Reports whether it worked. */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      /* fall back below */
    }
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '0'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  ta.remove()
  return ok
}

export function Copy({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [state, setState] = useState<'idle' | 'done' | 'failed'>('idle')
  return (
    <button
      type="button"
      className="btn small"
      title={state === 'failed' ? 'This browser blocked copying: select the text and copy it yourself' : undefined}
      onClick={async () => {
        const ok = await copyText(text)
        setState(ok ? 'done' : 'failed')
        setTimeout(() => setState('idle'), ok ? 1500 : 4000)
      }}
    >
      {state === 'done' ? 'Copied' : state === 'failed' ? 'Copy it by hand' : label}
    </button>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

/** A dialog over the page. Closes on Escape, on the backdrop or with the X. */
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal card" role="dialog" aria-modal="true" aria-label={title}>
        <header className="card-head">
          <h2>{title}</h2>
          <button className="btn small ghost" onClick={onClose} aria-label="Close">✕</button>
        </header>
        {children}
      </div>
    </div>
  )
}

type DialogRequest = {
  title: string
  message: ReactNode
  ok: string
  cancel: boolean
  danger: boolean
  resolve: (ok: boolean) => void
}
let showDialog: ((d: DialogRequest) => void) | undefined

/**
 * Asks the user to confirm, in the page's own style instead of the browser's.
 * Resolves true when they choose ok. Needs <Dialogs /> on the page.
 */
export function ask(title: string, message: ReactNode, { ok = 'OK', danger = false } = {}): Promise<boolean> {
  return new Promise((resolve) => {
    if (!showDialog) return resolve(window.confirm(typeof message === 'string' ? message : title))
    showDialog({ title, message, ok, cancel: true, danger, resolve })
  })
}

/** Tells the user something went wrong, with only a button to close it. */
export function tell(title: string, message: ReactNode): Promise<void> {
  return new Promise((resolve) => {
    if (!showDialog) return resolve(window.alert(typeof message === 'string' ? message : title))
    showDialog({ title, message, ok: 'Close', cancel: false, danger: false, resolve: () => resolve() })
  })
}

/** Shows the dialogs ask and tell open, one at a time. */
export function Dialogs() {
  const [queue, setQueue] = useState<DialogRequest[]>([])
  useEffect(() => {
    showDialog = (d) => setQueue((q) => [...q, d])
    return () => { showDialog = undefined }
  }, [])
  const d = queue[0]
  if (!d) return null
  const close = (ok: boolean) => {
    d.resolve(ok)
    setQueue((q) => q.slice(1))
  }
  return (
    <Modal title={d.title} onClose={() => close(false)}>
      <div className="stack">
        <div className="dialog-message">{d.message}</div>
        <div className="row">
          <button className={`btn ${d.danger ? 'danger solid' : 'primary'}`} autoFocus onClick={() => close(true)}>{d.ok}</button>
          {d.cancel && <button className="btn" onClick={() => close(false)}>Cancel</button>}
        </div>
      </div>
    </Modal>
  )
}

export const MIN_PASSWORD = 8

/** Under a new-password field: the length rule, as a warning while it isn't met. */
export function PasswordHint({ value }: { value: string }) {
  const short = value.length > 0 && value.length < MIN_PASSWORD
  return (
    <small className={short ? 'warn-text' : 'muted'}>
      At least {MIN_PASSWORD} characters{short ? ` (${MIN_PASSWORD - value.length} more)` : ''}
    </small>
  )
}

/** Next to a disabled button: what is still needed before it can be used. */
export function StillNeeded({ items }: { items: (string | false | undefined)[] }) {
  const left = items.filter(Boolean) as string[]
  if (left.length === 0) return null
  return <small className="muted">Still needed: {left.join(', ')}.</small>
}

/** "IBS 1.2.0" under a name; nothing when the other side didn't say (older versions). */
/** Version is a table cell with a version number; "—" when it isn't known (older versions don't report it). */
export function Version({ v }: { v?: string }) {
  return <td className="mono small">{v || <span className="muted" title="Not reported">—</span>}</td>
}

/** useSubmit runs a form action with busy and error state. */
export function useSubmit(action: () => Promise<void>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      await action()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return { busy, error, submit }
}
