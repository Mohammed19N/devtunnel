# Self-hosting Passway

`passway-server` has two modes. Self-hosted is the default; cloud mode, selected with `--mode cloud` or `PASSWAY_MODE=cloud`, adds accounts, a
GitHub-backed dashboard and plan limits. This page is about **self-hosted**
mode: your own relay with no accounts and no dashboard.

## Start a relay

```sh
passway-server --domain tunnels.example.com \
  --tls-addr :443 --tls-cert /etc/passway/fullchain.pem --tls-key /etc/passway/privkey.pem \
  --addr :80 --tokens secret1,secret2
```

Tunnels get random names such as `brave-marmot-93.tunnels.example.com`, or a
name the client asks for with `--name`.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--domain` | (required) | Base domain. Tunnels live at `<name>.<domain>`. |
| `--mode` | `$PASSWAY_MODE`, else `self-hosted` | `self-hosted` (no accounts or dashboard) or `cloud` (needs `--db` and GitHub OAuth settings). Self-hosters never need to set it. |
| `--addr` | `:8080` | Plain HTTP listener. With TLS enabled it redirects to HTTPS. Empty disables it. |
| `--tls-addr` | (empty) | TLS listener, e.g. `:443`. |
| `--tls-mode` | `files` | `files` reads `--tls-cert`/`--tls-key`. `acme` obtains and renews certificates automatically (see below). |
| `--tls-cert`, `--tls-key` | (empty) | Certificate and key files. Required with `--tls-addr` in `files` mode. |
| `--acme-email` | (empty) | ACME account email. Required with `--tls-mode acme`. |
| `--acme-dir` | `<dir of --db>/certs`, else `./certs` | Where certificates and the ACME account are stored. |
| `--acme-ca` | Let's Encrypt production | ACME directory URL. |
| `--tokens` | (empty) | Comma-separated accepted client tokens. Only valid in self-hosted mode. |
| `--db` | (empty) | SQLite database. In self-hosted mode it enables tokens created with `admin token create`. |
| `--open` | `false` | Allow no tokens on a non-localhost domain. That makes an open relay anyone can use; the server refuses to start otherwise. Not needed when `--tokens` or `--db` is set. |
| `--public-port` | `0` | Port shown in public URLs, if it differs from the listener (for example behind a proxy). |
| `--rate-limit` | `0` | Maximum requests per minute for each tunnel; over it, visitors get `429` with `Retry-After`. `0` is unlimited. |
| `--max-conns-per-tunnel` | `64` | Maximum concurrent proxied connections per tunnel. |
| `--browser-warning` | `false` | Show a one-time warning page to browser page loads before they reach a tunnel. Webhooks and other API traffic are not interrupted. Clients can skip it with a `Passway-Skip-Warning` header. |
| `--block-words` | `false` | Refuse tunnel names containing brand names and common phishing terms (`paypal`, `login`, `stripe`, ...). Always on in cloud mode. |
| `--proxy-protocol` | `false` | Require a PROXY protocol header (v1 or v2) on every TLS connection, so the server sees real client IPs behind a TCP passthrough proxy such as nginx `stream`. Connections without one are closed. |

Self-hosted tunnels are unlimited: there are no concurrent-tunnel, bandwidth or
reserved-subdomain limits. Any valid name can be requested unless you pass
`--block-words`. Edge pages (offline, errors) carry no Passway branding.

## Tokens

`--tokens` takes a fixed list. For tokens you can create and revoke without a
restart, use a database:

```sh
passway-server --domain tunnels.example.com --db /var/lib/passway/p.db ...
passway-server admin --db /var/lib/passway/p.db token create ci
```

`token create` creates the database if it doesn't exist yet, so this works
on a fresh install. The token is printed once. Only its hash is stored.

## DNS and TLS

- Point a wildcard record at the server: `*.tunnels.example.com` and
  `tunnels.example.com` (A and AAAA).
- Use a wildcard certificate for `*.tunnels.example.com` (and the apex).
  Wildcard certificates need a DNS-01 challenge, for example with certbot or
  lego and your DNS provider's plugin.
- Or let the server do it: `--tls-mode acme --acme-email you@example.com`
  with `PASSWAY_CLOUDFLARE_API_TOKEN` set in the environment (a Cloudflare API
  token with Zone.DNS edit on the domain's zone; there is no flag for it). The
  server obtains `tunnels.example.com` and `*.tunnels.example.com` over DNS-01
  and renews them. For a dry run, pass
  `--acme-ca https://acme-staging-v02.api.letsencrypt.org/directory`
  (Let's Encrypt staging: untrusted certificates, generous rate limits).
- Run it on `:443` with `--tls-addr`, or terminate TLS in front of it and pass
  `--public-port` if the public port differs.
- If you run a public relay, put your domain on the
  [Public Suffix List](https://publicsuffix.org/) so browsers isolate cookies
  between tunnels.

## Docker

The image is `ghcr.io/mohammed19n/passway-server`. It is a static binary on
distroless, runs as a non-root user and keeps its data in `/data`.

```sh
docker volume create passway
docker run -d --name passway --restart unless-stopped \
  -p 80:8080 -p 443:8443 -v passway:/data \
  -e PASSWAY_CLOUDFLARE_API_TOKEN=... \
  ghcr.io/mohammed19n/passway-server \
  --domain tunnels.example.com --addr :8080 --tls-addr :8443 --public-port 443 \
  --tls-mode acme --acme-email you@example.com --db /data/passway.db

docker exec passway /passway-server admin --db /data/passway.db token create laptop
```

- The container listens on unprivileged ports, and Docker maps 80 and 443 to
  them. `--public-port 443` keeps the port out of tunnel URLs.
- `--db` enables admin tokens. Certificates go to `/data/certs`.
- A named volume works as is. A bind-mounted host directory must be writable
  by uid 65532 (`sudo chown 65532:65532 <dir>`).

## systemd

Create a user and copy the binary from the release archive:

```sh
sudo useradd --system --home-dir /var/lib/passway --shell /usr/sbin/nologin passway
sudo install -m 0755 passway-server /usr/local/bin/
```

`/etc/systemd/system/passway-server.service`:

```ini
[Unit]
Description=Passway relay
After=network-online.target
Wants=network-online.target

[Service]
User=passway
Group=passway
# Holds only the Cloudflare token (mode 0600): PASSWAY_CLOUDFLARE_API_TOKEN=...
EnvironmentFile=/etc/passway/env
StateDirectory=passway
ExecStart=/usr/local/bin/passway-server --domain tunnels.example.com \
  --addr :80 --tls-addr :443 --tls-mode acme --acme-email you@example.com \
  --db /var/lib/passway/passway.db
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/passway
PrivateTmp=true
ProtectHome=true
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now passway-server
sudo -u passway passway-server admin --db /var/lib/passway/passway.db token create laptop
```

With `--tls-mode files` instead, make the key readable by the `passway` user
only.

## Behind an existing nginx

If nginx already owns ports 80 and 443, let it pass TLS through by SNI and
keep its own sites:

- run passway-server on loopback (`--tls-addr 127.0.0.1:9443 --addr 127.0.0.1:8081 --public-port 443 --proxy-protocol`);
- use the nginx snippets in [`deploy/nginx/`](../deploy/nginx).

[deploy.md](deploy.md) walks through it for the hosted cloud, and the same
steps apply to a self-hosted relay.

## Backups

`passway-server admin --db PATH backup <dest>` writes a consistent copy of the
database while the server runs. It refuses an existing `<dest>`, so use dated
names. [`deploy/backup.cron`](../deploy/backup.cron) is a nightly example.

## Admin commands

Admin commands read and write the SQLite database directly and work while the
server runs.

```
passway-server admin --db PATH <command>
  ban user <login> [--reason R]    ban host <label> [--reason R]
  unban user <login>               unban host <label>
  set-plan <login> free|pro        whois <label>
  admit <login>                    backup <dest>
  token create <name>              token list        token revoke <id>
```

`ban`, `unban`, `set-plan`, `whois` and `admit` apply to cloud accounts. A
ban closes the live tunnels within about 15 seconds. `admit` takes a user off
the waitlist; a waitlisted user is also admitted automatically at their next
sign-in once the number of Free accounts is below `--free-signup-cap`.
`whois` prints `<label> -> @<login> (plan, user id)`. `token` manages the
admin tokens used by self-hosted relays; they are refused in cloud mode. Every
command except `token create` needs an existing database. Revoking a token
closes the tunnels that use it (within about 15 seconds when revoked here).
