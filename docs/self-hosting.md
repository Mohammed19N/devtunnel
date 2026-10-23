# Self-hosting Passway

`passway-server` has two modes. Cloud mode (the default) adds accounts, a
GitHub-backed dashboard and plan limits. This page is about **self-hosted**
mode: your own relay with no accounts and no dashboard.

## Start a relay

```sh
passway-server --self-hosted --domain tunnels.example.com \
  --tls-addr :443 --tls-cert /etc/passway/fullchain.pem --tls-key /etc/passway/privkey.pem \
  --addr :80 --tokens secret1,secret2
```

Tunnels get random names such as `brave-marmot-93.tunnels.example.com`, or a
name the client asks for with `--name`.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--domain` | (required) | Base domain. Tunnels live at `<name>.<domain>`. |
| `--self-hosted` | `false` | No accounts or dashboard. Without it the server runs in cloud mode and needs `--db` and GitHub OAuth settings. |
| `--addr` | `:8080` | Plain HTTP listener. With TLS enabled it redirects to HTTPS. Empty disables it. |
| `--tls-addr` | (empty) | TLS listener, e.g. `:443`. |
| `--tls-cert`, `--tls-key` | (empty) | Certificate and key files. Required with `--tls-addr`. |
| `--tokens` | (empty) | Comma-separated accepted client tokens. Only valid with `--self-hosted`. |
| `--db` | (empty) | SQLite database. In self-hosted mode it enables tokens created with `admin token create`. |
| `--open` | `false` | Allow no tokens on a non-localhost domain. That makes an open relay anyone can use; the server refuses to start otherwise. Not needed when `--tokens` or `--db` is set. |
| `--public-port` | `0` | Port shown in public URLs, if it differs from the listener (for example behind a proxy). |
| `--rate-limit` | `0` | Maximum requests per minute for each tunnel; over it, visitors get `429` with `Retry-After`. `0` is unlimited. |
| `--max-conns-per-tunnel` | `64` | Maximum concurrent proxied connections per tunnel. |
| `--browser-warning` | `false` | Show a one-time warning page to browser page loads before they reach a tunnel. Webhooks and other API traffic are not interrupted. Clients can skip it with a `Passway-Skip-Warning` header. |

Self-hosted tunnels are unlimited: no concurrent-tunnel, bandwidth or
reserved-subdomain limits. Requested names containing blocked words (brand
names and common phishing terms) are refused.

## Tokens

`--tokens` takes a fixed list. For tokens you can create and revoke without a
restart, use a database:

```sh
passway-server --self-hosted --domain tunnels.example.com --db /var/lib/passway/p.db ...
passway-server admin --db /var/lib/passway/p.db token create ci
```

The database must already exist: start the server once with `--db`, which
creates it, before running `admin`. The token is printed once. Only its hash
is stored.

## DNS and TLS

- Point a wildcard record at the server: `*.tunnels.example.com` and
  `tunnels.example.com` (A and AAAA).
- Use a wildcard certificate for `*.tunnels.example.com` (and the apex).
  Wildcard certificates need a DNS-01 challenge, for example with certbot or
  lego and your DNS provider's plugin.
- Run it on `:443` with `--tls-addr`, or terminate TLS in front of it and pass
  `--public-port` if the public port differs.
- If you run a public relay, put your domain on the
  [Public Suffix List](https://publicsuffix.org/) so browsers isolate cookies
  between tunnels.

## systemd (placeholder)

```ini
[Unit]
Description=Passway relay
After=network-online.target

[Service]
ExecStart=/usr/local/bin/passway-server --self-hosted --domain tunnels.example.com \
  --tls-addr :443 --tls-cert /etc/passway/fullchain.pem --tls-key /etc/passway/privkey.pem \
  --db /var/lib/passway/p.db
AmbientCapabilities=CAP_NET_BIND_SERVICE
DynamicUser=yes
StateDirectory=passway
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`DynamicUser=yes` cannot read key files under `/etc/passway` unless they are
world-readable, which a private key should not be. Use `LoadCredential=` to
pass the certificate and key, or replace `DynamicUser` with a dedicated
`User=`/`Group=` that owns them.

## Docker (placeholder)

The official image ships in a later milestone. The command will look like:

```sh
docker run -p 80:80 -p 443:443 -v passway:/data ghcr.io/mohammed19n/passway-server \
  --self-hosted --domain tunnels.example.com --db /data/p.db ...
```

## Admin commands

Admin commands read and write the SQLite database directly and work while the
server runs.

```
passway-server admin --db PATH <command>
  ban user <login> [--reason R]    ban host <label> [--reason R]
  unban user <login>               unban host <label>
  set-plan <login> free|pro        whois <label>
  token create <name>              token list        token revoke <id>
```

`ban`, `unban`, `set-plan` and `whois` apply to cloud accounts. A ban closes
the live tunnels within about 15 seconds. `token` manages the admin tokens
used by self-hosted relays.
