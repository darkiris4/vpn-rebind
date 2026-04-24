# vpn-rebind

**Automatically recreates containers so they reattach to their VPN network namespace after a provider restart.**

<!-- Replace with an actual recording once you have one -->
<!-- ![vpn-rebind demo](docs/demo.gif) -->

[![GitHub release](https://img.shields.io/github/v/release/darkiris4/vpn-rebind)](https://github.com/darkiris4/vpn-rebind/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## The problem

When a VPN container (like Gluetun) restarts, Linux replaces its network namespace. Docker does not dynamically reattach containers to a replaced network namespace. Any containers sharing it via `network_mode: "container:gluetun"` keep running — but their network namespace is now invalid. Traffic either breaks entirely or bypasses the VPN depending on firewall configuration — in both cases, the container is no longer operating as intended.

Health checks won't catch it. Compose restart policies won't fix it. The only reliable fix in Docker today is to stop, remove, and recreate each dependent container so it attaches to the new namespace.

**vpn-rebind does that automatically**, with no polling and no manual steps.

---

## Quickstart

Add one service to your existing `docker-compose.yml`:

```yaml
services:
  vpn-rebind:
    image: ghcr.io/darkiris4/vpn-rebind:latest
    container_name: vpn-rebind
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    group_add:
      - "999"                    # docker group GID — verify: getent group docker
    environment:
      - VPN_REBIND_PROVIDER=gluetun
      - VPN_REBIND_DEPENDENTS=qbittorrent,prowlarr
    read_only: true
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
```

That's it. Change `gluetun`, `qbittorrent`, and `prowlarr` to your container names, then `docker compose up -d vpn-rebind`.

> **Requirement:** Dependent containers must use `network_mode: "container:<vpn>"`. vpn-rebind only fixes namespace attachment — it has no effect on bridge-networked containers.

> **Note:** The socket mount needs write access (no `:ro`) so vpn-rebind can stop and recreate containers.

For a complete working stack (Gluetun + qBittorrent + Prowlarr), see [`docker-compose.example.yml`](docker-compose.example.yml).

---

## Do you need this?

You likely need vpn-rebind if:

- You use `network_mode: "container:gluetun"` (or any other VPN provider)
- Your VPN container restarts occasionally — on update, crash, or reconnect
- You've noticed any of these symptoms after a VPN restart:
  - torrents stalling until you manually restart qBittorrent
  - indexers timing out until containers are bounced
  - services that "just start working again" after a manual restart

If you've ever fixed your stack by restarting dependent containers after a VPN reconnect — this tool automates that.

---

## Why vpn-rebind instead of X?

| Approach | Why it falls short |
|---|---|
| **Do nothing** | Dependent containers keep running in a stale/invalid namespace after every provider restart |
| **Restart policies** | Docker only restarts *stopped* containers — dependents keep running, silently broken |
| **`depends_on` in Compose** | Only controls startup order; does nothing when a dependency restarts mid-run |
| **`docker compose up -d --force-recreate`** | Requires manual intervention or external automation; not event-driven |
| **Health checks** | Need a custom script per container to detect the namespace change and trigger a recreate — fragile and duplicated |
| **Watchtower** | Watches for image updates, not network namespace events |
| **Cron / polling scripts** | Polling adds latency; you're reimplementing this tool with more failure modes |
| **vpn-rebind** | Event-driven, debounced, zero config for simple setups, no polling, no scripts |

vpn-rebind starts without acting — a clean `docker compose up` does not trigger spurious rebinds.

---

## Configuration

### Environment variables (simplest, no file needed)

| Variable | Description | Default |
|---|---|---|
| `VPN_REBIND_PROVIDER` | VPN provider container name | — |
| `VPN_REBIND_DEPENDENTS` | Comma-separated dependent names: `qbittorrent,prowlarr` | — |
| `VPN_REBIND_LABEL_SELECTOR` | Discover dependents by label: `vpn.required=true,vpn.provider=gluetun` | — |
| `VPN_REBIND_DELAY` | Wait after provider starts before rebinding (e.g. `5s`) | `3s` |
| `VPN_REBIND_STOP_TIMEOUT` | Graceful stop timeout per dependent | `10s` |
| `VPN_REBIND_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `info` |

### YAML config file (for multiple providers or advanced use)

Mount a file to `/config/config.yaml`:

```yaml
rebind_delay: 5s
stop_timeout: 10s
log_level: info

groups:
  - name: gluetun
    provider: gluetun
    dependents:
      - qbittorrent
      - prowlarr
    label_selector:         # discover additional dependents by label
      vpn.required: "true"
      vpn.provider: gluetun

  - name: wireguard         # second provider on the same host
    provider: wireguard
    label_selector:
      vpn.provider: wireguard
```

Then mount the config directory:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - ./vpn-rebind:/config:ro
```

See [`config.example.yaml`](config.example.yaml) for all options.

### Label-based discovery

Add labels to dependent containers to have vpn-rebind discover them automatically without listing names explicitly:

```yaml
services:
  qbittorrent:
    labels:
      vpn.required: "true"
      vpn.provider: gluetun
```

Labels and the explicit `dependents` list are combined and deduplicated.

---

## How it works

```
Docker daemon ──events──► vpn-rebind
                               │
                  provider die │ → mark group "needs rebind"
                  provider start│ → schedule debounced rebind (after rebind_delay)
                               │
                        for each dependent:
                          stop → remove → recreate → start
```

vpn-rebind only acts after it has observed a provider go down **and** then come back up. A clean `docker compose up` at startup does nothing.

**The key step:** after Docker creates a container, `NetworkMode` is stored as `container:<id>` — a stale ID once the provider restarts. Before recreating each dependent, vpn-rebind rewrites this to `container:<provider-name>` so Docker resolves it to the provider's current running instance. Without this, recreated containers would attach to a non-existent namespace.

---

## Docker Compose compatibility

vpn-rebind preserves the full `ContainerConfig` and `HostConfig` when recreating containers (env vars, mounts, labels, restart policy — all kept). The one side-effect: after a recreate, Compose will detect a config hash mismatch and offer to reconcile on the next `docker compose up -d`. That's a one-time, harmless sync.

---

## Security considerations

vpn-rebind requires read-write access to the Docker socket to stop, remove, and recreate containers. This grants significant control over Docker on the host — treat it the same as any other privileged service.

The container itself is locked down:

- Runs as a non-root user
- Read-only filesystem (`read_only: true`)
- No Linux capabilities (`cap_drop: ALL`)
- No privilege escalation (`no-new-privileges:true`)

Only deploy in environments where you trust the image source. The `group_add: "999"` pattern grants socket access via group membership rather than running as root.

---

## Design goals

- **Event-driven** — no polling; acts on Docker events only
- **Idempotent** — safe across restarts; cold-start does not trigger rebinds
- **Minimal privileges** — non-root, no capabilities, read-only filesystem
- **No Compose dependency** — works with any Docker setup, not just Compose stacks

---

## Building from source

Requires Go 1.22+.

```sh
make build          # → bin/vpn-rebind
make test           # go test -race ./...
make image          # local Docker image
make image-multiarch # multi-arch push (requires buildx)
```
