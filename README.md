<p align="center">
  <img src="docs/logo/ibs-patch.svg" width="200" alt="IBS logo: a round mission patch with a planet under the Milky Way and the name Intergalactic Backup System around the rim">
</p>

# IBS — Intergalactic Backup System

IBS is S3 storage that works like RAID, with sites instead of disks: every file is kept
three times, encrypted, at your site and on two other sites, and a damaged copy is
repaired from another. You run a **site** yourself, on your own hardware or on a VPS,
and it connects to sites run by friends, family or your other locations. Together they
form "the IBS". None of the sites need open ports.

Use it for backups and archives you write once and rarely read, with any backup tool
that speaks S3. Whenever keeping data safe and fast access conflict, IBS chooses
safety; it's not live storage for applications, websites or databases. Like RAID, it
isn't a backup by itself: a deleted or overwritten file is gone everywhere within
seconds. Versions and deduplication are your backup tool's job.

- **Manager:** each site has one. It serves S3 and a web interface, and keeps the site's
  metadata.
- **Storage nodes:** optional extra machines that add disk space to a site.
- **Relay:** a small service with one open port, for example on a VPS. Sites reach each
  other through it. Any site in the IBS can run one, and one relay can serve them all.

## Contents

- [Setup](#setup)
- [More setup](#more-setup)
- [Configuration](#configuration)
- [How your data is protected](#how-your-data-is-protected)
- [When something goes wrong](#when-something-goes-wrong)
- [S3 compatibility](#s3-compatibility)
- [FAQ](#faq)
- [Limitations](#limitations)
- [License](#license)

Building, testing and releasing IBS: see [DEVELOPMENT.md](DEVELOPMENT.md).

## Setup

IBS runs in Docker. Choose your case; the ports each machine needs are under
[Ports](#ports).

<details><summary><b>I have an invite</b></summary>

1. **Start your site:**

   With docker run:

   ```bash
   docker run -d \
     --name ibs \
     --restart unless-stopped \
     -v /srv/ibs:/data \
     -p 8080:8080 \
     -p 9000:9000 \
     -p 7443:7443/tcp \
     -p 7443:7443/udp \
     ghcr.io/spagettikod/ibs/launcher:1.1.0
   ```

   Or with Docker Compose:

   ```yaml
   services:
     ibs:
       image: ghcr.io/spagettikod/ibs/launcher:1.1.0
       container_name: ibs
       restart: unless-stopped
       ports:
         - 8080:8080
         - 9000:9000
         - 7443:7443/tcp
         - 7443:7443/udp
       volumes:
         - /srv/ibs:/data
   ```

2. Open `http://<host>:8080`, choose **Create new site**, and pick a name and an admin
   password. **Write down the recovery phrase.** It's shown only once.

3. **The Known Universe → Join a site:** enter the invite code, like
   `relay.example.com 7-comet-harbor-violet` (or paste a long invite). Your site connects to every site in
   the IBS, through their relays. That's it.

</details>

<details><summary><b>I'm starting a new IBS</b></summary>

1. **Run a relay** on a machine with a public address (skip it if your sites can
   [reach each other directly](#direct-connections)):

   With docker run:

   ```bash
   docker run -d \
     --name ibs-relay \
     --restart unless-stopped \
     -v /srv/ibs-relay:/data \
     -p 7444:7444/tcp \
     -p 7444:7444/udp \
     ghcr.io/spagettikod/ibs/relay:1.4.0
   ```

   Or with Docker Compose:

   ```yaml
   services:
     ibs-relay:
       image: ghcr.io/spagettikod/ibs/relay:1.4.0
       container_name: ibs-relay
       restart: unless-stopped
       ports:
         - 7444:7444/tcp
         - 7444:7444/udp
       volumes:
         - /srv/ibs-relay:/data
   ```

   Print its token:

   ```bash
   docker exec ibs-relay ibs-relay token
   ```

2. **Start your site:**

   With docker run:

   ```bash
   docker run -d \
     --name ibs \
     --restart unless-stopped \
     -v /srv/ibs:/data \
     -p 8080:8080 \
     -p 9000:9000 \
     -p 7443:7443/tcp \
     -p 7443:7443/udp \
     ghcr.io/spagettikod/ibs/launcher:1.1.0
   ```

   Or with Docker Compose:

   ```yaml
   services:
     ibs:
       image: ghcr.io/spagettikod/ibs/launcher:1.1.0
       container_name: ibs
       restart: unless-stopped
       ports:
         - 8080:8080
         - 9000:9000
         - 7443:7443/tcp
         - 7443:7443/udp
       volumes:
         - /srv/ibs:/data
   ```

3. Open `http://<host>:8080`, choose **Create new site**, and pick a name and an admin
   password. **Write down the recovery phrase.** It's shown only once.

4. **The Known Universe → Add relay:** enter the relay's address (e.g.
   `relay.example.com:7444`) and token.

5. **The Known Universe → Invite a site** for each site that should join, and send the code to
   its admin.

</details>

### Store your backups

1. **Buckets → Create bucket:** give it a name. The form shows roughly how much more
   fits right now.
2. **Access keys → Create:** copy the key ID and secret.
3. Use the key with any S3 client, for example the AWS CLI:

   ```bash
   export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… AWS_DEFAULT_REGION=us-east-1
   aws --endpoint-url http://<host>:9000 s3 sync ~/Pictures s3://photos/
   ```

### HTTPS

The web interface and S3 use plain HTTP. For HTTPS, put a TLS proxy in front of the
manager, or let IBS get a Let's Encrypt certificate itself under **Settings → HTTPS**.
That needs a domain at a DNS provider with an API (most large ones work; the page lists
them). No port has to be open. It can also be set with the `IBS_TLS_*` variables under
[Configuration](#configuration).

### Updates

New versions show up in the web interface, under **Settings → Updates**. **Install**
restarts the site with the new version, and its storage nodes follow. You can roll back
to an older version that still works with your data and the other sites, and a version
that fails to start is undone automatically. Tick **Install new versions automatically**
to have new versions installed as soon as they're downloaded.

Only stable releases are offered unless you tick **Include betas**. Versions must be
signed: by the IBS release key, or by a key you add under **Release keys** to run builds
of your own (see [DEVELOPMENT.md](DEVELOPMENT.md)).

Relays are updated by pulling a new `ibs/relay` image.

## More setup

Optional, when you need it.

### Add storage nodes

Extra machines that add disk space to your site. **Nodes → Add a node** shows the
command with your manager's address and join token. Run it on each machine:

With docker run:

```bash
docker run -d \
  --name ibs-node \
  --restart unless-stopped \
  -v /srv/ibs-node:/data \
  -p 7480:7480 \
  -e IBS_MANAGER=<manager-host>:7443 \
  ghcr.io/spagettikod/ibs/launcher:1.1.0 \
  node <token>
```

Or with Docker Compose:

```yaml
services:
  ibs-node:
    image: ghcr.io/spagettikod/ibs/launcher:1.1.0
    container_name: ibs-node
    command: ["node"]
    restart: unless-stopped
    ports:
      - 7480:7480
    volumes:
      - /srv/ibs-node:/data
    environment:
      IBS_MANAGER: <manager-host>:7443
      IBS_JOIN_TOKEN: <token>
```

How much of each node's disk IBS may use is set under **Nodes → Change**, or with
`IBS_STORAGE_LIMIT` on the node. A node gets IBS, and every update, from its manager.

### Direct connections

Sites that can reach each other's port 7443 (TCP and UDP) connect directly instead of
through a relay: on the same LAN, over a VPN such as WireGuard or Tailscale, or through
a port forward. **The Known Universe** shows which way each site is connected.

How sites find each other's addresses:

- **Through a shared relay.** Each site tells the relay its own addresses, and the
  relay passes them on. Inside Docker a site only knows its container's address, so
  this rarely works between two Docker hosts on the same LAN.
- **A guess.** A site also offers the name you open its web interface with, plus port
  7443: a site opened at `http://192.168.1.20:8080` is tried at `192.168.1.20:7443`. It's
  a guess; behind a reverse proxy or a VPN name it may be wrong, which only costs a
  failed attempt. **The Known Universe → Direct address** shows the current guess.
- **A direct address you set.** The reliable way: on each site, **The Known Universe → Direct
  address → Edit**, e.g. `192.168.1.20:7443` or the site's VPN address (or set
  `IBS_PEER_ADVERTISE`). Every site in the IBS learns it, and invites carry it.

Without any relay, set a direct address on **each** site and pair them as usual with
**Invite a site** and **Join a site**.

## Configuration

Environment variables. All are optional.

### Manager variables

| Variable | Default | Description |
|---|---|---|
| `IBS_ADMIN_PASSWORD` | | With `IBS_SITE_NAME`: create the site on first start |
| `IBS_BETA` | `false` | `true`: first download may be a beta; betas offered as updates until changed in **Settings → Updates** |
| `IBS_BINARY` | | Local `.ibsrelease` file to run instead of downloading from GitHub |
| `IBS_CHECK_INTERVAL` | `5s` | Health check and replication interval |
| `IBS_DATA` | `/data` | Data directory |
| `IBS_JOIN_TOKEN_OUT` | | Write the node join token to this file |
| `IBS_LOG_LEVEL` | `info` | `debug` logs every S3 request |
| `IBS_NODE_JOIN_ADDRESS` | page address | Manager address shown in the node command |
| `IBS_PEER_ADDR` | `:7443` | TCP address for nodes and sites |
| `IBS_PEER_ADVERTISE` | | Direct address other sites can use (LAN/VPN) |
| `IBS_NODE_GRACE` | `30m` | Offline time before a storage node counts as lost and its blocks are restored |
| `IBS_PEER_GRACE` | `2m` | Offline time before the copies on a site are re-created on other sites |
| `IBS_PROBATION` | `2m` | How long a new version must run before it counts as working |
| `IBS_QUIC_ADDR` | `:7443` | UDP address for direct connections |
| `IBS_RELAY_ADDRESS` | | Relay to register with on start |
| `IBS_RELAY_TOKEN` | | Token of that relay |
| `IBS_REBUILD_RATE` | `20M` | How fast copies of an offline site are re-created, per second |
| `IBS_RELAY_TOKEN_FILE` | | Read the relay token from a file |
| `IBS_ROOM_COOLDOWN` | `1h` | After pushing other sites' copies away for its own files, how long a site takes no new copies |
| `IBS_RELEASES_URL` | GitHub | Where to look for new releases |
| `IBS_S3_ADDR` | `:9000` | S3 address |
| `IBS_S3_DOMAIN` | | Enables `bucket.domain` S3 addresses |
| `IBS_SCRUB_RATE` | `100M` | How fast a scrub may read, per second, also when other sites' scrubs check their copies here |
| `IBS_SITE_NAME` | | With `IBS_ADMIN_PASSWORD`: create the site on first start (phrase in the log) |
| `IBS_STORAGE_LIMIT` | whole disk | Disk space for IBS, e.g. `500G`, `2T`; `0` = none. Overrides **Nodes → Change** |
| `IBS_TLS_CA` | Let's Encrypt | ACME directory URL; `staging` = Let's Encrypt's test CA |
| `IBS_TLS_DNS_PROPAGATION_WAIT` | | Wait this long (e.g. `2m`) instead of checking the DNS record |
| `IBS_TLS_DNS_PROVIDER` | | DNS provider code for HTTPS, see [HTTPS](#https) |
| `IBS_TLS_DNS_RESOLVERS` | system | DNS servers to check the record with, e.g. `1.1.1.1:53` |
| `IBS_TLS_DOMAINS` | | Names for the HTTPS certificate, comma-separated; sets up HTTPS instead of the web interface |
| `IBS_TLS_EMAIL` | | Contact address for the certificate account |
| `IBS_TLS_REQUIRE` | `true` | With `IBS_TLS_DOMAINS`: refuse plain HTTP once the certificate is in place |
| `IBS_TRUSTED_KEYS` | | Extra release keys to trust (`ibspub1:…`), comma-separated |
| `IBS_WEB_ADDR` | `:8080` | Web interface address |

### Storage node variables

| Variable | Default | Description |
|---|---|---|
| `IBS_DATA` | `/data` | Data directory |
| `IBS_JOIN_TOKEN` | | Join token, instead of `node <token>` |
| `IBS_JOIN_TOKEN_FILE` | | Read the join token from a file |
| `IBS_LOG_LEVEL` | `info` | Log level |
| `IBS_MANAGER` | | Manager address, `host:7443` |
| `IBS_MODE` | | `node`, instead of the `node` argument |
| `IBS_NODE_ADDR` | `:7480` | Listen address |
| `IBS_NODE_ADVERTISE` | connection IP | Address the manager should use (behind NAT) |
| `IBS_NODE_NAME` | hostname | Display name |
| `IBS_STORAGE_LIMIT` | whole disk | Disk space for IBS, e.g. `500G`, `2T`. Overrides **Nodes → Change** |

### Relay variables

| Variable | Default | Description |
|---|---|---|
| `IBS_DATA` | `/data` | Data directory |
| `IBS_RELAY_ADDR` | `:7444` | Listen address, TCP and UDP |
| `IBS_RELAY_ADVERTISE` | | Address to show in the log |
| `IBS_RELAY_SECRET` | generated | Fixed registration secret (disables `ibs-relay new-token`) |
| `IBS_RELAY_TOKEN_OUT` | | Also write the token to this file |

### Ports

| Machine | Port | Open to |
|---|---|---|
| Relay | 7444 TCP and UDP | The internet: the only port to forward |
| Manager | 8080 TCP (web interface) | You, on your LAN |
| | 9000 TCP (S3) | Your S3 clients, on your LAN |
| | 7443 TCP and UDP | Your storage nodes, on your LAN. Other sites only when you use a [direct address](#direct-connections); otherwise don't forward it on your router. |
| Storage node | 7480 TCP | Its manager, on your LAN |

## How your data is protected

- **Encrypted before it's stored.** Files are split into 4 MiB blocks, and each block is
  encrypted (AES-256-GCM) with its bucket's key. That happens before anything is
  written, locally or elsewhere. Nodes, other sites and relays only ever see encrypted
  blocks. They can check that a block is intact, but they can't read it.
- **One recovery phrase.** Creating a site shows a recovery phrase once. All of the
  site's keys come from it. With the phrase you can rebuild a lost site; without it,
  nobody can, including you. Keep it offline.
- **Every file exists three times.** One copy at your site and one on each of two other
  sites, never two on the same site. Each file has its own blocks; IBS doesn't share
  blocks between files. An upload is confirmed once it's stored at your
  site, and the copies follow within seconds. The web interface shows what's still being
  copied. With fewer than two other sites online, files get as many copies as there are
  sites, and the rest follow when more sites are available.
- **Damage is repaired.** Every block is checked against its checksum whenever it's
  read, and once a month a scrub checks all of the site's files: here, and their copies
  on other sites, which must prove they hold the exact same bytes (the **Scrub** page
  shows it, and you can start one by hand). A damaged or missing block is replaced from
  another copy. If no intact copy is left, the Scrub page names the affected
  files and reading them fails, instead of returning damaged data.
- **When storage runs out.** Other sites' copies may use any free space on your site,
  but your own files come first: when an upload needs room, your site pushes other
  sites' copies away. Their owners place them on another site, or, if no site has room,
  keep each file at two copies (the original and one elsewhere) until there's room
  again. Their dashboard then says the IBS is running out of space. Copies of an offline
  site are never pushed away. If nothing can make room, the upload fails with "storage
  is full". So when the IBS fills up, files lose their third copy before anyone's
  upload fails: add storage or a site in time.
- **The site's index is backed up too.** The manager's database knows which files
  exist, which blocks they consist of, and where those blocks are. An encrypted copy of
  it goes to every storage node and every connected site, within about 15 seconds of
  each change.
- **Connections are private.** Sites, nodes and the manager talk over mutual TLS, and
  each side checks the other's key. Sites connect directly when they can. Otherwise
  their traffic goes through a relay, which can't read it.
- **Only invited sites get in.** A site joins the IBS with an invite from a site
  already in it; the others then accept it because a site they trust vouches for it.
  So anyone running a site in your IBS can bring in new sites. Sites can't throw each
  other out; a site can only leave by itself. An invite code works once and for a day;
  a wrong guess makes it stop working, and the relay that passes it on can't read or
  use it.

## When something goes wrong

Click a situation to expand it. The commands assume the container names from
[Setup](#setup).

### Storage nodes

<details><summary><b>A storage node is offline or its disk failed</b></summary>

Nothing to do right away. Reads of its blocks are served from other sites. After 30
minutes (`IBS_NODE_GRACE`), its blocks are copied from other sites onto your other
nodes. Blocks not copied to other sites yet exist only on that node: the **Nodes**
page shows how many.

- **The node comes back:** it rejoins on its own, and outdated copies on it are removed.
- **It's gone for good:** remove it on the **Nodes** page.
- **To replace it:** start a new node with the command from **Nodes → Add a node**.

</details>

<details><summary><b>Retire a storage node</b></summary>

1. **Nodes → Change** next to it, and choose **Nothing** (or restart it with
   `-e IBS_STORAGE_LIMIT=0`). Its blocks move to your other nodes.
2. When the **Nodes** page shows 0 blocks for it, remove it there and stop the
   container.

</details>

<details><summary><b>Uploads fail with StorageFull</b></summary>

No node has room within its storage limit. Add a node, or raise a node's limit under
**Nodes → Change**. The dashboard shows how much of the space other sites' copies use.

</details>

### Manager

<details><summary><b>The manager is down for a while</b></summary>

S3 and the web interface are unavailable, but no data is lost. Nodes and other sites
keep their blocks, and everything resumes when the manager starts again. If it's down
longer than 30 minutes, other sites temporarily store your copies of their data
elsewhere. The extra copies are cleaned up once you're back.

</details>

<details><summary><b>The manager's disk is lost, but the nodes are fine</b></summary>

1. Start an empty manager (step 2 of Setup) on a new disk.
2. Choose **Restore lost site → Storage node**. Enter the recovery phrase, a node's
   address (`node-host:7480`) and a new admin password.
3. If the manager's address changed, restart the nodes with the new `IBS_MANAGER`.

The site's index comes back from the node, and blocks on the nodes are used where they
are. Blocks that were on the manager itself are fetched from other sites.

</details>

<details><summary><b>The manager moved to a new address</b></summary>

- **Storage nodes:** restart them with the new `IBS_MANAGER`.
- **Other sites:** reach it through the relay again on their own.
- **Direct address:** if you set one, update it under **The Known Universe → Direct address**.

</details>

<details><summary><b>Move the manager to another machine</b></summary>

Stop the container and copy `/srv/ibs` to the new machine. Start it there, then update
`IBS_MANAGER` on the nodes if the address changed.

</details>

<details><summary><b>Forgot the recovery phrase (the manager still runs)</b></summary>

```bash
docker exec -u ibs ibs ibs recovery-phrase
```

</details>

<details><summary><b>Can't reach the web interface after requiring HTTPS</b></summary>

For example because the name no longer points at the manager. Turn HTTPS off and
restart; then set it up again under **Settings → HTTPS**:

```bash
docker exec -u ibs ibs ibs https-off
docker restart ibs
```

</details>

<details><summary><b>Forgot the admin password</b></summary>

```bash
docker exec -u ibs ibs ibs reset-password 'new-password'
```

</details>

### Relay

<details><summary><b>The relay is down or gone</b></summary>

Direct connections keep working. Connections through the relay stop, and sites keep
retrying it. To move to a new relay:

1. Run a new one (step 1 of Setup).
2. Add it on one site with **The Known Universe → Add relay**.
3. Remove the old one there.

The other sites follow within a minute or two, if they can still reach that site
(directly or through another relay); otherwise do the same on them. No new invites are
needed. Tip: use two
relays.

</details>

<details><summary><b>The relay token leaked, or you want a new one</b></summary>

```bash
docker exec ibs-relay ibs-relay new-token
```

The old token can no longer add sites. Sites already using the relay keep working and
get the new token automatically, so their invites stay valid.

</details>

<details><summary><b>The relay moved or has a new name</b></summary>

On one site, **The Known Universe → Relays → Edit address** next to the relay. The token
stays the same. The other sites using that relay pick up the new address from it within
a minute or two and reconnect, also when the old address still works. A site that can't reach any other site meanwhile needs
the new address set on it too.

</details>

### Sites

<details><summary><b>The whole site is lost</b></summary>

1. Start an empty manager.
2. Choose **Restore lost site → Peer via relay**. Enter the recovery phrase, your relay's
   address and token, the site ID of a site you were connected to (shown on its
   dashboard), and a new admin password. If you have no relay, use **Peer by address**
   instead.
3. The site's index comes back from that site, and your blocks are copied back from
   other sites in the background. Files are readable right away.

Files that weren't copied to other sites yet are lost with the site.

</details>

<details><summary><b>Another site is offline</b></summary>

New files get their copies on the sites that are online. After 2 minutes, the copies the
offline site held of your data are made again on other sites, if there are enough, at
most 20 MB/s (`IBS_REBUILD_RATE`). When it returns, the surplus copies are removed.

</details>

<details><summary><b>The log says "failed to sufficiently increase receive buffer size"</b></summary>

Harmless. Direct connections between sites ask Linux for a larger UDP buffer than it
allows by default. Everything works, but fast direct connections can be a little
slower. To allow it, run this on the machine that runs the manager (it can't be set
from inside Docker), then restart the container:

```bash
sudo sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000
printf 'net.core.rmem_max=7500000\nnet.core.wmem_max=7500000\n' | sudo tee /etc/sysctl.d/90-ibs-quic.conf
```

The second line keeps the setting after a reboot.

</details>

<details><summary><b>"1 of 2 copies" or "blocks pending"</b></summary>

Every file should have copies on two other sites, but fewer are online, or the copies
are still being made. Everything works normally, and the missing copies are made as
sites become available.

</details>

## S3 compatibility

Path-style requests (`http://host:9000/bucket/key`), Signature V4 and presigned URLs.
Tested with the AWS CLI and rclone; see `test/s3/`.

| Operation | Example | Notes |
|---|---|---|
| ListBuckets, CreateBucket, HeadBucket, DeleteBucket | `aws s3 ls`, `mb`, `rb` | `rb` needs an empty bucket (or `--force`) |
| GetBucketLocation, GetBucketVersioning | `aws s3api get-bucket-location` | No region; always "not versioned" |
| PutObject, GetObject, HeadObject | `aws s3 cp` | Ranges, `If-*` conditions, `x-amz-meta-*`, `Content-MD5` |
| DeleteObject, DeleteObjects | `aws s3 rm [--recursive]` | |
| CopyObject, UploadPartCopy | `aws s3 cp/mv s3://… s3://…` | Copies within a bucket use no extra space |
| ListObjects v1/v2 | `aws s3 ls` | Prefix, delimiter, paging |
| Multipart uploads | large `aws s3 cp` | Create, UploadPart, Complete, Abort, ListParts, ListMultipartUploads |
| GetObjectTagging | | Always empty |

Not supported (returns an error): versioning, ACLs and bucket policies, CORS, lifecycle
rules, setting tags, object lock, website hosting, and storage classes other than
`STANDARD`. Upload checksums such as CRC32 are accepted but not verified; signatures and
`Content-MD5` are verified.

rclone works for `ls`, `lsl`, `lsjson`, `copy`, `sync`, `bisync`, `move`, `check`,
`md5sum`, `cat`, `rcat`, `touch`, `link`, `delete`, `purge` and `cleanup`.
Modification times are kept. Not working: `--s3-versions`, `settier`, `about`. `mount`
is untested.

## FAQ

**Can I use IBS as S3 storage for an application?**

No. IBS is for backups and archives. It verifies, encrypts and copies every block,
copies may go through relays and home internet connections, and only one manager serves
a site. Point your backup tool (restic, rclone, Kopia, a NAS backup job) at it, and keep
live data on regular storage.

**Do all sites have to invite each other?**

No. An invite brings a site into the IBS: it connects to every site in it, and they to
it, sharing relays where needed.

**Can the IBS be shown on a map?**

Yes. Under **Settings → Location**, click roughly where the site is. Every site in the
IBS sees that spot, so pick a general area such as your town, not your address.
**The Known Universe** and the dashboard then switch between **Diagram** and **Map**. The map images come from OpenStreetMap; nothing about your IBS
is sent there.

**Can I remove a site from the IBS?**

No, only a site can take itself out: **The Known Universe → Leave the IBS**. Every site then
forgets it and deletes the copies it kept for it, and makes new copies of its own data
on the remaining sites. The site that left deletes the copies it kept for others; its
own files stay. Sites that were offline find out when they come back. It can join again
later with a new invite.

**Is an upload confirmed when it's stored locally, or once it's copied?**

Once it's stored at your site. Copies follow in the background. There's no option yet
to wait for them.

**What if a relay disappears?**

See [When something goes wrong → Relay](#relay). Sites never need to pair
again.

**How many copies are kept, and can I change it?**

Every file exists three times: at your site and on two other sites. That's fixed, so
there's nothing to set and nothing to get wrong. With only one other site, files have
one copy there and a warning says so; see ["1 of 2 copies"](#sites).

**How much more can I store?**

**Buckets** shows an estimate: what fits at your site and twice on two different other
sites. It's an estimate because other sites in the IBS can use the same free space.

**Is the site's metadata on the nodes too?**

Yes, an encrypted copy is on every node. A site can be restored from any of its nodes.

**Does IBS update itself?**

It finds and downloads new versions on its own, but only installs one when you click
**Install** under **Settings → Updates**. Storage nodes follow their manager
automatically. See [Updates](#updates).

**How much space do I share with the IBS?**

What your nodes' limits allow (**Nodes → Change**, or `IBS_STORAGE_LIMIT`). Other
sites' copies may use any free space, but your own files come first: when they need
room, your site pushes other sites' copies away. Their owners place them on another
site first; if the IBS is full, a file may drop to two copies (the original and one
elsewhere) until there is room again. Copies of a site that is offline are never pushed
away. **The Known Universe** shows what each site shares, and the dashboard warns when
the IBS is running out of space.

## Limitations

- **One manager per site.** If it's lost, it has to be restored. There's no automatic
  failover.
- **HTTPS needs a DNS provider with an API** (see [HTTPS](#https)). Without one, the
  web interface and S3 use plain HTTP; use them on your LAN or put a TLS proxy in front.
- **Full protection needs three sites.** With only one other site, files have one copy
  elsewhere instead of two, and the dashboard warns about it.
- **Updates from the web interface are for linux/amd64 only**, and not for relays.
  Relays are updated by pulling a new image.
- **Built for safety, not speed.** Throughput and latency are fine for backups, not for
  live workloads. Speed through a relay depends on the relay machine's bandwidth.

## License

MIT. See [LICENSE](LICENSE).
