import { FormEvent, Fragment, useEffect, useRef, useState } from 'react'
import { api, ScrubInfo, State, UpdatesInfo } from './api'
import UpdateBanner from './UpdateBanner'
import { ErrorBox, useLive, useLoader } from './ui'
import { disconnect } from './events'
import Setup from './pages/Setup'
import { Logo } from './Logo'
import DashboardPage from './pages/Dashboard'
import BucketsPage from './pages/Buckets'
import NetworkPage from './pages/Network'
import NodesPage from './pages/Nodes'
import KeysPage from './pages/Keys'
import ScrubPage from './pages/Scrub'
import SettingsPage, { SETTINGS_SECTIONS, settingsSection } from './pages/Settings'

const pages = {
  dashboard: { label: 'Dashboard', component: DashboardPage },
  buckets: { label: 'Buckets', component: BucketsPage },
  network: { label: 'The Known Universe', component: NetworkPage },
  nodes: { label: 'Nodes', component: NodesPage },
  scrub: { label: 'Scrub', component: ScrubPage },
  keys: { label: 'Access keys', component: KeysPage },
  settings: { label: 'Settings', component: SettingsPage },
} as const
type PageId = keyof typeof pages

function currentPage(): PageId {
  const h = window.location.hash.replace(/^#\/?/, '').split('/')[0]
  return (h in pages ? h : 'dashboard') as PageId
}

export default function App() {
  const [state, setState] = useState<State>()
  const [error, setError] = useState<string>()

  const refresh = () =>
    api<State>('GET', '/api/state')
      .then(setState)
      .catch((e) => setError(e.message))

  useEffect(() => {
    refresh()
    const onLogout = () => {
      disconnect()
      setState((s) => (s ? { ...s, authenticated: false } : s))
    }
    window.addEventListener('ibs:logout', onLogout)
    window.addEventListener('ibs:renamed', refresh)
    return () => {
      window.removeEventListener('ibs:logout', onLogout)
      window.removeEventListener('ibs:renamed', refresh)
    }
  }, [])

  // The tab shows which site this is: "roland-001 - IBS".
  useEffect(() => {
    document.title = state?.site_name ? `${state.site_name} - IBS` : 'IBS'
  }, [state?.site_name])

  if (!state) return <div className="center"><ErrorBox error={error} />{!error && <p className="muted">Loading…</p>}</div>
  return (
    <>
      {!state.setup ? <Setup onDone={refresh} />
        : !state.authenticated ? <Login siteName={state.site_name} onDone={refresh} />
        : <Shell state={state} onLogout={refresh} />}
      <UpdateBanner build={state.build} />
    </>
  )
}

function Login({ siteName, onDone }: { siteName?: string; onDone: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      await api('POST', '/api/login', { password })
      onDone()
    } catch (err) {
      setError((err as Error).message)
      // Select the wrong password, so typing replaces it.
      input.current?.focus()
      input.current?.select()
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="center">
      <form className="card auth" onSubmit={submit}>
        <Logo size={44} />
        <h1>{siteName ?? 'IBS site'}</h1>
        <p className="muted">Sign in to manage this site.</p>
        <label>
          Admin password
          <input ref={input} type="password" autoFocus value={password} onChange={(e) => setPassword(e.target.value)} />
        </label>
        <ErrorBox error={error} />
        <button className="btn primary" disabled={busy || !password}>Sign in</button>
      </form>
    </div>
  )
}

function Shell({ state, onLogout }: { state: State; onLogout: () => void }) {
  const [page, setPage] = useState<PageId>(currentPage)
  const [, setHash] = useState(window.location.hash) // re-render for submenu changes
  const [menuOpen, setMenuOpen] = useState(false)
  // In the phone menu, Settings unfolds its sections instead of opening one.
  const [unfolded, setUnfolded] = useState(false)
  useEffect(() => {
    const onHash = () => { setPage(currentPage()); setHash(window.location.hash) }
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])
  const Page = pages[page].component
  const live = useLive()
  const updates = useLoader(() => api<UpdatesInfo>('GET', '/api/updates'), ['peers'], 60000)
  const scrub = useLoader(() => api<ScrubInfo>('GET', '/api/scrub'), ['scrub'], 60000)
  const logout = async () => {
    await api('POST', '/api/logout').catch(() => {})
    disconnect()
    onLogout()
  }
  return (
    <div className="shell">
      <aside className={`sidebar ${menuOpen ? 'open' : ''}`}>
        <div className="sidebar-top">
          <Logo size={48} stacked />
          <button className="btn small menu-toggle" onClick={() => { setMenuOpen(!menuOpen); setUnfolded(false) }}>Menu</button>
        </div>
        <div className="site-name" title={state.site_id}><small>Site</small>{state.site_name}</div>
        <nav>
          {(Object.keys(pages) as PageId[]).map((id) => (
            <Fragment key={id}>
              <a href={`#/${id}`} className={id === page ? 'active' : ''} aria-expanded={id === 'settings' && menuOpen ? unfolded || page === 'settings' : undefined}
                onClick={(e) => {
                  if (id === 'settings' && menuOpen) {
                    e.preventDefault() // the menu is open on a phone: show the sections to choose from
                    setUnfolded(!unfolded)
                    return
                  }
                  setMenuOpen(false)
                }}>
                {pages[id].label}
                {id === 'settings' && updates.data?.newer && <span className="attention" title={`IBS ${updates.data.newer} is available`} />}
                {id === 'scrub' && (scrub.data?.unresolved ?? 0) > 0 && <span className="attention bad" title="The scrub found problems it couldn't repair" />}
                {id === 'settings' && menuOpen && <span className="unfold" aria-hidden>{unfolded || page === 'settings' ? '▴' : '▾'}</span>}
              </a>
              {id === 'settings' && (page === 'settings' || (menuOpen && unfolded)) && (
                <div className="subnav">
                  {SETTINGS_SECTIONS.map((s) => (
                    <a key={s.id} href={`#/settings/${s.id}`} className={page === 'settings' && s.id === settingsSection() ? 'active' : ''}
                      onClick={() => { setMenuOpen(false); setUnfolded(false) }}>
                      {s.label}
                      {s.id === 'updates' && updates.data?.newer && <span className="attention" />}
                    </a>
                  ))}
                </div>
              )}
            </Fragment>
          ))}
        </nav>
        <div className="live" title={live ? 'Receiving live updates' : 'Reconnecting to live updates…'}>
          <span className={`dot ${live ? 'on' : ''}`} />
          {live ? 'Live' : 'Reconnecting…'}
        </div>
        <button className="btn ghost logout" onClick={logout}>Sign out</button>
        {state.version && <div className="version">IBS {state.version}</div>}
      </aside>
      <main className="content">
        <Page />
      </main>
    </div>
  )
}
