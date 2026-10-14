import { useEffect, useRef, useState } from 'react'
import { api, State } from './api'
import { onStatus } from './events'

/**
 * Shows a bar when the manager now runs a different build of IBS than the one
 * this page was loaded from, e.g. after an upgrade. It checks when the live
 * connection comes back (the manager restarted) and once a minute. Reloading
 * is left to the user, so nothing they're doing is lost.
 */
export default function UpdateBanner({ build }: { build?: string }) {
  const loaded = useRef(build)
  const [update, setUpdate] = useState<{ version?: string }>()
  useEffect(() => {
    if (!loaded.current && build) loaded.current = build
  }, [build])

  useEffect(() => {
    let stopped = false
    const check = () =>
      api<State>('GET', '/api/state').then((s) => {
        if (!stopped && loaded.current && s.build && s.build !== loaded.current) setUpdate({ version: s.version })
      }, () => {})
    let wasLive = true
    const off = onStatus((live) => {
      if (live && !wasLive) check() // reconnected: the manager may have restarted with a new version
      wasLive = live
    })
    const timer = window.setInterval(check, 60_000)
    return () => { stopped = true; off(); window.clearInterval(timer) }
  }, [])

  if (!update) return null
  return (
    <div className="update-banner" role="status">
      <span>
        IBS has been updated{update.version && update.version !== 'dev' ? ` to ${update.version}` : ''}. Reload to use the new
        version.
      </span>
      <button className="btn small primary" onClick={() => window.location.reload()}>Reload</button>
    </div>
  )
}
