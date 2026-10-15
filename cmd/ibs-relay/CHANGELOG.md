# IBS relay changelog

The relay (`ghcr.io/spagettikod/ibs/relay`) is versioned separately from IBS and only
released when it changes. Before 1.2.0, its changes are in the IBS changelog. Each
release says which IBS versions it works with.

## 1.4.0

**What to update:** relays, by pulling the new image (keep `/data`). Needed for short
invite codes; sites keep working with older relays, with long invites.

**Works with:** every IBS 1.x site; invite codes need IBS 1.4.0 or newer.

- **Invite codes:** a site can claim a number for an invite code, and a site joining
  with the code is put through to it once. The relay only passes encrypted bytes along.

## 1.3.0

**What to update:** optional; pull `ghcr.io/spagettikod/ibs/relay:1.3.0`, keeping `/data`.

**Works with:** every IBS 1.x site.

- Tells sites its version, shown under **IBS network → Relays**.

## 1.3.0-beta1

**Works with:** every IBS 1.x site; sites show its version from IBS 1.3.0-beta1.

**What to update:** optional; pull `ibs-relay:1.3.0-beta1` and keep `/data`.

- Tells sites its version, so they can show it under **IBS network → Relays**.

## 1.2.0

**Works with:** every IBS 1.x site.

- The relay released with IBS 1.2.0. From here on it has its own versions.
