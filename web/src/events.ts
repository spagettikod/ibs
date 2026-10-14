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

function connect() {
  if (source) return
  source = new EventSource('/api/events')
  source.onopen = () => {
    setLive(true)
    // Events may have been missed while disconnected: refresh everything.
    if (missedEvents) listeners.forEach((l) => l(ALL_TOPICS))
    missedEvents = false
  }
  source.onerror = () => {
    setLive(false)
    missedEvents = true
  }
  source.addEventListener('change', (e) => {
    const topics = JSON.parse((e as MessageEvent).data) as string[]
    listeners.forEach((l) => l(topics))
  })
}

export function subscribe(fn: Listener): () => void {
  listeners.add(fn)
  connect()
  return () => listeners.delete(fn)
}

export function onStatus(fn: StatusListener): () => void {
  statusListeners.add(fn)
  fn(live)
  return () => statusListeners.delete(fn)
}

export function disconnect() {
  source?.close()
  source = null
  setLive(false)
}
