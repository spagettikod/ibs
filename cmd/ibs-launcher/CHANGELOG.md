# IBS launcher changelog

The launcher (`ghcr.io/spagettikod/ibs/launcher`) is versioned separately from IBS. Each
release says which IBS versions it works with.

## 1.0.0

**What to update:** nothing; new installs and the switch from the `ibs` image use it.

**Works with:** IBS 1.3.0 or newer.

- Downloads, verifies and runs signed IBS releases, restarts into versions installed
  under **Settings → Updates**, and goes back when a new version fails to start.
- Storage nodes get IBS from their manager, never from the internet.

## 1.0.0-beta2 (not released on its own; in 1.0.0)

- A storage node gets IBS from its manager on its first start: the version the manager
  runs, trusted by the manager's release keys. Nodes never go to the internet and need
  no release settings; until the manager answers, the launcher keeps asking it.

## 1.0.0-beta1

**Works with:** IBS 1.3.0-beta1 or newer.

- First version: downloads, verifies and runs signed IBS releases, restarts into the
  version installed under **Settings → Updates**, and goes back to the previous version
  when a new one stops during its first minutes. Downloads the latest stable release
  unless `IBS_BETA=true`.
