# Changelog

Each release says what to update and which launcher and relay it works with: IBS,
the launcher and the relay are released separately.

## Unreleased

- **New versions from other sites** arrive over slow links and relays too (the
  download gave up after 30 seconds). The Updates page lists them in the version table
  while they are fetched, and says why when fetching fails.
- **New versions on other sites are fetched right away**, not up to a minute later, and
  the log says which site has one.
- **Renamed relays spread:** a new address set on one site reaches the others even while
  the old one still works. If the relay doesn't connect at the new address, a site keeps
  the old one.

## 1.5.0

**What to update:** sites, under **Settings → Updates**; nodes follow. Update every
site: copies of a site still on 1.4.0 can't be pushed away.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

- **No share setting:** other sites' copies may use all free space. When your own files
  need room, your site pushes other sites' copies away: their owners place them
  elsewhere first, or keep two copies of the file. Copies of offline sites stay. The
  owner's dashboard says when the IBS is running out of space; the missing copies come
  back when there is room.
- **The network diagram** draws direct connections around relays, not through them.
- Old `ibs1.` invites no longer work; make a new invite.

## 1.4.0

**What to update:** sites, under **Settings → Updates**; nodes follow. Relays: pull
`ghcr.io/spagettikod/ibs/relay:1.4.0` (keep `/data`) for short invite codes.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

- **Short invite codes**, like `relay.example.com 7-comet-harbor-violet`. A code works
  once; a wrong guess spends it, and the relay can't read or use it.
- **Long invites are half as long** (`ibs2.…`). Old `ibs1.` invites work until 1.5.0.
- **Room for your own files:** other sites' copies may use at most half of a site's
  storage (adjustable under **Settings → General**). The dashboard warns when a full site
  holds up copies.
- **Relay moves spread:** set a relay's new address on one site; the others follow.
- **Install new versions automatically** (optional, **Settings → Updates**).
- **The Known Universe** shows the storage each site shares, and the total.
- **The network diagram** has no labels; names show when you point at a site or relay.
- Releases are signed with OpenSSL (see DEVELOPMENT.md).

## 1.3.3

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer.

- **The browser tab shows the site's name**, e.g. "roland-001 - IBS".
- **IBS network is now called The Known Universe.**
- The network map shows only the ping on each connection; the speed is in its details.
- **A new logo:** a retro mission patch with a planet under the Milky Way. The web
  interface and the browser tab use the badge; the README shows the full patch.
- **The sidebar** shows "Intergalactic Backup System" next to the logo, and the site name
  set apart from the menu.
- **After a wrong password** the login page selects it, so the next one can be typed
  straight away.
- Fixed: after renaming the site, the sidebar showed the old name until a reload.

## 1.3.2

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer.

- **Sites on the same LAN find each other in Docker:** a site offers the name its web
  interface is opened with as a guess at its LAN address (shown under **IBS network →
  Direct address**).
- Fixed: a direct address added while sites were connected through a relay was only
  used after a restart.
- Fixed: after the site restarted, the web interface could show "Reconnecting" until
  the page was reloaded.

## 1.3.1

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer.

- Fixed: Copy buttons did nothing on sites opened over plain HTTP.

## 1.3.0

**What to update:** sites and storage nodes: switch from the `ibs` image to
`ghcr.io/spagettikod/ibs/launcher:1.0.0`, keeping `/data`. Relays: optional,
`ghcr.io/spagettikod/ibs/relay:1.3.0`.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer.

- **Updates in the web interface:** **Settings → Updates** installs, rolls back and removes
  signed versions. Sites share new versions with each other; nodes follow their manager.
- **Scrubs:** a monthly check of everything a site stores, repairing damage from copies
  on other sites. New **Scrub** page and dashboard tile.
- **Versions** of sites, nodes and relays are shown in the web interface.
- **Reload notice** when the site has been updated while the page is open.
- Fixed: a large upload could go past the storage limit.
- Fixed: downloading a file with an unreadable block returned an empty file instead of
  an error.

## 1.3.0-beta2

**What to update:** sites: **Settings → Updates → Install**; their nodes follow.

**Works with:** launcher 1.0.0-beta1 or newer, relay 1.2.0 or newer (relay 1.3.0-beta1
to see its version in the relays table).

- **Settings → Updates:** **Upload a version** and **Add key** open dialogs from their
  cards. **Check now** shows that it's checking, when it last checked and what it found.
- **After Install,** the page waits for IBS to restart and reloads into the new
  version, or says that the new version didn't start and the launcher went back.
- **Versions in their own column** in the sites, relays and nodes tables, instead of
  "IBS 1.3.0" under the name.
- **The log names the relay's version** when a site connects to it ("not reported" for
  relays before 1.3.0-beta1).

## 1.3.0-beta1

**What to update:** sites and nodes: switch their image from `ibs` to `ibs-launcher`,
keeping `/data`. Relays: optional, relay 1.3.0-beta1 shows its version to sites.

**Works with:** launcher 1.0.0-beta1 or newer, relay 1.2.0 or newer.

- **The relay is versioned on its own** and only released when it changes; see
  `cmd/ibs-relay/CHANGELOG.md`. Relay 1.3.0-beta1 tells sites its version.
- **Updates from the web interface.** Sites and nodes now run the new
  `ghcr.io/spagettikod/ibs/launcher` image (a separate program, version 1.0.0) that
  downloads and runs signed IBS releases; the `ibs` image is no longer published. **Settings → Updates** installs new versions,
  rolls back to compatible older ones, removes old ones, and manages the keys releases
  must be signed with. Sites check GitHub (can be turned off) and fetch versions signed
  with a trusted key from each other; storage nodes follow their manager. The dashboard
  and Settings show when a newer version is available. A version that fails to start is
  undone by the launcher. Sites and nodes on the old image switch to `ibs-launcher` once,
  keeping their `/data`. Release keys can be removed once no version signed with them is
  left (not the built-in one). Betas are opt-in: `IBS_BETA=true` on the launcher, and
  **Include betas** under **Settings → Updates**.
- **The web interface notices updates.** When the manager runs a different build than
  the open page (after an upgrade), a bar offers to reload. Works from this version on.
- **Versions everywhere:** the web interface shows which IBS version each storage node,
  site and relay runs (from this version on; older ones don't report it).
- **Nodes:** the button to change a node's share moved to the end of the row, so the
  "Shared" column only shows the amount.

## 1.2.0

- **"Space for other sites' data" is removed.** What a site shares with the IBS is what
  its nodes' limits allow; its own files and other sites' copies share that space. The
  **Copies & storage** section in Settings is gone with it.
- **The direct address moved to IBS network** (a card with an Edit dialog that explains
  when it's needed); the **Connections** section in Settings is gone.
- **Settings → General** now also holds the admin password, so Settings has three
  sections: General, Location and HTTPS.
- **README:** explains the "failed to sufficiently increase receive buffer size" log
  message and how to allow larger UDP buffers.

## 1.1.0

- **Every file exists three times:** at your site and on two other sites. The number of
  copies is no longer set per bucket (or as a default in Settings); existing buckets
  switch to two copies on other sites when the site starts. With fewer other sites
  online, files get as many copies as there are sites, with a warning.
- **Remaining space is one estimate** (Buckets): what fits here and twice on two
  different other sites. The dashboard's check for whether pending copies fit now also
  takes into account that the two copies must be on different sites.
- **The IBS on a map.** A site can set its location under **Settings → Location** by
  clicking a map (a general area, shared with the IBS as clicked). The network
  view switches between **Diagram** and **Map** (OpenStreetMap, in greyscale).
- **An invite brings a site into the whole IBS.** It connects to every site in the IBS,
  and they to it, registering with their relays where needed; sites no longer have to
  invite each other one by one.
- **Leave the IBS** replaces disconnecting sites: sites can't remove each other, a site
  can only leave by itself (**IBS network → Leave the IBS**). Every site forgets it,
  also sites that were offline at the time, and it can rejoin with a new invite.

## 1.0.0

The first stable release. IBS keeps encrypted copies of your backups at other sites
you and people you trust run, with no open ports at home. Everything from the betas
below is included; new since 1.0.0-beta3:

- **Node storage in the web interface:** set how much of each node's disk IBS may use
  under **Nodes → Change** (the whole disk, a size, or nothing). `IBS_STORAGE_LIMIT`
  still works and takes precedence.
- **Dashboard:** the storage tile shows the cap on other sites' copies, and a site
  that isn't connected to another site is a warning: it has no backups yet.
- **Setup and password forms** show the 8-character minimum and what is still needed.
- **Settings** is split into sections (General, Copies & storage, Connections, HTTPS,
  Admin password), listed under Settings in the menu. Buttons are smaller throughout,
  and labels in the setup wizard's HTTPS section are left-aligned again.

## 1.0.0-beta3

- **New logo:** a floppy disk on a steel shield.
- **HTTPS:** the manager can get and renew a Let's Encrypt certificate for the web
  interface and S3 with the DNS-01 challenge, so no port has to be open. Set it up
  under **Settings → HTTPS** or in the setup wizard (or with `IBS_TLS_*` variables).
  HTTP and HTTPS work on the same ports until **Require HTTPS** is turned on;
  `ibs https-off` turns it off again. The settings and certificate are backed up with
  the site's metadata and come back with a restore.
- **Relay token:** `ibs-relay token` prints it, and `ibs-relay new-token` replaces it.
  Sites already using the relay keep working and receive the new token. The token is
  no longer written to the relay's log.
- **README:** IBS is described as cold-storage backup, not general-purpose S3;
  connecting sites without a relay is documented.

## 1.0.0-beta2

- **Dashboard:** redesigned. One summary of what needs attention, four key numbers, and
  compact nodes and network cards.
- **IBS network:** inviting, joining and adding or editing relays happen in dialogs
  opened from buttons, instead of forms on the page.
- **License:** IBS is released under the MIT license. `LICENSE` is included in the
  images and the binary archives.
- **README:** reorganised (setup first, configuration tables, collapsible fixes in
  "When something goes wrong").

## 1.0.0-beta1

First public beta.

- **S3 storage:** buckets, uploads (including multipart), downloads with ranges, copy,
  delete and listing. Tested with the AWS CLI and rclone.
- **Encryption:** every 4 MiB block is encrypted with its bucket's key before it is
  stored. Other sites, nodes and relays only see encrypted blocks.
- **Copies on other sites:** each bucket keeps a chosen number of copies on other
  sites. They're re-created automatically when a site is lost, and trimmed when it
  returns.
- **No open ports at home:** sites connect through self-hosted relays (`ibs-relay`) and
  switch to direct connections (hole punching) when possible.
- **Storage nodes:** extra disk space per site, with per-node limits. When a node is
  lost, its data is restored from other sites, and a node can be drained before it's
  retired.
- **Recovery:** one recovery phrase per site. The site's index is replicated to every
  node and every connected site. A manager can be restored from a node, and a whole
  site from another site.
- **Web interface:** dashboard, network map with ping and speed, buckets and file
  browser, nodes, relays, access keys and settings. Updates live.
- **Releases:** container images for Linux (amd64), and Linux binaries with systemd
  units.
