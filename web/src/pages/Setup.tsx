import { FormEvent, useState } from 'react'
import { api } from '../api'
import { emptyDraft, HTTPSDraft, HTTPSFields, settingsFrom } from '../HTTPSForm'
import { Copy, ErrorBox, MIN_PASSWORD, PasswordHint, StillNeeded } from '../ui'
import { Logo } from '../Logo'

export default function Setup({ onDone }: { onDone: () => void }) {
  const [mode, setMode] = useState<'create' | 'restore'>('create')
  const [phrase, setPhrase] = useState<string>()

  if (phrase) return <ShowPhrase phrase={phrase} onDone={onDone} />
  return (
    <div className="center">
      <div className="card auth wide">
        <Logo size={44} />
        <h1>Set up this site</h1>
        <div className="tabs">
          <button className={mode === 'create' ? 'active' : ''} onClick={() => setMode('create')}>Create new site</button>
          <button className={mode === 'restore' ? 'active' : ''} onClick={() => setMode('restore')}>Restore lost site</button>
        </div>
        {mode === 'create' ? <CreateForm onCreated={setPhrase} /> : <RestoreForm onDone={onDone} />}
      </div>
    </div>
  )
}

function CreateForm({ onCreated }: { onCreated: (phrase: string) => void }) {
  const [name, setName] = useState('')
  const [address, setAddress] = useState('')
  const [pw, setPw] = useState('')
  const [pw2, setPw2] = useState('')
  const [https, setHTTPS] = useState<HTTPSDraft>(emptyDraft)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const missing = [!name.trim() && 'a site name', pw.length < MIN_PASSWORD && `a password of at least ${MIN_PASSWORD} characters`,
    pw.length >= MIN_PASSWORD && pw2 !== pw && 'the password repeated', !!https.domains.trim() && !https.provider && 'a DNS provider for HTTPS']
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (pw !== pw2) return setError('Passwords do not match')
    setBusy(true)
    try {
      const tls = https.domains.trim() ? settingsFrom(https) : undefined
      const res = await api<{ phrase: string }>('POST', '/api/setup/create', { site_name: name, password: pw, peer_advertise: address, tls })
      onCreated(res.phrase)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="stack">
      <p className="muted">This node becomes the manager of a new site. You can connect it to other sites afterwards.</p>
      <label>Site name<input value={name} onChange={(e) => setName(e.target.value)} placeholder="home-lab" autoFocus /></label>
      <label>
        Direct address <span className="muted">(optional)</span>
        <input value={address} onChange={(e) => setAddress(e.target.value)} placeholder="vpn-host.example:7443" />
        <small className="muted">Only if other sites can reach this site's port 7443, e.g. over a VPN. Otherwise add a relay later.</small>
      </label>
      <label>Admin password<input type="password" value={pw} onChange={(e) => setPw(e.target.value)} /><PasswordHint value={pw} /></label>
      <label>
        Repeat password<input type="password" value={pw2} onChange={(e) => setPw2(e.target.value)} />
        {pw2 && pw2 !== pw && <small className="warn-text">The passwords don't match</small>}
      </label>
      <details className="fold">
        <summary>HTTPS <span className="muted">(optional, can be set up later in Settings)</span></summary>
        <HTTPSFields value={https} onChange={setHTTPS} />
      </details>
      <ErrorBox error={error} />
      <div className="stack">
        <button className="btn primary" disabled={busy || missing.some(Boolean)}>Create site</button>
        <StillNeeded items={missing} />
      </div>
    </form>
  )
}

function RestoreForm({ onDone }: { onDone: () => void }) {
  const [phrase, setPhrase] = useState('')
  const [via, setVia] = useState<'node' | 'relay' | 'address'>('relay')
  const [nodeAddress, setNodeAddress] = useState('')
  const [peer, setPeer] = useState('')
  const [relayAddress, setRelayAddress] = useState('')
  const [address, setAddress] = useState('')
  const [pw, setPw] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const source = via === 'node' ? { node_address: nodeAddress }
        : via === 'relay' ? { relay_address: relayAddress }
        : { peer_address: peer }
      await api('POST', '/api/setup/restore', { phrase, password: pw, peer_advertise: address, ...source })
      onDone()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const sourceOK = via === 'node' ? !!nodeAddress : via === 'relay' ? !!relayAddress.trim() : !!peer
  return (
    <form onSubmit={submit} className="stack">
      <p className="muted">
        Restore a site whose manager was lost. Its metadata is recovered from one of its storage nodes or a former peer
        site; file blocks still on the site's nodes are used where they are, and missing ones are fetched back from
        the copies on other sites.
      </p>
      <label>
        Recovery phrase
        <textarea rows={3} value={phrase} onChange={(e) => setPhrase(e.target.value)} placeholder="XXXXX-XXXXX-…" className="mono" />
      </label>
      <div className="tabs">
        <button type="button" className={via === 'relay' ? 'active' : ''} onClick={() => setVia('relay')}>Through a relay</button>
        <button type="button" className={via === 'address' ? 'active' : ''} onClick={() => setVia('address')}>Peer by address</button>
        <button type="button" className={via === 'node' ? 'active' : ''} onClick={() => setVia('node')}>Storage node</button>
      </div>
      {via === 'node' && (
        <label>
          Address of one of this site's storage nodes
          <input className="mono" value={nodeAddress} onChange={(e) => setNodeAddress(e.target.value)} placeholder="node-host:7480" />
          <small className="muted">Every node keeps an encrypted copy of the site's metadata. Its blocks stay where they are.</small>
        </label>
      )}
      {via === 'relay' ? (
        <label>
          Address of a relay your other sites use
          <input className="mono" value={relayAddress} onChange={(e) => setRelayAddress(e.target.value)} placeholder="relay.example.com:7444" />
          <small className="muted">
            Listed under The Known Universe → Relays on your other sites. They vouch for this site on the relay, so no
            relay token is needed; at least one of them must be online.
          </small>
        </label>
      ) : via === 'address' && (
        <label>
          Address of a former peer
          <input value={peer} onChange={(e) => setPeer(e.target.value)} placeholder="other-site.example:7443" />
        </label>
      )}
      <label>
        This site's direct address <span className="muted">(optional)</span>
        <input value={address} onChange={(e) => setAddress(e.target.value)} placeholder="vpn-host.example:7443" />
      </label>
      <label>New admin password<input type="password" value={pw} onChange={(e) => setPw(e.target.value)} /><PasswordHint value={pw} /></label>
      <ErrorBox error={error} />
      <div className="stack">
        <button className="btn primary" disabled={busy || !phrase || !sourceOK || pw.length < MIN_PASSWORD}>{busy ? 'Restoring… this can take a minute' : 'Restore site'}</button>
        <StillNeeded items={[!phrase && 'the recovery phrase', !sourceOK && 'where to restore from',
          pw.length < MIN_PASSWORD && `a password of at least ${MIN_PASSWORD} characters`]} />
      </div>
    </form>
  )
}

function ShowPhrase({ phrase, onDone }: { phrase: string; onDone: () => void }) {
  const [confirmed, setConfirmed] = useState(false)
  return (
    <div className="center">
      <div className="card auth wide">
        <Logo size={44} />
        <h1>Your recovery phrase</h1>
        <p>
          This phrase is the only way to restore this site if its storage is lost. It derives the site's identity and the
          keys that encrypt your data. <strong>It is shown only once.</strong> Store it somewhere safe and offline.
        </p>
        <div className="phrase mono">{phrase}</div>
        <div className="row"><Copy text={phrase} label="Copy phrase" /></div>
        <label className="check">
          <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />
          I have stored the recovery phrase safely
        </label>
        <button className="btn primary" disabled={!confirmed} onClick={onDone}>Continue</button>
      </div>
    </div>
  )
}
