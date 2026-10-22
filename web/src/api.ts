export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { 'X-IBS': '1' }
  let payload: BodyInit | undefined
  if (body instanceof FormData) {
    payload = body
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await fetch(path, { method, headers, body: payload, credentials: 'same-origin' })
  const text = await res.text()
  const data = text ? JSON.parse(text) : undefined
  if (!res.ok) {
    // HTTPS is required: move the web interface to its HTTPS address.
    if (res.status === 421 && data?.https_url) {
      window.location.href = new URL(window.location.pathname + window.location.hash, data.https_url).toString()
    }
    if (res.status === 401 && path !== '/api/login') window.dispatchEvent(new Event('ibs:logout'))
    throw new ApiError(res.status, data?.error ?? res.statusText)
  }
  return data as T
}

export type State = { setup: boolean; authenticated: boolean; site_name?: string; site_id?: string; s3_port?: string; version?: string; build?: string; started?: number }

export type NodeInfo = {
  id: string; name: string; address: string; quota: number; limit_from_env: boolean; disk_capacity: number; disk_free: number
  capacity: number; free: number
  last_seen: number; local: boolean; online: boolean; lost: boolean; stranded: number; counted: boolean; blocks: number; held_blocks: number; used: number
  metadata_generation: number; version: string
}

export type PathInfo = {
  kind: '' | 'quic' | 'direct' | 'relay'; relay?: string; relay_name?: string
  rtt_ms: number; bps: number; since?: number; direct_error?: string
}

export type PeerInfo = {
  site_id: string; name: string; address: string; added: number; online: boolean
  last_seen: number; up_since: number; capacity: number; free: number; last_error: string
  reconciled: number; counted: boolean; replicas_out: number; removing: number; bytes_out: number; held_blocks: number; held_bytes: number
  path: PathInfo; relays: number; version: string; out_bps: number; in_bps: number; restore_bps: number
}

export type RelayInfo = {
  id: string; name: string; address: string; connected: boolean; since?: number
  rtt_ms: number; public?: string; error?: string; added: number; version?: string
}

export type Link = {
  from: string; to: string; kind: '' | 'quic' | 'direct' | 'relay' | 'uplink'; relay?: string; relay_name?: string
  rtt_ms: number; bps: number; online: boolean; updated: number
}

export type RelayNode = { id: string; name: string }

export type Location = { lat: number; lon: number }

export type NetSite = {
  site_id: string; name: string; up_since: number; capacity: number; free: number
  online: boolean; last_seen: number; location?: Location; version?: string
}

export type Dashboard = {
  site: {
    site_id: string; name: string; up_since: number; capacity: number; free: number; offered: number
    generation: number; peer_advertise: string; web_host?: string; buckets: number; objects: number; object_bytes: number
    held_blocks: number; held_bytes: number; held_offline: number; restored: number; location?: Location
  }
  replication: {
    blocks: number; bytes: number; under_replicated: number; over_replicated: number
    pending_bytes: number; missing_locally: number; missing_bytes: number; pending_deletes: number; no_room: number; copies_made: number; copies_possible: number; copying: number; copying_bytes: number
  }
  ibs_capacity: IBSCapacity
  counted: boolean // the figures above are in; false right after a start
  restore_bps: number // blocks fetched back from other sites' copies, bytes/s
  // Cleanup of blocks: small files still to combine, bytes of deleted files still to free.
  cleanup: { combine_left: number; freeable: number; convert_left: number; active: boolean }
  activity: { writing: boolean; deleting: boolean; buckets: Record<string, 'writing' | 'deleting'> }
  nodes: NodeInfo[]
  peers: PeerInfo[]
  network: NetSite[]
  relays: RelayInfo[]
  links: Link[]
  relay_nodes: RelayNode[]
}

export type IBSCapacity = { sites: number; free: number; needed: number; copies: number; fits: boolean }

export type StorableCapacity = {
  local: number
  sites: number
  copies: number
  storable: number
  limited_by: 'site' | 'ibs' | 'sites'
}

export type Bucket = { name: string; created: string; replication: number; counted: boolean; activity?: 'writing' | 'deleting'; objects: number; size: number; pending: number }

export type ObjectInfo = { bucket: string; key: string; size: number; etag: string; content_type: string; modified: string }

export type AccessKey = {
  id: string; name: string; created: string; secret?: string
  access: 'full' | 'read'
  buckets: string[] | null // null: all buckets
}

export type Settings = { site_name: string; peer_advertise: string }

export type TLSSettings = {
  domains: string[]
  email?: string
  provider?: string
  env?: Record<string, string>
  ca?: string
  resolvers?: string[]
  propagation_wait?: string
  require_https: boolean
}

export type TLSStatus = TLSSettings & {
  from_env: boolean
  active: boolean
  issuer?: string
  not_after?: string
  error?: string
  error_time?: string
  secrets_set?: string[]
}

export type DNSField = { name: string; description: string; secret: boolean }
export type DNSProvider = { code: string; name: string; docs: string; credentials: DNSField[]; additional: DNSField[] }

export type VersionInfo = {
  version: string; key_id: string; key_name: string; released: number
  current: boolean; bad: boolean; newer: boolean; beta: boolean; blocker: string; sites: string[] | null
  notes: string // what changed, Markdown; empty when unknown
}
export type SeenVersion = { version: string; sites: string[]; trusted: boolean; error?: string }
export type ReleaseKey = { id: string; name: string; key: string; default: boolean; env: boolean; signed: string[] }
export type UpdatesInfo = {
  running: string; launcher: boolean; launcher_version?: string; github: boolean; betas: boolean; auto_install: boolean; check_minutes: number; github_checked?: number; github_error?: string; github_retry?: number
  versions: VersionInfo[]; seen: SeenVersion[]; keys: ReleaseKey[]; newer: string
  old: { remove: string[]; keep?: string; fallback?: string } // what Remove old versions does
}

export type Scrub = {
  id: number; started: number; finished?: number; state: 'running' | 'paused' | 'done' | 'stopped'; manual: boolean
  phase: 'database' | 'own' | 'copies' | 'held'; total: number; checked: number; bytes: number; repaired: number; failed: number
  copies: number; copy_bytes: number; unchecked: number
}
export type ScrubError = {
  at: number; what: 'block' | 'remote' | 'held' | 'database' | 'copy' | 'node'; block_id?: string; place?: string
  problem: string; outcome: 'repaired' | 'failed' | 'replaced' | 'skipped'; detail?: string; resolved?: boolean
}
export type ScrubInfo = {
  current: Scrub | null; last: Scrub | null; history: Scrub[]; errors: ScrubError[]
  interval_days: number; next_due?: number; unresolved: number
}
export type CopyCheck = { site_id: string; name: string; copies: number; scrubbed: number; checked: number; bad: number }
