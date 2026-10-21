# Passway — Design

*2026-10-21 · formerly "DevTunnel"*

## 1. Intent

Passway is an **open-source, self-hostable ngrok alternative built for webhook
debugging**. One command gives a local app a public HTTPS URL, and a local
inspector shows every request that came through it, with one-click replay.

The business follows the Supabase model: the whole thing is open source, and a
hosted cloud ("Passway Cloud") sells convenience through a free tier plus paid
plans.

**Why (owner's goals):**
- **Reputation.** A well-received open-source launch makes the owner known and
  sends attention to their other SaaS products.
- **Income.** The owner is between jobs, so extra revenue matters. That income
  should be expected over months, not weeks.
- **Learning and portfolio** value, as a side benefit.

**Positioning.** Inspect and replay already exist in closed tools (ngrok,
LocalXpose, Hookdeck CLI, Webhook Relay). zrok is open source but has no webhook
focus. Passway's edge is **open source + self-hostable + a polished webhook
inspector + an honest free tier**. It also has to be a complete general-purpose
tunnel, so ordinary ngrok users can switch.

**First audience:** developers integrating webhooks (Stripe, GitHub, Shopify,
Slack, and similar). Their companies pay, and the feature makes a good demo.

## 2. Goals (launch = end of week 4)

1. `passway http 3000` prints a public `https://<name>.<domain>` URL that
   forwards to `localhost:3000`, including streaming bodies, WebSockets, and
   large uploads.
2. The local inspector at `http://localhost:4040` lists live requests and shows
   each one's full details (method, path, headers, body, response, timing). A
   **Replay** button resends a captured request to the local app, and the user
   can edit it before replaying.
3. Passway Cloud has GitHub sign-in, a dashboard, CLI auth tokens, reserved
   subdomains, custom domains with automatic TLS, Stripe billing, and plan
   limits that are enforced.
4. Self-hosting is one binary or one Docker image plus a short guide.
5. Distribution: prebuilt binaries for Linux, macOS, and Windows, a Homebrew
   tap, an install script, a README with a demo GIF, and a launch post (Show
   HN, r/golang, r/selfhosted, Product Hunt).

## 3. Non-goals (for launch)

- Raw TCP or UDP tunnels. The design should leave room for them (see §6.2), but
  they ship after launch.
- Request history stored on the server, team or org accounts, SSO, multiple
  regions, IP allowlists, and OAuth-protected tunnels. These are paid features
  planned for later.
- Mobile or desktop GUI apps.
- Backward compatibility with the v0.1/v0.2 code or CLI.

## 4. Anti-goals (failure even if it "works")

- **Becoming a phishing host.** If the domain lands on Google Safe Browsing or
  similar blocklists, every user's tunnel breaks. Abuse controls are a launch
  requirement, not polish.
- **A free tier that bleeds money.** Every free account needs hard caps on
  bandwidth, connections, and tunnels.
- **Scope creep that pushes launch past ~4 weeks.** Cut features before
  slipping the date.
- **Closing up the open-source core later.** The trust it builds is the whole
  pitch.

## 5. Constraints

- **Language:** Go for both binaries (the existing codebase, and the owner
  knows Go).
- **Hosting:** the owner's existing VPS, one region at launch. Some budget is
  available for bandwidth and upgrades. **Other apps already run on this VPS
  behind nginx, which owns ports 80 and 443** (see §6.4 for how Passway fits in).
  Specs and the bandwidth allowance are unknown, so the global caps in §6.7
  must be configurable.
- **Deploys:** Claude deploys over SSH, using access the owner will set up.
  Changes to the shared production nginx config are always shown to the owner
  and confirmed before they're applied.
- **Domain:** not bought yet, and it will come **after week 1**. The owner will
  buy one for Passway Cloud, and it needs DNS at a provider with an API for
  wildcard certificates (see §6.4).
- **Payments:** Stripe (available to the owner).
- **Builder:** Claude writes most of the code. The owner reviews a demo at the
  end of each milestone (§9) before work continues.
- **Licenses (my call):** MIT for the CLI, client, and shared protocol package.
  AGPL-3.0 for the server, so a company can't run a closed fork as a competing
  cloud.

## 6. Design

### 6.1 Components

```
                       Internet
                          │  https://acme.passway.dev/webhook
                          ▼
        ┌────────────────────────────────────┐
        │  passway-server  (VPS, :443, :80)  │
        │  ├─ edge: TLS + routes by Host     │
        │  ├─ tunnel registry (in memory)    │
        │  ├─ control endpoint /_tunnel (WS) │
        │  ├─ dashboard + API (html/htmx)    │
        │  ├─ billing (Stripe webhooks)      │
        │  └─ SQLite                         │
        └───────────────▲────────────────────┘
                        │ one outbound TLS WebSocket,
                        │ multiplexed with yamux
        ┌───────────────┴────────────────────┐
        │  passway (CLI on the dev laptop)   │
        │  ├─ tunnel client                  │
        │  ├─ inspector UI  :4040            │
        │  └─ forwards to localhost:3000     │
        └────────────────────────────────────┘
```

**Repo layout (my call):** module `github.com/mohammed19n/passway`.
- `cmd/passway`: the CLI.
- `cmd/passway-server`: the relay, dashboard, and API.
- `internal/protocol`: control messages shared by client and server, versioned.
- `internal/client`, `internal/inspector`, `internal/server/{edge,registry,auth,billing,dashboard,store,abuse}`.
- `web/`: templates and static assets, embedded with `embed`.

### 6.2 Tunnel transport

- **Connection.** The CLI makes one outbound connection to
  `wss://<domain>/_tunnel`, a WebSocket over TLS on port 443. That gets
  through corporate proxies and firewalls that block unusual ports. The
  WebSocket is wrapped as a `net.Conn`, and **yamux** multiplexes many streams
  over it.
- **Handshake.** The first stream carries a JSON `Hello`: protocol version,
  auth token, and the requested subdomain or custom hostname. The server
  replies with `Ready` (the public URL and plan limits) or `Error` (a
  machine-readable code plus a human message). A protocol version mismatch
  produces a clear "please upgrade" message.
- **Per request.** The edge accepts a public request, finds the tunnel by
  `Host`, opens a new yamux stream, and writes the raw HTTP/1.1 request to it.
  The client reads it with `http.ReadRequest`, forwards it to the local target,
  and writes back the raw response. WebSocket and other `Upgrade` requests
  switch to copying bytes in both directions once the upgrade succeeds.
- **Liveness.** yamux keepalives, plus automatic reconnect in the client with
  backoff. A reconnecting client gets the same subdomain back. If no tunnel is
  connected for a known subdomain, the edge returns a branded "tunnel offline"
  page with HTTP 502.
- **Room for TCP later.** The stream header carries a `kind` field (`http`
  now, `tcp` later), so raw TCP tunnels can be added without a protocol break.
- The existing `httputil`-style proxy code is replaced. The client's local
  forwarding should use `net/http/httputil.ReverseProxy` or an equivalent that
  handles hop-by-hop headers, `Host`, and `X-Forwarded-*` correctly.

### 6.3 CLI

```
passway login <token>             # saves the token to the user config dir
passway http 3000                 # account's default subdomain (see below)
passway http 3000 --name acme     # reserved subdomain (must be owned)
passway http 3000 --domain hooks.example.com   # custom domain (must be verified)
passway http localhost:8000 --host-header=rewrite
passway version
```

- **No `--name`, on the cloud (my call):** there are no throwaway random
  subdomains, because disposable names help phishing. The bare command uses
  the account's default subdomain, which is auto-assigned at signup (for
  example `quick-otter-42`). If that one is already in use by another of the
  user's tunnels, it uses the next free reserved subdomain. If none is free,
  it fails with a clear message ("all your subdomains are in use; use --name
  or upgrade"). The handshake carries an optional name, and the server
  resolves it.
- **In self-hosted mode:** a random subdomain is generated if `--name` is
  omitted. It is kept for the client's session, so reconnects keep it.
- **Before accounts exist (weeks 1–2):** the server uses the self-hosted
  behaviour.
- Prints a status block (URL, forwarding target, inspector URL) and then a live
  log, one line per request, with method, path, status, and duration.
- Flags: `--server` (for self-hosters), `--inspect=false`, `--inspect-addr`.
- The config file lives at `os.UserConfigDir()/passway/config.yaml`. The env var
  `PASSWAY_TOKEN` overrides it.

### 6.4 Edge, domains, TLS

- **Platform subdomains:** a wildcard certificate for `*.<domain>`, obtained
  with **certmagic** over a DNS-01 challenge. That requires a DNS provider with
  a libdns plugin (Cloudflare recommended, and free).
- **Custom domains:** the user adds `hooks.example.com` in the dashboard and
  points a CNAME at `<their-subdomain>.<domain>` or a fixed `edge.<domain>`. The
  dashboard checks the CNAME before marking the domain verified. Certificates
  come from certmagic **on-demand TLS**, and its `DecisionFunc` allows only
  verified domains in the DB, so random hostnames can't trigger certificate
  issuance.
- Port 80 redirects to HTTPS, except for ACME challenges.

**Running behind the existing nginx (decided).** nginx keeps owning 80 and 443.
Passway still terminates its own TLS, so certmagic, wildcard certs, and
on-demand certs work unchanged.
- **:443:** nginx's `stream` module with `ssl_preread` routes by SNI. Hostnames
  for the owner's existing apps go to nginx's own `http` block, which moves to
  an internal port such as `127.0.0.1:8443`. Everything else, meaning
  `<domain>`, `*.<domain>`, and any unknown SNI (custom domains), is **TLS
  passthrough** to `passway-server` on `127.0.0.1:9443`. The existing apps
  are the known list, so unknown names default to Passway.
- **Client IPs:** turn on `proxy_protocol` in nginx, and have passway-server
  parse PROXY protocol v2 on its listener (config flag
  `--proxy-protocol`). Rate limits and abuse logs need the real IPs.
- **:80:** nginx `server` blocks for `<domain>`/`*.<domain>` (and a default
  server for unknown hosts) proxy to passway-server's HTTP listener for ACME
  HTTP-01 and the redirects.
- The listen addresses are configurable, so a dedicated box can still bind
  :80/:443 directly.
- Ship a ready-made nginx snippet and a test checklist with the server. Since
  this changes how the owner's other apps are served, it gets applied in a
  scheduled window with a rollback copy of the old config.
- **Risk to watch:** if abuse gets the shared VPS's IP blocklisted, the owner's
  other apps get hit too. Plan to move Passway to its own small VPS once it has
  real traffic. The same binary and config just bind 80/443 directly.
- Reserved hostnames (`www`, `api`, `app`, `edge`, `_tunnel`, `admin`, `status`,
  `docs`, and so on) can't be claimed.

### 6.5 Inspector (local, in the CLI)

- An HTTP server on `127.0.0.1:4040`. It binds only to localhost, never to
  public interfaces.
- An in-memory ring buffer of the last **500** exchanges by default. Bodies are
  capped at **1 MB** each, and larger bodies are marked truncated.
- The UI is a single page embedded in the binary (vanilla JS or htmx, no build
  step). It has a request list that updates live over SSE and a detail pane.
  The detail pane shows headers and body, pretty-prints JSON, form data, and
  XML, shows the response, and shows timing.
- **Replay:** resends the captured request to the local target and adds the
  result as a new entry marked "replay".
- **Edit & replay:** lets the user change the method, path, headers, or body
  first.
- Copy as cURL.
- This is the headline feature and the GIF in the README, so it gets real
  design attention.

### 6.6 Cloud: accounts, dashboard, API

- **Sign-in:** GitHub OAuth only at launch (my call). Developers already have
  accounts, there's no password storage, and a GitHub account raises the bar
  for abuse.
- **Dashboard** (server-rendered `html/template` + htmx, embedded):
  - Getting started: install command plus your `passway login <token>` line
  - Active tunnels: URL, client version, connected-since, request count
  - Reserved subdomains: claim or release, within plan limits
  - Custom domains: add, show DNS instructions, verify status, remove
  - Auth tokens: create, name, revoke. Store only a hash, and show the token once.
  - Billing: current plan, usage this month, upgrade (Stripe Checkout), manage
    (Stripe Customer Portal)
  - Static pages: docs, pricing, terms, privacy, acceptable use, abuse report
- **Storage:** **SQLite** (pure-Go driver, WAL mode) for both cloud and
  self-host (my call; this changes the Postgres mentioned in chat). One VPS
  and one binary make SQLite enough for launch scale. Keep the store behind an
  interface so Postgres can be added later.
- **Tables (rough):** `users`, `tokens`, `subdomains`, `custom_domains`,
  `subscriptions`, `usage_monthly`, `bans`, `audit_log`.

### 6.7 Plans and limits (numbers are my placeholders; the owner sets final prices)

| | Free | Pro (~$6–8/mo) |
|---|---|---|
| Concurrent tunnels | 1 | 5 |
| Reserved subdomains | 1 (stable, auto-assigned on signup) | 5 (choose your own) |
| Custom domains | — | 2 |
| Bandwidth / month | 1 GB | 50 GB |
| Requests / minute per tunnel | 60 | 600 |
| Browser warning page | yes | no |

- The limits are enforced at the edge and in the handshake. Bandwidth is
  counted per tunnel in memory and flushed to `usage_monthly` periodically.
- **Global caps (VPS protection, my call):** configurable limits on total
  free-tier bandwidth per month (default 500 GB) and total free signups
  (default 1,000, with a waitlist after that). Hitting the bandwidth cap stops
  free tunnels only. Paid tunnels keep working, and the admin is alerted at
  80%. The defaults get tuned once the VPS allowance is known.
- When a limit is exceeded, the edge returns a clear error (HTTP 429 or 402
  with a branded page) and the CLI prints a message.
- **Stripe:** Checkout to subscribe and the Customer Portal to manage.
  Webhooks (`checkout.session.completed`, `customer.subscription.updated` and
  `.deleted`, `invoice.payment_failed`) update `subscriptions`. They're
  processed **idempotently** by recording event IDs, and signatures are
  verified.

### 6.8 Abuse controls (launch requirement)

- GitHub sign-in is required, so there are no anonymous tunnels on the cloud.
  New GitHub accounts younger than N days can be restricted (configurable).
- **Free-tier warning page:** a request that looks like browser navigation
  (`Accept: text/html`, no `passway-skip-warning` header) gets a one-time
  warning page ("You are visiting a tunnel run by @user…") with a cookie.
  API and webhook traffic is never interrupted. This is the key anti-phishing
  measure.
- A blocklist of words that subdomains can't contain (`paypal`, `login`,
  `bank`, `secure`, brand names, and so on).
- An abuse report form plus `abuse@<domain>`, and an admin CLI or dashboard
  page to ban a user or hostname instantly, which disconnects live tunnels.
- Per-tunnel rate limits and bandwidth caps (§6.7).
- An `X-Passway-Tunnel` header or log that ties every public request to its
  account, for takedowns.

### 6.9 Self-hosting mode

- `passway-server --self-hosted` (or a config flag) turns off GitHub sign-in,
  billing, plan limits, and the warning page. It uses static tokens from config
  or a simple admin-created token list.
- The self-host guide covers DNS (wildcard A record), the DNS provider token
  for certificates, `docker run` or systemd, and pointing the CLI with
  `--server`.

### 6.10 Operations

- Deploy on the VPS as a systemd service, or with Docker Compose. Back up the
  SQLite DB every night (for example with Litestream or a cron snapshot to
  object storage).
- Structured logs (`log/slog`). A `/healthz` endpoint. Basic metrics (active
  tunnels, requests, bytes) on an admin page.
- Releases through GoReleaser (binaries, checksums, Homebrew tap, Docker
  image) triggered by git tags in GitHub Actions.

## 7. Testing

- Unit tests for protocol encoding, limit accounting, the domain verification
  decision, subdomain validation, and Stripe webhook handling (using recorded
  fixtures).
- **End-to-end test** in `go test`: start the server (self-hosted mode,
  self-signed TLS), a client, and a local HTTP app in-process. Then check
  GET/POST/streaming/WebSocket forwarding, reconnect, offline page, rate-limit
  response, and inspector capture plus replay.
- Run the tests with the race detector in CI.

## 8. Existing code

The current v0.1/v0.2 code (`main.go`, `internal/proxy`, `internal/tunnel`,
`localServer`) is a learning prototype and gets replaced by the layout in §6.1.
The owner's uncommitted v0.3 TCP work stays in git history if they commit it
first. The README gets rewritten for Passway. The owner renames the GitHub repo
`Mohammed19N/devtunnel` to `passway`.

## 9. Milestones (owner reviews a demo at the end of each)

| Week | Milestone | Demo |
|---|---|---|
| 1 | Restructure, protocol, transport, edge with TLS, CLI `http`, reconnect, offline page, e2e test | Runs on this machine: server with a self-signed cert or `*.localhost`, tunnel reaching a local app, e2e test green. (No domain yet, so no public URL.) |
| 2 | Inspector (list, detail, replay, edit, cURL), abuse basics (warning page, blocklist, rate limits) | Send a Stripe test webhook, inspect it, replay it |
| 3 | GitHub sign-in, dashboard, tokens, reserved subdomains, plan limits, self-hosted mode | Sign up, `passway login`, claim a subdomain |
| 4 | Stripe billing, custom domains, GoReleaser + Homebrew, docs/pricing/legal pages, launch assets | Pay for Pro, add a custom domain, install with `brew` |

**First VPS deploy** happens as soon as the domain and Cloudflare token exist,
and at the latest in week 3 (it's needed for GitHub OAuth callbacks and Stripe
webhooks). It includes the nginx SNI change from §6.4. If the domain arrives
later than week 3, launch slips with it.

If a week slips, cut in this order: custom domains (move to v1.1), edit
before replay, Homebrew tap.

## 10. Left to the builder

- Exact library choices beyond those named: WebSocket library, yamux settings,
  config format details, SQLite driver, and migration tool.
- Inspector and dashboard visual design, within "clean, fast, no JS build
  step."
- Error codes, log formats, and internal package boundaries.

## 11. Owner to-dos outside the code

- Buy the domain, put its DNS on Cloudflare (or another provider with a libdns
  plugin), and create an API token. **Needed by week 3 at the latest.**
- Set up SSH access for Claude (host, user, key), and share the current nginx
  config and the list of hostnames the existing apps use.
- Look up the VPS plan's bandwidth allowance, so the global caps can be tuned.
- Create a Stripe account, products, and prices. Set up a GitHub OAuth app.
- Provide or approve the terms, privacy, and acceptable-use text (templates
  are fine at launch).
- Set final prices.
