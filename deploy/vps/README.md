# The edge VPS

The edge is the only machine on the public internet (`architecture.md` §1,
§7). It runs Traefik and the WireGuard hub, behind nftables, and nothing
else:

- **Traefik** terminates TLS for the portal (`:443`) and for customers'
  databases (`:5432`), with a Let's Encrypt wildcard by DNS-01 (Q4, Q14). It
  routes the portal to `otter api`, and each database by SNI to its
  cluster's PgBouncer, over the tunnel.
- **WireGuard** (`wg0`, `10.62.0.1/24`): each node dials in from behind its
  NAT. A peer's `AllowedIPs` are its tunnel address and its tenant subnet.
- **nftables** lets in 443, 5432, 51820/udp, and SSH from an allowlist. It
  forwards nothing, and lets in nothing from the tunnel: the edge opens
  connections, never the nodes.

The edge holds no platform credential and has no write path into the
platform. Traefik *pulls* its database routes from `otter api` (#103).
Breaking into it gives an attacker the TLS keys and the DNS token, not the
platform (`security.md`, R9).

The commands are for Debian 13, as root. Every host-specific value
(names, addresses, keys, the DNS token) is a placeholder here; the real
values stay out of the repository.

## Files

| Path | Owner, mode | What |
|---|---|---|
| `/etc/nftables.conf` | `root:root 0644` | [`nftables.conf`](nftables.conf) |
| `/etc/systemd/system/nftables.service.d/only-ours.conf` | `root:root 0644` | [`nftables-only-ours.conf`](nftables-only-ours.conf): stop and reload leave other tools' tables alone |
| `/etc/wireguard/wg0.conf` | `root:root 0600` | [`wg0.conf.sample`](wg0.conf.sample) |
| `/etc/wireguard/wg0.key` | `root:root 0600` | The hub's private key, made here |
| `/usr/local/bin/traefik` | `root:root 0755` | Traefik, from its release, checksum verified |
| `/etc/systemd/system/traefik.service` | `root:root 0644` | [`traefik.service`](traefik.service) |
| `/etc/traefik/` | `root:traefik 0750` | Settings |
| `/etc/traefik/traefik.yml` | `root:traefik 0640` | [`traefik.yml.sample`](traefik.yml.sample): static settings |
| `/etc/traefik/dynamic/edge.yml` | `root:traefik 0640` | [`dynamic/edge.yml.sample`](dynamic/edge.yml.sample): the portal's route, TLS |
| `/etc/traefik/dns-token` | `root:root 0400` | The DNS API token, `Zone:DNS:Edit` on the one zone (Q14) |
| `/var/lib/traefik/acme.json` | `traefik:traefik 0600` | Certificates and the ACME account key, made by Traefik |

## Install

### 1. Firewall

Fill the SSH allowlist first (or use the management-interface line), or
loading the file over SSH ends the session. Check the file, and arm an
undo that opens everything again in 3 minutes, in case it locks you out:

```sh
apt-get install -y nftables wireguard-tools
install -m 0644 nftables.conf /etc/nftables.conf
install -d /etc/systemd/system/nftables.service.d
install -m 0644 nftables-only-ours.conf /etc/systemd/system/nftables.service.d/only-ours.conf
systemctl daemon-reload
nft -c -f /etc/nftables.conf
systemd-run --unit=fw-undo --on-active=180 /usr/sbin/nft delete table inet filter
systemctl enable --now nftables
# From a second session: SSH still works? Then:
systemctl stop fw-undo.timer
```

### 2. WireGuard hub

```sh
sh -c 'umask 077; wg genkey > /etc/wireguard/wg0.key'
wg pubkey < /etc/wireguard/wg0.key       # → each node's [Peer] for the edge
install -m 0600 wg0.conf.sample /etc/wireguard/wg0.conf   # then fill it in
systemctl enable --now wg-quick@wg0
```

A new node is a new `[Peer]` and `systemctl reload wg-quick@wg0`. Its
side, `PersistentKeepalive` included, is in `docs/runbook-m0.md` Part I
(#22).

### 3. Traefik

From its [release](https://github.com/traefik/traefik/releases), 3.6.8 or
later, its checksum checked against the release's list:

```sh
V=v3.7.17
cd "$(mktemp -d)"
curl -fsSLO "https://github.com/traefik/traefik/releases/download/$V/traefik_${V}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/traefik/traefik/releases/download/$V/traefik_${V}_checksums.txt"
grep " traefik_${V}_linux_amd64.tar.gz\$" "traefik_${V}_checksums.txt" | sha256sum -c -
tar -xzf "traefik_${V}_linux_amd64.tar.gz" traefik
install -m 0755 traefik /usr/local/bin/traefik

useradd --system --home-dir /var/lib/traefik --shell /usr/sbin/nologin traefik
install -d -o root -g traefik -m 0750 /etc/traefik /etc/traefik/dynamic
install -o root -g traefik -m 0640 traefik.yml.sample /etc/traefik/traefik.yml
install -o root -g traefik -m 0640 dynamic/edge.yml.sample /etc/traefik/dynamic/edge.yml
# then fill in both files
sh -c 'umask 077; cat > /etc/traefik/dns-token'   # paste the token, then Ctrl-D
install -m 0644 traefik.service /etc/systemd/system/traefik.service
systemctl daemon-reload
systemctl enable --now traefik
```

An upgrade is the same download and `install`, then `systemctl restart
traefik`.

### 4. DNS

DNS-only records (Q14): the portal's name and `*.<database zone>`, both
`A` records pointing to the VPS's IPv4.

## Check

```sh
ss -tlnup | grep -E ':(22|443|5432)\b'   # traefik on 443 and 5432; sshd on 22
ss -ulnp | grep 51820                     # the hub
wg show wg0                               # each node: a recent handshake
journalctl -u traefik -e                  # the certificates obtained, no errors
curl -sS https://<portal host name>/healthz            # ok, once #22 is up
openssl s_client -starttls postgres -connect <any>.<database zone>:5432 \
  -servername <any>.<database zone> </dev/null 2>/dev/null | openssl x509 -noout -subject -ext subjectAltName
# → the wildcard, served for any name in the zone
```

From outside, everything else is closed: `nmap -Pn <VPS address>` shows
443 and 5432 open, and 22 only from the allowlist.
