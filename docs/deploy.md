# Deploying Passway Cloud

This is the runbook for the hosted cloud. It runs on a VPS where nginx already
serves other apps on ports 80 and 443. For a relay on a machine of its own, see
[self-hosting.md](self-hosting.md).

The files it uses are in [`deploy/`](../deploy):

| File | Goes to | What it is |
|---|---|---|
| `nginx/passway-stream.conf` | `/etc/nginx/passway/` | :443 SNI routing: the existing apps go to nginx, everything else to Passway |
| `nginx/passway-http.conf` | `/etc/nginx/passway/` | :80 server blocks for Passway and for unknown hosts |
| `nginx/existing-app.example.conf` | (reference) | The edit to make in each existing app's HTTPS server block |
| `systemd/passway-server.service` | `/etc/systemd/system/` | The service (`PASSWAY_MODE=cloud`, hardened) |
| `passway.env.example` | `/etc/passway/env` | Settings and secrets |
| `compose.yaml` | (alternative) | The same service as a container, instead of systemd |
| `backup.cron` | `/etc/cron.d/passway` | Nightly database backup |

```
            :443 (nginx stream, ssl_preread, PROXY header on every connection)
 client ──►  SNI = an existing app   ──► 127.0.0.1:8443  nginx http (the apps)
             anything else           ──► 127.0.0.1:9443  passway-server (TLS, --proxy-protocol)
            :80 (nginx http)
             the apps' hosts         ──► the apps' existing :80 blocks (unchanged)
             <domain>, *.<domain>,   ──► 127.0.0.1:8081  passway-server (ACME HTTP-01, redirect)
             unknown hosts
```

Passway terminates its own TLS. Its wildcard certificate comes over DNS-01
through Cloudflare, and its custom-domain certificates come on demand. nginx
only reads the SNI and passes the bytes through.

## 1. Prerequisites

Collect these before the deploy window. Steps marked **owner** need the
owner's accounts.

- **Domain on Cloudflare** (owner). Create these DNS records, with the proxy
  off (grey cloud, "DNS only"):
  - `<domain>`: A (and AAAA) to the VPS.
  - `*.<domain>`: A (and AAAA) to the VPS.
  - `edge.<domain>`: an **A/AAAA record** to the VPS, **not a CNAME**. Users
    point their custom domains at `edge.<domain>` with a CNAME. The dashboard
    verifies that CNAME with Go's `LookupCNAME`, which follows the whole chain
    to its canonical name. If `edge.<domain>` were itself a CNAME, the check
    would see the end of the chain instead, and verification would always fail.
- **Cloudflare API token** (owner): Zone → DNS → Edit, on that zone only. It
  goes in `PASSWAY_CLOUDFLARE_API_TOKEN`.
- **GitHub OAuth app** (owner):
  - Homepage `https://<domain>`, callback URL `https://<domain>/auth/github/callback`.
  - The client ID goes in `PASSWAY_GITHUB_CLIENT_ID`, the secret in
    `PASSWAY_GITHUB_CLIENT_SECRET`.
  - Your numeric user ID (`https://api.github.com/users/<login>`, field `id`)
    goes in `PASSWAY_ADMIN_GITHUB_IDS`.
- **Stripe** (owner):
  - A Pro product with a monthly recurring price. Its `price_...` ID goes in
    `PASSWAY_STRIPE_PRICE_PRO`. The site reads the amount from Stripe at
    startup, so set the final price in Stripe.
  - The secret key (`sk_live_...`, or `sk_test_...` for a test run) goes in
    `PASSWAY_STRIPE_SECRET_KEY`.
  - A webhook endpoint at `https://<domain>/stripe/webhook`, subscribed to
    these 5 events:
    - `checkout.session.completed`
    - `customer.subscription.created`
    - `customer.subscription.updated`
    - `customer.subscription.deleted`
    - `invoice.payment_failed`

    Its signing secret (`whsec_...`) goes in `PASSWAY_STRIPE_WEBHOOK_SECRET`.
  - Turn on the Customer Portal (Settings → Billing → Customer portal). The
    dashboard's "Manage billing" button opens it.
  - Without the three Stripe values, Pro shows "Coming soon".
- **SSH access** to the VPS, with sudo.
- **The list of hostnames the existing apps serve.** Get it from the live
  config, not from memory:

  ```sh
  sudo nginx -T 2>/dev/null | grep -E '^\s*server_name' | sort -u
  sudo nginx -T 2>/dev/null | grep -E '^\s*listen' | sort | uniq -c
  ```

  Every HTTPS hostname goes in the map in `passway-stream.conf`. A hostname
  missing there goes to Passway, and that app stops working.
- **nginx modules.** Check with `nginx -V 2>&1 | tr ' ' '\n' | grep -E 'stream|realip'`:
  - `stream`, `stream_ssl_preread` and `http_realip` are built into the
    nginx.org packages.
  - On Debian/Ubuntu, install `libnginx-mod-stream`.

## 2. Install Passway (no traffic yet)

Passway only listens on loopback, so this step does not affect the existing
apps.

```sh
sudo useradd --system --home-dir /var/lib/passway --shell /usr/sbin/nologin passway
sudo install -m 0755 passway-server /usr/local/bin/passway-server   # from the release archive
sudo install -d -m 0750 /etc/passway
sudo install -m 0600 deploy/passway.env.example /etc/passway/env
sudoedit /etc/passway/env                                            # fill in every value
sudo install -m 0644 deploy/systemd/passway-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now passway-server
journalctl -u passway-server -f
```

In the log, look for:
- `automatic TLS ... on_demand=true`;
- `billing enabled price="$7 / month"`, with your price;
- two `listening` lines (https on 127.0.0.1:9443, http-redirect on
  127.0.0.1:8081);
- certificate obtained for `<domain>` and `*.<domain>` (DNS-01 needs no
  nginx change).

Check it locally. The PROXY header is required, so a plain connection must
fail:

```sh
curl -sk --resolve <domain>:9443:127.0.0.1 https://<domain>:9443/healthz   # fails: no PROXY header
curl -s -H 'Host: <domain>' http://127.0.0.1:8081/healthz -o /dev/null -w '%{http_code}\n'   # 308 to https
```

`deploy/compose.yaml` runs the same service as a container (`docker compose
-f deploy/compose.yaml --env-file /etc/passway/env up -d`). Use it or the
systemd unit, not both.

## 3. Back up the nginx config

Take the backup first, before anything under `/etc/nginx` changes. Every
later step compares against it, and restores from it. Set `B` to the backup
path here, once:

```sh
B=/etc/nginx.bak-$(date +%F-%H%M); echo "backup: $B"
sudo test -e "${B:?}" && echo "STOP: $B already exists" || sudo cp -a /etc/nginx "${B:?}"
```

**Do §3 to §7 in this same shell.** Every later command uses `$B`, written as
`"${B:?}"` so that it stops with an error if `B` is unset. An unset `B` would
make a restore treat `/` as its source. If you open a new shell, set `B` again
first, to the path echoed above (for example
`export B=/etc/nginx.bak-2026-10-26-0215`).

## 4. Prepare the change and show it to the owner

Draft the edits in a staging copy, only to produce the diff. Don't run
`nginx -t` against the staging copy. Its includes are absolute paths
(`/etc/nginx/passway/...`, and on Debian `/etc/nginx/sites-enabled/*`), so
the test would fail on missing files, or quietly check the live files instead.
The real test happens in place in §5.

```sh
sudo rm -rf /tmp/nginx.new                         # no stale staging copy
sudo cp -a "${B:?}" /tmp/nginx.new
sudo install -d /tmp/nginx.new/passway
sudo install -m 0644 deploy/nginx/passway-stream.conf deploy/nginx/passway-http.conf /tmp/nginx.new/passway/
```

In `/tmp/nginx.new`:

1. **`passway/passway-stream.conf`**: replace the example hostnames in the map
   with every hostname from step 1. `hostnames;` allows `*.app.example.com`
   style entries.
2. **`passway/passway-http.conf`**: replace `passway.example` with the domain.
   If the existing config already has a `listen 80 default_server`, delete the
   last server block and move its `/.well-known/acme-challenge/` location into
   that default server (see §ACME below).
3. **`nginx.conf`**: add the stream include at the **top level**, outside
   `http { }`, and the http include inside it:

   ```nginx
   include /etc/nginx/passway/passway-stream.conf;   # top level

   http {
       ...
       include /etc/nginx/passway/passway-http.conf;
   }
   ```

4. **Every existing app's HTTPS server block**, as shown in
   `deploy/nginx/existing-app.example.conf`:
   - `listen 443 ssl;` / `listen [::]:443 ssl;` become `listen 127.0.0.1:8443 ssl proxy_protocol;`.
   - Add `set_real_ip_from 127.0.0.1;` and `real_ip_header proxy_protocol;`.

   Find every :443 listener in every form: `443`, `[::]:443`,
   `1.2.3.4:443`, `*:443`, with or without `ssl`, `default_server` and other
   parameters.

   ```sh
   sudo grep -rnE 'listen[^;]*\b443\b' /tmp/nginx.new
   ```

   Read every match by eye. The pattern also matches a `listen` whose port is
   something else but has `443` as a separate word elsewhere on the line.
   A listener split over several lines, or hidden behind an unusual
   `include`, won't match at all, so also skim `sudo nginx -T` from step 1.

   Before the edit, each app's :443 listener should match. After it, the
   only matches may be in `passway/passway-stream.conf`: its `listen 443;` and
   `listen [::]:443;` lines, plus a comment that mentions :443. The stream block
   owns :443, and nginx refuses to start if http also listens there.

5. **Show the owner the full diff against the backup, and wait for an
   explicit yes:**

   ```sh
   sudo diff -ruN "${B:?}" /tmp/nginx.new
   ```

   This changes how the owner's other apps are served, so it is never applied
   without approval. Apply it in an agreed window.

## 5. Apply in place, test, then reload

Only after the owner's yes. The order matters: **no reload until `nginx -t`
passes on the files in their real place.** nginx keeps serving the config it
loaded at its last start or reload, so editing files on disk changes nothing
for visitors until the reload.

First, check that nothing changed the live config since the §3 backup.
certbot renewals, package updates or another admin might have, and the apply
below would silently revert their changes:

```sh
sudo diff -r "${B:?}" /etc/nginx && echo "unchanged since the backup"
```

It must print no differences. If anything differs, stop: start again from §3
with a new backup, redo the edits in §4, and get a fresh yes on the new diff.

Then apply and test:

```sh
sudo rsync -a --delete /tmp/nginx.new/ /etc/nginx/   # the approved edits, in place; no reload
sudo nginx -t                                         # tests the real paths
```

- **If `nginx -t` fails:** put the backup back at once, and confirm that the
  restored files pass again:

  ```sh
  sudo rsync -a --delete "${B:?}"/ /etc/nginx/
  sudo nginx -t                                       # must pass: the old config
  ```

  Don't reload. Fix the problem in `/tmp/nginx.new`, show the owner the new
  diff, and start this section again.

- **If `nginx -t` passes** (and the owner has said yes in §4):

  ```sh
  sudo systemctl reload nginx
  ```

A reload keeps the old workers serving if the new config fails to load.
Moving :443 from http to stream is a listener change, and some setups only
pick it up on a restart. If `ss -ltnp | grep ':443 '` still shows the old
listener after the reload, run `sudo systemctl restart nginx`. That causes
a brief connection drop.

## 6. Test checklist

Run these from the laptop, not the VPS. Each existing app should behave
exactly as before.

- [ ] **Each existing app:** `curl -sI https://<app-host>/` returns the usual
      status and its own certificate (`curl -vI ... 2>&1 | grep subject:`).
      Also try a browser.
- [ ] **Each existing app over plain HTTP:** `curl -sI http://<app-host>/`
      gives the same redirect or page as before.
- [ ] **Passway health:** `curl -s https://<domain>/healthz` prints `ok`.
- [ ] **The dashboard** loads at `https://<domain>`, and GitHub sign-in
      completes.
- [ ] **A tunnel from the laptop:**
      - `passway login <token>`, then `passway http 3000 --server https://<domain>`.
      - The printed `https://<name>.<domain>` URL reaches the local app.
      - The request appears in the inspector at http://127.0.0.1:4040.
- [ ] **ACME issuance:** `journalctl -u passway-server | grep -iE 'certificate|obtain'`
      shows the wildcard obtained without errors. If a custom domain was
      added, its on-demand certificate is obtained on first visit.
- [ ] **Real client IPs, Passway:**
      - Request a tunnel from the laptop. The local app sees your public IP
        in `X-Forwarded-For`, not `127.0.0.1`.
      - Compare with `curl -s https://ifconfig.me`. The inspector shows the
        header too.
- [ ] **Real client IPs, existing apps:** `sudo tail -n 5 /var/log/nginx/access.log`
      (or the app's own log) shows your public IP for your request, not
      `127.0.0.1`.
- [ ] **Unknown hostname:** `curl -skI --resolve nope.example:443:<vps-ip> https://nope.example/`
      fails the TLS handshake. Passway only serves certificates for its own
      names and verified custom domains.
- [ ] **Stripe:** send a test event from the dashboard (Developers →
      Webhooks → the endpoint → Send test event). The delivery shows 200.

## 7. Rollback

If any check fails and the fix isn't obvious within the window, roll back:

```sh
sudo rsync -a --delete "${B:?}"/ /etc/nginx/   # B as set in §3
sudo nginx -t && sudo systemctl restart nginx
```

A restart, not a reload, so that :443 moves back to the http block. Then
re-run the existing-app checks above. Passway can keep running on loopback,
or `sudo systemctl stop passway-server`.

## 8. Nightly backups

```sh
sudo install -d -o passway -g passway -m 0750 /var/lib/passway/backups
sudo install -m 0644 deploy/backup.cron /etc/cron.d/passway
```

- Each night at 03:15, `passway-server admin --db ... backup <dest>` writes a
  consistent copy to `/var/lib/passway/backups/passway-YYYY-MM-DD.db`, and
  local copies older than 14 days are removed.
- `admin backup` refuses an existing destination, so the dated name is
  required.
- Copying the backups off the VPS is up to the owner. Fill in the commented
  rclone line, or use any tool, and test one restore. Stop the service
  first, then remove the `-wal` and `-shm` files next to the database: a WAL
  left by an unclean stop would otherwise be replayed onto the restored file.

  ```sh
  sudo systemctl stop passway-server
  sudo rm -f /var/lib/passway/passway.db-wal /var/lib/passway/passway.db-shm
  sudo -u passway cp /var/lib/passway/backups/passway-<date>.db /var/lib/passway/passway.db
  sudo systemctl start passway-server
  ```

## ACME: how custom-domain certificates are issued

- **Base and tunnel names:** `<domain>` and `*.<domain>` use one wildcard
  certificate over **DNS-01** through Cloudflare. This needs no inbound
  traffic.
- **Custom domains:**
  - These use **on-demand** certificates, issued on the first TLS connection
    for a domain the dashboard has verified. Unverified names are refused, so
    random hostnames can't trigger issuance.
  - Their DNS is not on our Cloudflare account, so they use one of the two
    challenges below.
  - Both work with this layout. Let's Encrypt tries whichever the ACME client
    offers, and certmagic offers both.
  - **HTTP-01 on :80:** Let's Encrypt requests
    `http://<custom-domain>/.well-known/acme-challenge/<token>`. The custom
    domain is unknown to nginx, so the `default_server` in `passway-http.conf`
    must forward that path to 127.0.0.1:8081.
    - If the existing config already has its own `default_server` on :80, add
      the `location /.well-known/acme-challenge/ { proxy_pass http://127.0.0.1:8081; proxy_set_header Host $host; }`
      block to it.
    - Without either, HTTP-01 fails for custom domains.
  - **TLS-ALPN-01 on :443:** Let's Encrypt connects with SNI `<custom-domain>`
    and ALPN `acme-tls/1`.
    - The map sends unknown SNI to Passway, the stream block passes TLS
      through untouched, and Passway answers the challenge itself.
    - This needs no :80 change, so it is the fallback if :80 can't be
      changed.
