# Developing IBS

## Layout

| Path | What |
|---|---|
| `cmd/ibs` | Manager and storage node (`ibs manager`, `ibs node`, plus admin commands) |
| `cmd/ibs-relay` | Relay |
| `cmd/ibs-launcher` | Launcher, a separate program with its own version: what the published `ibs/launcher` image runs; installs and starts signed releases |
| `cmd/ibs-sign` | Creates the release signing key and signs releases |
| `internal/release` | Signed releases (`.ibsrelease`), the version store and trusted keys |
| `internal/site` | Metadata, blocks, replication, peers, relays, direct connections, recovery |
| `internal/s3api` | S3 protocol and Signature V4 |
| `internal/web` | Admin API (the UI's backend) |
| `internal/node` | Storage node and join token |
| `internal/relay` | Relay server and client |
| `internal/tlscert` | HTTPS certificates from Let's Encrypt (lego, DNS-01) |
| `internal/blockstore`, `internal/blockcodec`, `internal/keys` | Block storage, block encryption, keys and TLS |
| `web/` | React web interface (embedded into the `ibs` binary) |
| `contrib/systemd` | systemd units shipped with the release binaries |
| `test/` | Client and network tests (below) |

## Building

```bash
docker build -t ibs .                             # IBS built in, for development and tests (not published)
docker build -t ibs-launcher --target launcher .  # launcher image (published)
docker build -t ibs-relay --target relay .        # relay image (published)
```

Without Docker you need Go 1.26 and Node 20:

```bash
cd web && npm ci && npm run build && cd ..
go build ./cmd/ibs ./cmd/ibs-relay
```

`cd web && npm run dev` serves the UI with hot reload and sends `/api` to a manager on
`:8080`.

## Local demo

`docker compose up -d --build` starts a relay and three sites, with data in `./.data/`:

| Site | Web interface | S3 |
|---|---|---|
| site-a (manager + one node) | http://localhost:8081 | http://localhost:9001 |
| site-b | http://localhost:8082 | http://localhost:9002 |
| site-c | http://localhost:8083 | http://localhost:9003 |

The admin password is `changeme123` everywhere. Join them into one IBS with invites (one
site invites each of the others; they then connect to each other on their own). site-a
and site-b connect directly; site-c only through the relay. To start over, delete
`.data`.

## Tests

`test/run-all.sh` runs everything below: `go vet` and the Go tests, then the Docker
suites three at a time (`JOBS=…` to change that), with a summary and timings at the
end and a log per suite in `test/logs/`. Run it before every release. Afterwards,
`test/cleanup.sh` removes what the suites leave in Docker (their containers, the images
they build or download, old IBS layers) without pruning anything else. The suites one
by one:

```bash
go test ./...            # unit tests
go vet ./...
test/s3/aws-cli.sh       # every supported S3 operation with the AWS CLI
test/s3/rclone.sh        # rclone compatibility (runs rclone from its Docker image)
test/nat/run.sh          # two sites behind NAT routers connect directly
test/nat/run.sh symmetric  # with strict NAT they stay on the relay
test/tls/run.sh          # HTTPS certificate through DNS-01, against Pebble (a test CA)
test/mesh/run.sh         # one invite brings a site into the whole IBS; leaving and rejoining
test/updates/run.sh      # updates with the launcher: GitHub, other sites, nodes, rollback
test/scrub/run.sh        # bit rot: damaged blocks repaired on read and by a scrub
```

`test/tls` builds Pebble from source, since its published images may not be pullable.
The DNS providers IBS includes, and the settings the web interface shows for each, are
generated from lego's provider descriptions: edit the list in
`scripts/gen-dns-providers.py` and run it (also after updating lego). Each provider adds
to the binary size, so keep the list to common ones.

The `test/` scripts need the `ibs` image (and `ibs-relay` for `test/nat`) built from the
current source (`test/updates` builds its own launcher image and signs test releases
with a throwaway key). Each starts its own throwaway sites and removes them afterwards.

Every S3 operation IBS supports must have cases in `test/s3/aws-cli.sh`, and in
`test/s3/rclone.sh` where relevant. Keep the S3 table in the README in sync.

## Releasing

Releases are signed. Sites and nodes run the `ibs/launcher` image: it downloads the
signed `ibs-<version>-linux-amd64.ibsrelease` from the GitHub release, and sites only
install versions signed with a key they trust. The built-in key is the public key in
`internal/release/keys.go`; its private half is the signing key, by default
`~/.config/ibs/signing.key` (or `$IBS_SIGNING_KEY`).

- **Once:** `go run ./cmd/ibs-sign keygen` creates the signing key and prints its public
  key. Put that in `internal/release/keys.go` and keep a backup of the key file
  somewhere safe: without it, no site will accept a new official release, and anyone
  who has it can sign one.
- **Your own builds:** `ibs-sign keygen -key my.key`, then
  `ibs-sign sign -key my.key -version 1.3.0-mine -platform linux/amd64 -o ibs.ibsrelease ibs`.
  Add the public key (`ibs-sign pubkey -key my.key`) to a site and upload the file under
  **Settings → Updates**.

- **Rotating the key:** create a new key, add its public key to `BuiltinKeys` in
  `internal/release/keys.go` (keep the old one) and release, signed with the old key.
  IBS saves its built-in keys in `/data/keys` on start, so launchers already out there
  trust the new key too. Sign later releases with the new key; release a launcher with
  it for new installs.
- **If the key leaks:** remove it from `BuiltinKeys`, add its ID to `RevokedKeys`, and
  release IBS (signed with the new key) and a new launcher. Tell admins to pull the new
  launcher: it no longer trusts the old key, and if no installed version is left that it
  can run, it downloads the latest release.

A release records the oldest version it can work alongside (`MinCompatible` in
`internal/version`) and its database level (the number of migrations). Raise
`MinCompatible` when a release changes what sites or nodes exchange in a way older
versions can't handle; sites then refuse to roll back past it.

1. Add the version's notes to `CHANGELOG.md` (`## <version>`) and commit. IBS, the
   launcher and the relay are released separately, so the notes start with two lines
   that `release.sh` checks for: **What to update** (sites, nodes, relays, and how) and
   **Works with** (the launcher and relay versions this release needs or supports).
   Launcher and relay notes have a **Works with** line too, naming IBS versions, which
   `release-image.sh` checks for.
2. Tag and push: `git tag -a v<version> -m "IBS <version>" && git push origin main v<version>`.
3. Build and publish:

   ```bash
   GITHUB_TOKEN=… scripts/release.sh <version>
   ```

   This builds the Linux binaries (with systemd units, `README.md`, `LICENSE` and
   `SHA256SUMS`) and signs the `.ibsrelease` files. With `GITHUB_TOKEN` set (a token
   that may create releases), it also creates the GitHub release and uploads the files;
   `RELEASE_ONLY=1` does only that, from an existing `dist/<version>/`. It builds no
   images, and notes when the launcher or relay changed since their last release.

### The launcher and the relay

Both are programs of their own, with their own versions and changelogs, released only
when they change:

| | Version | Changelog | Tag |
|---|---|---|---|
| Launcher | `Version` in `cmd/ibs-launcher/main.go` | `cmd/ibs-launcher/CHANGELOG.md` | `launcher-v<version>` |
| Relay | `Version` in `internal/relay/version.go` | `cmd/ibs-relay/CHANGELOG.md` | `relay-v<version>` |

Raise the version by hand when the program changes, including code it shares with IBS
(for the launcher everything `go list -deps ./cmd/ibs-launcher` lists, such as
`internal/release` and `internal/node`; for the relay `internal/relay` and
`internal/keys`). `release.sh` reminds you when one changed since its last release.
Then `scripts/release-image.sh launcher` (or `relay`) builds and pushes
`ghcr.io/spagettikod/ibs/<launcher|relay>:<version>` (and `latest` for stable versions)
after `docker login ghcr.io`; tag the commit as the script shows. Keep the README's
image tags at the newest versions.

Neither gets a GitHub release: sites and launchers look for IBS versions in the
repository's GitHub releases. They take the newest stable release, and betas (GitHub
pre-releases) only with `IBS_BETA=true` (launcher) or **Include betas** (sites).

IBS learns the launcher's version from `IBS_LAUNCHER`, and the launcher must keep
working with every IBS version: it only verifies, starts and restarts the program in
`/data/versions`, and passes all arguments and settings on unchanged.

Running the release binaries without Docker (the archive also holds the current relay
binary): install `ibs` and `ibs-relay` to
`/usr/local/bin`, create a system user `ibs`, and use the units in `contrib/systemd`.
Settings go in `/etc/ibs/*.env`.

## Logo

`python3 docs/logo/generate.py` regenerates every logo file from one definition: the
mission patch with the name around the rim (`ibs-patch.svg`, used in the README), the
badge without it (`ibs-mark.svg`, the web interface), a simpler badge for 32 px and below
(the favicon), the shapes the UI draws (`web/src/logo-paths.ts`), and
`docs/logo/preview.svg` (`rsvg-convert -w 1440 docs/logo/preview.svg -o
docs/logo/preview.png` for the PNG).
