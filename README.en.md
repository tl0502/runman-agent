# Runman Agent (Fork · Podman-Only)

> Slice your idle server into rentable NAT VPS instances and amortize hosting costs.
>
> This project is forked from [narwhal-cloud/runman-agent](https://github.com/narwhal-cloud/runman-agent) with second-party development for self-hosted deployments: **only the Podman backend is maintained**, with a self-hosted release/update channel, plus container network isolation, elastic IP, security hardening and white-label branding.

[中文](README.md) · [Gateway Protocol](PROTOCOL.en.md)

---

## What Is This

The Agent is a daemon running on the host (bare metal) that slices server resources into NAT VPS instances — managing container lifecycle, port forwarding, traffic accounting — and connects to the platform over a gRPC bidirectional stream (see [PROTOCOL.en.md](PROTOCOL.en.md)). It also ships a local web panel (default `:8792`).

```
Your server ──► Runman Agent ──► NAT VPS instances (containers on narwhal-net 10.91.0.0/20)
                 │
                 └── gRPC bidirectional stream: heartbeat, commands, traffic billing
```

## Key Differences from Upstream

| Difference | Details |
|---|---|
| **Podman backend only** | The installer rejects `--virt incus/cloudhv`; leftover incus/cloudhv Go code is unmaintained |
| **Custom netavark** | Uses [tl0502/netavark-Runman](https://github.com/tl0502/netavark-Runman) (adds a `snat_ipv6` option); auto-replaces `/usr/libexec/podman/netavark` on install/update |
| **Container network isolation & whitelists** | Bridge-family nft rules isolate containers from each other; host port whitelist + private-subnet whitelist (v0.5.x) |
| **Elastic IP model** | New `public_ipv4` config: all containers share one host IPv4 for ingress listening and egress SNAT |
| **Security hardening** | `userns=auto`, `no_new_privs`, pids limit, fine-grained capabilities (on by default) |
| **White-label branding** | Panel site name and site icon (favicon) replaceable via panel/API |
| **Self-hosted update channel** | Agent self-update and installer downloads point at `tl0502/runman-agent`; `netavark_base` is configurable |
| **SemVer + CI releases** | Pushing tag `v*` triggers GitHub Actions to test, build and publish a Release |
| **API compatibility promise** | Platform-facing API semantics are frozen; new features ship as optional fields only — zero impact for existing clients |

## Requirements

| Item | Requirement |
|---|---|
| OS | **Debian 13 (Trixie)** strongly recommended |
| Arch | x86_64 (amd64) · aarch64 (arm64) |
| RAM | ≥ 1 GB |
| Disk | ≥ 10 GB recommended; installer keeps 1536 MiB reserved on the root partition |
| Backend | Podman (auto-installed); existing Docker workloads coexist untouched |

## Install

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/tl0502/runman-agent/main/install.sh)
```

Or from the latest release:

```bash
bash <(curl -fsSL https://github.com/tl0502/runman-agent/releases/latest/download/install.sh)
```

The installer downloads Agent and rfw binaries from `tl0502/runman-agent` Releases and the custom netavark from `tl0502/netavark-Runman`. Pin a version with `AGENT_RELEASE_TAG=vX.Y.Z`; point at a mirror with `RUNMAN_AGENT_DOWNLOAD_BASE` and `NETAVARK_BASE`.

### One-shot examples

```bash
# IPv4 NAT only
bash install.sh zh --virt podman --nat4 --non-interactive --generate-token

# NAT4 + IPv6 SNAT (single public IPv6)
bash install.sh zh --virt podman --data-size 3G --ipv6-mode snat \
  --non-interactive --generate-token

# NAT4 + manual routed /64 (HE 6in4, WireGuard, provider static route)
bash install.sh zh --virt podman --nat4 --non-interactive \
  --ipv6-mode subnet --ipv6-addr 2001:db8:100::1 \
  --ipv6-subnet 2001:db8:100::/64 --ipv6-iface wg6 --ipv6-routed

# Podman registry mirror (leave empty if docker.io is reachable)
bash install.sh zh --virt podman --data-size 8G --ipv6-mode none \
  --podman-registry-mirror https://mirror.example.com --non-interactive

# Detect/validate IPv6 without installing anything
bash install.sh zh --detect-ipv6 --non-interactive
bash install.sh zh --validate-ipv6 --ipv6-mode subnet \
  --ipv6-addr 2001:db8:100::1 --ipv6-subnet 2001:db8:100::/64 \
  --ipv6-iface wg6 --ipv6-routed
```

### Main menu

Run `bash install.sh zh` with no arguments:

| # | Action |
|---:|---|
| 1 | Install/update (guided network setup) |
| 2 | Show installation/service status |
| 3 | Repair network and restart Agent/firewall |
| 4 | Reset web panel password |
| 5–6 | Detect / validate IPv6 |
| 7–8 | Show / rotate integration token |
| 9–10 | Back up / roll back IPv6 configuration |
| 11 | Uninstall (keeps containers/images) |
| 12 | Configure/refresh container images |
| 13 | Agent-only upgrade (keeps networking) |
| 0 | Exit |

---

## Elastic IP Model (Important)

Containers always use the `narwhal-net` private network (`10.91.0.0/20`). When `public_ipv4` is set:

- **Ingress**: all port mappings listen on that address only;
- **Egress**: container outbound traffic is SNAT'd to that address;
- **Ingress and egress must be the same IP**.

The `entry_host` reported to the platform resolves as:

```
host (manual entry, DDNS domains supported) > public_ipv4 > auto-detected public IPv4
```

Under the elastic IP model, `public_ipv4` is exactly the `entry_host` reported to the platform. `public_ipv6` is a reserved field, not implemented yet.

## Container Network Isolation & Access Control

On by default since v0.5.x, implemented entirely via `runman-hostfw` nft tables (**not** netavark isolate — without br_netfilter, same-bridge traffic never hits the FORWARD hook, so isolate does not apply):

| Table | Purpose |
|---|---|
| `inet runman-hostfw` | INPUT host port whitelist (`host_allow_ports`) + FORWARD private-subnet blocking (`container_allow_subnets`) |
| `bridge runman-hostfw` | Inter-container isolation: `meta ibrname == obrname drop` — containers reach only the gateway and outbound |

Defaults:

- `container_isolate` (default true): containers cannot reach each other;
- `host_allow_ports`: TCP ports containers may reach on the host; empty means no host access at all;
- `container_allow_subnets`: private CIDRs containers may reach; empty means all private ranges (10/8, 172.16/12, 192.168/16, 169.254/16) are blocked — outbound public internet only.

> Gotcha: netavark skips chain recreation when the table already exists. Rebuilding the network requires `nft delete table inet netavark` first, then reload. Menu item 3 handles this automatically.

## Security Hardening

Applied by default at container creation (all toggleable in `config.json`):

| Config | Default | Purpose |
|---|---|---|
| `container_userns` | true | `userns=auto` — container root maps to an unprivileged host uid (requires `/etc/subuid`, `/etc/subgid`) |
| `container_no_new_privs` | true | Blocks setuid privilege escalation |
| `container_pids_limit` | 2048 | cgroup pids ceiling |
| `container_drop_caps` / `container_add_caps` | empty | Extra capabilities dropped/added at creation (systemd + sshd images depend heavily on the default set — test before aggressive drops) |

## IPv6

| Mode | Trigger | Behavior |
|---|---|---|
| `none` | No public IPv6 | IPv4 only |
| `snat` | Single `/128` or prefix `/65`–`/127` | Containers use ULA `fd91:cafe:cafe:10::/64` and egress via host IPv6 SNAT (paired with the custom netavark `snat_ipv6=false` option) |
| `subnet` | Prefix ≤ `/64` | The routed /64 is split into /112 subnets per container — each container gets its own public IPv6 |

Auto-detection distinguishes native on-link prefixes from tunnel-link prefixes (HE 6in4, WireGuard); manual /64 supports `--ipv6-routed`. Affected sysctl/NIC config is backed up to `/var/lib/narwhal-agent/backups/` before changes, with one-click rollback.

## White-Label Branding

- Site name (top bar + browser title) editable in panel settings;
- Site icon upload (PNG/ICO) also serves as the browser favicon; falls back to the built-in icon;
- API: `POST /api/brand/icon`.

## Updates & Releases

- **Release flow**: push tag `v*` (e.g. `v0.5.3`) → GitHub Actions runs Go + installer tests → builds amd64/arm64 → publishes the Release. `main` builds continuously as `continuous-<commit>`.
- **Agent self-update**: checks `tl0502/runman-agent` every 6 hours, applies new versions after a random 24–72h delay via `--update-only --non-interactive`; the source is overridable via `update_repo` / `update_base_url` / `update_api_url`.
- **Agent-only upgrade** (keeps networking and workloads):

```bash
bash install.sh zh --update-only
journalctl -u narwhal-agent-update --no-pager   # auto-update task log
```

- Disable auto-update: `systemctl edit narwhal-agent`, add `Environment=RUNMAN_AGENT_AUTO_UPDATE=0`, then `daemon-reload && restart`.

## Installed Files

| Component | Path |
|---|---|
| Agent binary | `/opt/narwhal-agent/narwhal-agent` |
| Config | `/opt/narwhal-agent/config.json` |
| Agent database | `/opt/narwhal-agent/agent.db` |
| Data dir / backups | `/var/lib/narwhal-agent` (`backups/` inside) |
| Podman data disk | `/xfs_disk.img` → XFS mounted at `/data` |
| Systemd services | `narwhal-agent.service` (optional `rfw.service`) |
| Custom netavark | `/usr/libexec/podman/netavark` (replaced by installer) |

**Web panel**: `http://<server-ip>:8792`

## Key Config Fields (`/opt/narwhal-agent/config.json`)

### Platform & updates

| Field | Purpose |
|---|---|
| `token` | Platform integration token (`--show-token` / `--rotate-token`) |
| `platform_addr` | Platform gRPC address; changes reconnect automatically |
| `platform_insecure` | Skip gRPC TLS verification (self-signed/IP-only setups) |
| `update_repo` / `update_base_url` / `update_api_url` | Self-update source (default `tl0502/runman-agent`) |
| `netavark_base` | Custom netavark download base |
| `virt_type` | Fixed to `podman` |
| `web` / `web_user` / `web_pass_hash` | Panel listen address, username, bcrypt hash |
| `monitor_nic` / `monitor_disk` | Traffic/disk accounting targets |
| `max_port_forward` / `max_forward_ips` | Per-VM port-forward and source-IP limits |
| `traffic_reset_day` | Monthly traffic reset day (1-28) |

### Added by this fork

| Field | Purpose |
|---|---|
| `public_ipv4` | Elastic IP: shared ingress listen + egress SNAT address; empty keeps legacy behavior |
| `site_name` | Panel brand name (site icon lives in the config dir, set via panel/API) |
| `container_isolate` | Inter-container isolation (default true) |
| `host_allow_ports` | Host TCP port whitelist for containers |
| `container_allow_subnets` | Private-subnet whitelist for containers (CIDRs) |
| `container_no_new_privs` / `container_userns` / `container_pids_limit` / `container_drop_caps` / `container_add_caps` | Container hardening |

## Service Management & Troubleshooting

```bash
systemctl status narwhal-agent          # Agent status
journalctl -u narwhal-agent -f          # Live logs
systemctl restart narwhal-agent
systemctl status rfw                    # rfw firewall
```

Common issues:

- **Agent fails to start**: `journalctl -u narwhal-agent --no-pager -n 50`
- **rfw fails to start**: some cloud NICs don't support XDP; add `--xdp-mode skb` to the service args (the installer auto-switches on mount failure)
- **Podman data disk not mounted**: `mount -o defaults,pquota,loop,noatime /xfs_disk.img /data`, then restart the Agent
- **Containers can't egress with Docker coexisting**: don't flush Docker rules; `systemctl restart runman-podman-forwarding` — this oneshot service only whitelists `10.91.0.0/20` and `fd91:cafe:cafe:10::/64` in `DOCKER-USER`
- **Network rules broken**: menu item 3 rebuilds a missing `narwhal-net` from current config (deleting the `inet netavark` table first to avoid stale chains), then restarts Agent/rfw

## Protocol & Compatibility

Platform integration uses the gRPC bidirectional `AgentGateway` stream; see [PROTOCOL.en.md](PROTOCOL.en.md) for the full protocol and a minimal Go implementation.

**Compatibility rule**: platform-facing API semantics must not change; new capabilities are added exclusively as optional fields so legacy clients are unaffected. Agent implementers only need PROTOCOL.md — fork backend differences are irrelevant to the wire protocol.

## Local Development

```bash
go build ./...          # Fails on Windows (manager/cloudhv uses syscall.Setsid) — Linux-only, covered by CI
go test -tags containers_image_openpgp ./...
bash -n install.sh
python3 -m unittest discover -s tests -v
```

Web assets live in `web/static/`; panel API in `web/server.go`.
