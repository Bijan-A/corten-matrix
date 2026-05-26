# Docker

The bridge ships as a Docker image at `ghcr.io/lrhodin/imessage`. The image bundles the bridge binary (built with all rustpush patches applied via `make build`), bbctl, runtime dependencies, the Apple Root CA, and the existing install scripts. Updates are `imessage update` once the host CLI is installed.

Host-side commands (logs, lifecycle, migration, aliases) live in a small `imessage` CLI script alongside the image. The container only does container things; host concerns stay on the host.

## Quick start (new install)

```bash
# 1. Install the host CLI (one line, always on PATH).
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/imessage \
    | sudo install /dev/stdin /usr/local/bin/imessage

# 2. Drop in a compose file. Edit BEEPER + the bind-mount path.
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml

# 3. Start the container and run the interactive setup wizard.
imessage start
imessage setup
```

You don't need to `mkdir` the host bind-mount source. If it doesn't exist, Docker creates it on first start and the container chowns it to the bridge user automatically.

## The `imessage` CLI

| Command | What it does |
|---|---|
| `imessage setup` | Run the interactive setup wizard inside the container (Beeper login, iMessage login, toggles). |
| `imessage login` | Re-run only the iMessage login flow. |
| `imessage logs` | Tail bridge logs (`docker logs -f bridge`). |
| `imessage status` | Show whether the bridge container is running. |
| `imessage shell` | Open a bash shell inside the container (debugging). |
| `imessage start` | `docker compose up -d`. |
| `imessage stop` | `docker compose stop bridge`. |
| `imessage restart` | `docker compose restart bridge`. |
| `imessage update` | `docker compose pull && docker compose up -d`. |
| `imessage pull` | `docker compose pull` (no restart). |
| `imessage migrate` | One-shot migration from a bare-Linux install — see below. |
| `imessage install-aliases` | Add `start-imessage` / `stop-imessage` / `restart-imessage` / `imessage-log` / `imessage-setup` / `imessage-logs` aliases to your shell rc file. Auto-detects bash vs zsh from `$SHELL`. Idempotent. |
| `imessage uninstall-aliases` | Remove the managed-alias block. |

Compose-driven subcommands (`start` / `stop` / `restart` / `update` / `pull`) look for `docker-compose.yml` in the current directory. Set `IMESSAGE_COMPOSE_FILE=/path/to/docker-compose.yml` to run them from anywhere.

To update the CLI itself: re-run the same `curl … install` line. To uninstall: `sudo rm /usr/local/bin/imessage`.

## Host paths

The container only cares about `/data` internally — the host bind-mount source is your choice:

| Platform | Typical host path |
|---|---|
| Standard Linux | `~/.local/share/mautrix-imessage` (matches bare-Linux install path) |
| UNRAID | `/mnt/user/appdata/mautrix-imessage` |
| Synology | `/volume1/docker/mautrix-imessage` |
| TrueNAS / ZFS | dataset of your choice |

Edit the `volumes:` line in `docker-compose.yml` to point at your path.

## UID / GID

The container starts as root just long enough to chown `/data` to the bridge user, then drops privileges via `gosu`. The long-lived bridge process is never root.

Defaults: UID 1000, GID 1000. Override via `PUID` / `PGID` in the compose `environment:` block when your host appdata is owned by a different UID — common on UNRAID (often `99:100`), Synology, or shared-server setups:

```yaml
environment:
  PUID: "99"
  PGID: "100"
```

Check with `stat -c '%u:%g' /path/to/your/appdata`.

## Migrating from a bare-Linux install

If you already have a working bare-Linux install at `~/.local/share/mautrix-imessage` and want to switch to Docker without losing state, login, or the iCloud Keychain trust circle:

```bash
# 1. Install the host CLI (same one-liner as above).
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/imessage \
    | sudo install /dev/stdin /usr/local/bin/imessage

# 2. Run the migration helper. It strips the bare-Linux systemd-based
#    shell aliases from ~/.bashrc / ~/.zshrc, stops + disables + removes
#    the systemd unit, and leaves your state at
#    ~/.local/share/mautrix-imessage in place.
imessage migrate

# 3. Drop in the compose file. Default bind mount already points at the
#    right host path.
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml

# 4. Start the container. It resumes against existing state — no
#    re-login, no re-setup.
imessage start
```

`imessage migrate` is idempotent. Safe to re-run if something didn't complete the first time.

## Updating

```bash
imessage update
```

Images are published manually via the `docker` GitHub Actions workflow (not on every commit), so a new tag means someone deliberately built and pushed it.

## Apple Silicon NAC relay

If your hardware key was extracted from an Apple Silicon Mac, the bridge inside the container fetches NAC validation data from a relay running on that Mac. The relay URL, bearer token, and TLS fingerprint are all embedded in the base64 hardware-key blob, so there's nothing extra to configure in compose — the container just needs network reachability to the relay's hostname/port.

For Docker on the same Mac, `host.docker.internal` resolves to the host. For Docker on a Linux server reaching a remote Mac, the key needs to have been extracted with a hostname/IP that's routable from the Linux server.

## Out of scope

- **`backfill_source: chatdb`** doesn't work in Docker (macOS-only, Full Disk Access required). Use CloudKit backfill.
- **macOS Contacts framework** — same reason. Use the external CardDAV path if you want non-iCloud contacts.
- **`extract-key` / NAC relay GUIs** — those run on the user's Mac to mint the hardware key, not inside the bridge container.
