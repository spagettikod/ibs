import { useEffect, useState } from 'react'
import { api, DNSField, DNSProvider, TLSSettings } from './api'

/** HTTPS settings being edited: lists as text, as typed. */
export type HTTPSDraft = {
  domains: string
  email: string
  provider: string
  env: Record<string, string>
  ca: string
  resolvers: string
  propagation_wait: string
}

export const emptyDraft: HTTPSDraft = { domains: '', email: '', provider: '', env: {}, ca: '', resolvers: '', propagation_wait: '' }

export function draftFrom(s: TLSSettings): HTTPSDraft {
  return {
    domains: s.domains.join(', '), email: s.email ?? '', provider: s.provider ?? '', env: { ...(s.env ?? {}) },
    ca: s.ca ?? '', resolvers: (s.resolvers ?? []).join(', '), propagation_wait: s.propagation_wait ?? '',
  }
}

const list = (v: string) => v.split(/[\s,]+/).filter(Boolean)

export function settingsFrom(d: HTTPSDraft, require = false): TLSSettings {
  return {
    domains: list(d.domains), email: d.email, provider: d.provider, env: d.env, ca: d.ca,
    resolvers: list(d.resolvers), propagation_wait: d.propagation_wait, require_https: require,
  }
}

let providersCache: Promise<DNSProvider[]> | undefined
function useProviders() {
  const [providers, setProviders] = useState<DNSProvider[]>([])
  useEffect(() => {
    providersCache ??= api<DNSProvider[]>('GET', '/api/tls/providers')
      .then((ps) => ps.sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })))
    providersCache.then(setProviders, () => (providersCache = undefined))
  }, [])
  return providers
}

const caChoice = (ca: string) => (ca === '' || ca === 'staging' ? ca : 'custom')

/** The HTTPS form fields, shared by Settings and the setup wizard. */
export function HTTPSFields({ value, onChange, secretsSet = [] }: { value: HTTPSDraft; onChange: (d: HTTPSDraft) => void; secretsSet?: string[] }) {
  const providers = useProviders()
  const provider = providers.find((p) => p.code === value.provider)
  const set = (patch: Partial<HTTPSDraft>) => onChange({ ...value, ...patch })
  const setEnv = (name: string, v: string) => set({ env: { ...value.env, [name]: v } })
  const field = (f: DNSField) => (
    <label key={f.name}>
      <span className="mono small">{f.name}</span>
      <input className="mono" type={f.secret ? 'password' : 'text'} autoComplete="off" value={value.env[f.name] ?? ''}
        placeholder={f.secret && secretsSet.includes(f.name) ? 'Saved; leave empty to keep it' : ''}
        onChange={(e) => setEnv(f.name, e.target.value)} />
      <small className="muted">{plain(f.description)}</small>
    </label>
  )
  return (
    <div className="stack">
      <label>
        Names
        <input value={value.domains} placeholder="ibs.example.com" onChange={(e) => set({ domains: e.target.value })} />
        <small className="muted">
          One or more, comma-separated. They must be in a DNS zone you manage, and should point at this manager in your
          local DNS. No port needs to be open: the certificate is verified with a DNS record.
        </small>
      </label>
      <label>
        DNS provider
        <select value={value.provider} onChange={(e) => set({ provider: e.target.value, env: {} })}>
          <option value="">Choose…</option>
          {providers.map((p) => <option key={p.code} value={p.code}>{p.name}</option>)}
        </select>
        <small className="muted">Where the zone's DNS records are managed. IBS creates a temporary record there for each certificate.</small>
      </label>
      {provider && (
        <>
          <p className="muted small">
            Fill in what your account uses; usually one API token is enough.{' '}
            <a href={provider.docs} target="_blank" rel="noreferrer">{provider.name} settings explained</a>
          </p>
          {provider.credentials.map(field)}
          {provider.additional.length > 0 && (
            <details className="fold">
              <summary>More {provider.name} settings</summary>
              <div className="stack">{provider.additional.map(field)}</div>
            </details>
          )}
        </>
      )}
      <details className="fold">
        <summary>Advanced</summary>
        <div className="stack">
          <label>
            Email <span className="muted">(optional)</span>
            <input type="email" value={value.email} onChange={(e) => set({ email: e.target.value })} />
            <small className="muted">Contact address for the Let's Encrypt account.</small>
          </label>
          <label>
            Certificate authority
            <select value={caChoice(value.ca)} onChange={(e) => set({ ca: e.target.value === 'custom' ? 'https://' : e.target.value })}>
              <option value="">Let's Encrypt</option>
              <option value="staging">Let's Encrypt staging (for testing; browsers don't trust it)</option>
              <option value="custom">Other ACME server…</option>
            </select>
            {caChoice(value.ca) === 'custom' && (
              <input className="mono" value={value.ca} placeholder="https://acme.example.com/directory" onChange={(e) => set({ ca: e.target.value })} />
            )}
          </label>
          <label>
            DNS servers to check with <span className="muted">(optional)</span>
            <input className="mono" value={value.resolvers} placeholder="1.1.1.1:53" onChange={(e) => set({ resolvers: e.target.value })} />
            <small className="muted">If your local DNS answers your own domain itself, use a public DNS server here.</small>
          </label>
          <label>
            Wait instead of checking <span className="muted">(optional)</span>
            <input className="mono" value={value.propagation_wait} placeholder="2m" onChange={(e) => set({ propagation_wait: e.target.value })} />
            <small className="muted">If outgoing DNS is blocked, wait this long after creating the record instead of checking it.</small>
          </label>
        </div>
      </details>
    </div>
  )
}

// plain drops Markdown code marks and version notes from lego's descriptions.
const plain = (s: string) => s.replace(/`/g, '').replace(/\s*\(since v[\d.]+\)/g, '')
