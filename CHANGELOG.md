# Changelog

Each release says what to update and which launcher and relay it works with: IBS,
the launcher and the relay are released separately.

## 1.11.0

**What to update:** sites, under **Settings → Updates**, once 1.10 has switched them to
age; nodes follow. Sites on 1.9 update to 1.10 first.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- Reads only data encrypted with age. A site still switching stays on 1.10 until it's done.

### Fixed

- A direct connection could show as going through a relay on one of the two sites.
- **Room for your files** says only what you and other sites use.

## 1.10.1

**What to update:** sites on 1.10.0 that can't install 1.11.0 although they show no blocks
left to switch, under **Settings → Updates**.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### Fixed

- 1.11.0 couldn't be installed on a busy site that had finished switching to age.

## 1.10.0

**What to update:** sites, under **Settings → Updates**; nodes follow. After updating, a
site can't go back to 1.9.x. Its existing data switches to age in the background, which
sends it once more to the other sites.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- **Encryption with age:** everything IBS stores is encrypted with
  [age](https://age-encryption.org), a widely used, reviewed format, with post-quantum
  keys (ML-KEM with X25519). The recovery phrase stays the only thing to keep. Existing
  data switches over by itself, in the background; **Your files** shows how many blocks
  are left.
- **`ibs export`** writes all files to a folder from the recovery phrase, the site's
  index and the disks with its blocks, without a running site or other sites.

## 1.9.1

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- **Your files** on the dashboard shows the cleanup of blocks: how many small files are
  still to be combined, and how much space from deleted files is still to be freed.

### Fixed

- **Combining existing small files was very slow:** about one block every 5 seconds, and
  each rewritten block waited up to 2 minutes for its copies. It now goes block after
  block at up to `IBS_CLEANUP_RATE`, and copies start at once.
- **No fixed waits:** new small files are combined as soon as their bucket stops
  receiving files (was after 10 seconds), and a lone small file is copied right away.
  Cleanup pauses while files arrive or are deleted, or other sites send copies, and
  goes on 5 seconds after.

## 1.9.0

**What to update:** sites, under **Settings → Updates**; nodes follow. After updating, a
site can't go back to 1.8.x: small files then share blocks, which older versions can't
read.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- **Small files share blocks:** small files are stored together in 4 MB blocks, each
  encrypted on its own, and copied to other sites once per block. A site with many small
  files has far fewer blocks, so copying, restoring and scrubbing go faster and use less
  disk on all three sites. Existing sites move their small files over by themselves, in
  the background, at most `IBS_CLEANUP_RATE` (default 20 MB/s).
- **Deleting a file in a shared block** frees its space when the block is rewritten,
  once it's half empty.
- **Large files:** a file's blocks after its first GB are 16 MB, once every other site
  runs 1.9.0 or newer.
- Storage nodes read only the part of a block a file needs.

## 1.8.8

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- **Scrub history:** a scrub that found problems has **Details**, listing them; they were
  kept but only the latest scrub's could be seen.
- **Check now** is on the Versions card of the Updates page.

### Fixed

- **On a phone, Settings** opened General right away; it now unfolds its sections in the
  menu to choose from, as on a computer.
- Access keys' created date and a scrub's "Last checked" show the date only, with the time
  on hover; "Next scrub" shows its date and time on hover.
- The built-in release key is called **Official release key** (was "IBS releases").

## 1.8.7

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### Fixed

- **"403 Forbidden" when checking GitHub:** GitHub allows 60 checks an hour from your
  network, and each site start used one. Sites now remember when they last checked, and
  when the limit is used up they say until when and wait for it.

## 1.8.6

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with an invite).

### New

- **Nodes** shows the files coming back after a restore: how much is left, the speed and
  about how long.
- **Short invite codes are gone:** sites invite with the invite alone, now short enough
  to paste anywhere.

## 1.8.5

**What to update:** the relay to 1.6.0 first (pull the new image, keep `/data`), then
sites under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.6.0
to join through a relay with the new long invites).

### New

- **Long invites are about a third as long** (around 100 characters with two relays):
  they only say how to reach your site; the rest comes from your site when the other
  site joins. Through a relay, the joining site needs no relay token. Sites before
  1.8.5 can't read the new invites; through a relay it needs relay 1.6.0.
- **Restoring a site is faster:** files come back from the other sites four blocks at a
  time, one batch after another, instead of one block at a time every few seconds.

### Fixed

- **Room for your files** during a restore says how much is still coming back in its
  one line (was a line of its own).

## 1.8.4

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.5.0
to restore through a relay that doesn't know the lost site).

### New

- **Release notes** for each version on the Updates page.
- **Remove old versions** on the Updates page removes every version older than the one
  running, except the newest you can roll back to.
- Confirmations and error messages show in the page's own style instead of the
  browser's.
- **After restoring a site**, the dashboard shows the files coming back: a spinner, how
  much is left, the speed and about how long. The diagram shows them flowing in.
- The diagram takes only the room its sites need.

### Fixed

- **Room for your files** right after a restore showed more than you'll have: files not
  back yet now count as used.

## 1.8.3

**What to update:** the relay to 1.5.0 first (pull the new image, keep `/data`), then
sites under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.5.0
to restore through a relay that doesn't know the lost site).

- **Restoring a lost site needs only the recovery phrase** and the address of a relay
  your other sites use: no relay token and no site ID. Your other sites vouch for your
  site on the relay (relay 1.5.0) and the restore finds them itself. With an older relay
  it works if your site was registered with it.
- **The diagram's lines are gentle arcs**, bent so they pass no site or relay and cross
  as few other lines as possible, without long detours.
- Sites named like "siteA" show as "SA" in the diagram (was "SI").

## 1.8.2

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **The bucket browser can be sorted** by name, size or date: click a column header,
  again to reverse. The whole folder is sorted, not just the files shown. Folders stay
  above files; the choice is remembered in the browser.
- **Large folders show 2,000 files at a time**, with **Next 2,000** and **Load all**.
  Loaded files stay when the folder changes.
- **GitHub is checked once a day** unless chosen otherwise (was every 15 minutes).

## 1.8.1

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- The scrub history shows the date each scrub started; the time is on hover. **Your
  copies on other sites** shows the date and time of the last check.
- **Folders in the bucket browser show their size, file count and latest change** (the
  newest change to a file inside, at any depth). A folder is counted in the background
  the first time it's shown, then kept up to date with every upload and delete.

## 1.8.0

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Line speed and copying speed are told apart:** "Speed" is now **Line speed** (what the
  connection carries, measured on large transfers); "copying" is what actually arrives,
  lower because each copy is also read and stored. While copies move, the diagram shows
  their speed and direction on the connection.
- **Delete folders** in the bucket browser, with how many files and how much it holds in
  the confirmation. Large folders are deleted in the background.
- **The diagram puts your site in the centre,** the other sites around it, and relays on
  an inner ring between them.
- **A running scrub says about how long it has left** and when it should be done, on
  the Scrub page and the dashboard. The scrub history shows when each scrub started.
- **A node without a limit shares its free space,** not its disk's size: what IBS stores
  there plus what's empty. The Nodes page, the dashboard and what other sites see use
  that, and **Change share** says how much IBS can use at most.
- **Choose how often GitHub is checked** for new versions: every 15 minutes, 6 hours or
  once a day (**Settings → Updates**).

## 1.7.5

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **No more Copying panel popping up:** the dashboard's **Copies on other sites** shows a
  spinner, progress, speed and time left while copying, and **The IBS** a spinner when
  other sites' copies arrive. **Connected sites** in The Known Universe shows a spinner
  where copies move or deleted copies are being removed.

## 1.7.4

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **A spinner shows when files are arriving or being deleted,** on the dashboard's Your
  files and Room for your files and next to the busy bucket, while the figures catch
  up. It goes within a second after the last change.

## 1.7.3

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Pages load at once, however many files a site has.** Figures that take a pass over
  every block (copies, per site, node and bucket) are kept by one background routine
  and read from its cache; they may be a few seconds behind. Right after a start they
  show a spinner until counted. With 270,000 blocks the dashboard took 3 s (20 s and
  more on a busy disk); now under 1 ms.
- **Less background work:** the replication and trim scans run when something changed,
  and otherwise every 2 minutes, instead of every 5 seconds.

## 1.7.2

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Rolling back is possible more often:** only database changes that an older version
  would misread block going back to it (so far: limited access keys, from 1.7.0).
  Versions a site can never go back to are removed by themselves once the running
  version has started fine; uploading one is refused, saying why.
- **The web interface stays responsive while many files are uploaded:** the dashboard's
  copy counts take one pass over the blocks instead of five (with 50,000 files about
  0.5 s instead of 1.3 s), pages refresh at most every 2 seconds during a stream of
  changes, and the database backup goes out once uploads pause, or every minute while
  they keep coming, instead of every 15 seconds.

## 1.7.1

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **The dashboard shows the room for your files:** the empty space plus other sites'
  copies that make way for yours, instead of only the empty space. Copies of offline
  sites, which can't make way, don't count.

## 1.7.0

**What to update:** sites, under **Settings → Updates**; nodes follow. Existing access
keys keep full access to all buckets.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Access keys with limits:** a key can be read only (list and download), and limited
  to some buckets; it only sees those. Change a key's limits under **Access keys →
  Edit**; its secret stays. Deleting a bucket takes it off the keys that list it.
- Clicking **The IBS** on the dashboard opens The Known Universe; dashboard tiles no
  longer underline their text when pointed at.
- **A scrub catches up** on copies it couldn't check because their site was down: at
  the end it checks them if the site is back, and the "couldn't be reached" note goes.
- **A scrub that couldn't check everything says so** ("Not complete"), on the Scrub page
  and the dashboard, instead of looking clean. The data checked shows your data's size,
  not all copies added up.
- The dashboard's scrub tile says when the last scrub was stopped.
- **Dashboard messages say first what they mean for your files,** then why.
- You stay logged in for two weeks (was 12 hours).

## 1.6.1

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Nodes** shows a site's own blocks, and copies kept for other sites on a line of
  their own. **The Known Universe** says when deleted copies are still being removed
  on another site. **Your files** on the dashboard shows the number of blocks.
- **The network diagram** draws a site-to-relay line only when the site doesn't connect
  through that relay already.

## 1.6.0

**What to update:** sites, under **Settings → Updates**; nodes follow. Update every
site: a scrub only checks the copies on sites running 1.6.0. New sites and nodes can use
launcher 1.1.0.

**Works with:** launcher 1.0.0 or newer (1.1.0 recommended), relay 1.2.0 or newer (1.4.0
for short invite codes).

- **Fix: uploads taking over 10 minutes could lose their first blocks** while reporting
  success. An upload now owns its blocks until it finishes, and fails if one is missing.
- **No deduplication:** every file has its own blocks. Deleted and overwritten data is
  removed here and on other sites within seconds. Abandoned multipart uploads go after
  10 minutes.
- **Scrubs check your copies on other sites,** by a challenge only the exact bytes
  answer, and copy missing or damaged ones again. The history shows the data read.
- **A site offline for 2 minutes** (was 30) gets its copies re-created elsewhere, at most
  20 MB/s (`IBS_REBUILD_RATE`). Storage nodes: 30 minutes (`IBS_NODE_GRACE`).
- **Limits by progress, not time:** transfers fail after 30 seconds without progress,
  never while data moves. Storage nodes now update over slow networks too.
- Code for older versions removed, including `ibsn1.` node join tokens.

## 1.5.4

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

- The dashboard's replication tile says how many copies each file has on other sites,
  e.g. "1 of 2 per file; the next needs another site".

## 1.5.3

**What to update:** sites, under **Settings → Updates**; nodes follow.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

- **See copies move:** while copies are made, the dashboard shows where they go, how
  fast, what's left and about how long it takes (or why copying waits). Dots run along
  the connection in the network diagram, and The Known Universe shows the speed per site.
- **Faster copying:** four blocks at a time, and no pause between batches.
- **GitHub is checked for new versions every 15 minutes** (was every six hours).
- **Copied to other sites** on the dashboard shows progress also when there are fewer
  sites than copies.

## 1.5.2

**What to update:** sites, under **Settings → Updates**; nodes follow. Relays spread
between sites that are both on 1.5.2.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

- **Copying to other sites no longer stalls** after about 250 blocks when there are
  fewer sites than copies, or when the next site in line is full.
- **Relays spread:** a relay added or removed on one site is added or removed on the
  others. A site keeps a relay that is its only way to be reached.
- `IBS_RELAY_ADDRESS` no longer undoes a relay's newer address when a site restarts.

## 1.5.1

**What to update:** sites, under **Settings → Updates**; nodes follow. A site still on
1.5.0 may not get 1.5.1 from another site: use **Check now** there.

**Works with:** launcher 1.0.0, relay 1.2.0 or newer (1.4.0 for short invite codes).

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
