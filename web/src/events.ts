// One shared Server-Sent Events connection for the whole UI. The server
// announces which topics changed; loaders subscribed to those topics refetch.

export type Topic = 'data' | 'peers' | 'nodes' | 'replication' | 'scrub'
export const ALL_TOPICS: Topic[] = ['data', 'peers', 'nodes', 'replication', 'scrub']

type Listener = (topics: string[]) => void
type StatusListener = (live: boolean) => void

const listeners = new Set<Listener>()
const statusListeners = new Set<StatusListener>()
let source: EventSource | null = null
let live = false
let missedEvents = false

function setLive(v: boolean) {
  if (live === v) return
  live = v
  statusListeners.forEach((l) => l(v))
}

let retryTimer = 0
let retryDelay = 1000
let wanted = false // false after disconnect (signed out): don't reconnect

// connect opens the event stream. The browser retries a dropped stream by
// itself, but gives up for good when the server answers with an error, as
// it briefly does while IBS restarts; then we start a new one ourselves,
// waiting longer each time (up to 30 s), or at once when the tab comes back.
function connect() {
  if (source) return
  window.clearTimeout(retryTimer)
  const es = new EventSource('/api/events')
  source = es
  es.onopen = () => {
    retryDelay = 1000
    setLive(true)
    // Events may have been missed while disconnected: refresh everything.
    if (missedEvents) listeners.forEach((l) => l(ALL_TOPICS))
    missedEvents = false
  }
  es.onerror = () => {
    setLive(false)
    missedEvents = true
    if (es.readyState === EventSource.CLOSED && source === es) {
      source = null
      if (!wanted) return
      retryTimer = window.setTimeout(reconnect, retryDelay)
      retryDelay = Math.min(retryDelay * 2, 30000)
    }
  }
  source.addEventListener('change', (e) => {
    const topics = JSON.parse((e as MessageEvent).data) as string[]
    listeners.forEach((l) => l(topics))
  })
}

function reconnect() {
  if (wanted && listeners.size > 0 && !source) connect()
}
window.addEventListener('online', reconnect)
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') reconnect()
})

export function subscribe(fn: Listener): () => void {
  listeners.add(fn)
  wanted = true
  connect()
  return () => listeners.delete(fn)
}

export function onStatus(fn: StatusListener): () => void {
  statusListeners.add(fn)
  fn(live)
  return () => statusListeners.delete(fn)
}

export function disconnect() {
  wanted = false
  window.clearTimeout(retryTimer)
  source?.close()
  source = null
  setLive(false)
}
