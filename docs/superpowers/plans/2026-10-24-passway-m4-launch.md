# Passway M4: Billing, Custom Domains, Releases, Public Site, Launch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Passway launchable. This week adds:
- Stripe billing and custom domains with automatic TLS;
- PROXY protocol for the VPS behind nginx;
- release binaries, Homebrew, Docker and install.sh;
- an SEO-ready public site with legal pages and an abuse form;
- an admin page, the deploy kit and the launch assets.

It also applies the owner's rulings from the 2026-10-24 handoff review and the carry-overs from weeks 2–3.

**Architecture:**
- **Billing:** a new `internal/server/billing` package talks to Stripe over plain `net/http` (no SDK) and verifies webhook signatures. The dashboard serves checkout, the portal and the webhook endpoint.
- **Custom domains:** a new store table. The handshake gains an optional `Domain`, and the registry routes on the full hostname for non-base hosts.
- **TLS:** `cmd/passway-server` gains a certmagic mode (wildcard over DNS-01 plus on-demand certificates for verified custom domains) and a PROXY protocol v2 listener wrapper.
- **Public site:** the public pages live in the dashboard package, because they share its layout and are served only in cloud mode.
- **Install script:** a root package `passway` embeds `install.sh` so the server can serve it.

**Tech Stack:**
- Go 1.26 stdlib.
- Two new dependencies: `github.com/caddyserver/certmagic` and `github.com/libdns/cloudflare`. Use the latest tagged versions at implementation time that build together, and record them in go.mod.
- GoReleaser v2 and GitHub Actions.
- Headless Chromium and ffmpeg (both installed locally) for the screenshots, the OG image and the GIF.

**Spec:** `docs/superpowers/specs/2026-10-21-passway-design.md`, read together with the owner rulings below. They also live in the handoff review recorded in memory `passway-m4-followups`.

## Owner rulings (2026-10-24); these override the spec where they differ

- **R1 (W2-5):** Self-hosted servers show no "Served by Passway" footer on edge pages. In cloud mode it shows on pages of Free tunnels and of unknown hosts, but not on Pro tunnels.
- **R2 (W2-8):** Before the first successful connection, the CLI gives up after 2 minutes of failed attempts instead of retrying forever.
- **R3 (W2-13):** Webhook and SaaS brand words are blocked in subdomains. `stripe-hooks` is refused.
- **R4 (W3-2):** The server mode comes from `--mode cloud|self-hosted`, then `PASSWAY_MODE`, and otherwise defaults to `self-hosted`. `--self-hosted` is removed. Our VPS sets `PASSWAY_MODE=cloud` once.
- **R5 (W3-3):** No htmx. Public pages get full SEO, and dashboard pages stay `noindex`.
- **R6 (W3-5):** The `--min-github-age` default is `48h`.

## Global Constraints

- Module `github.com/mohammed19n/passway`.
  - The only new Go dependencies are certmagic and libdns/cloudflare (Task 11).
  - No Stripe SDK, no markdown library and no JS framework.
- Licences: new server code lives under `internal/server/`, which is AGPL. The root `passway` package (install.sh), `internal/proxyproto` and the client stay MIT. Add no licence headers.
- Every migration is a new numbered file in `internal/server/store/migrations/`:
  - `0002_billing.sql`
  - `0003_custom_domains.sql`
  - `0004_abuse.sql`
  - Never edit `0001`.
- The `Plan` struct gains two fields:
  - `MaxCustomDomains int`: unlike the other limits, 0 means none.
  - `Branding bool`: the footer shows.
- The plan values are:
  - **PlanFree:** `MaxCustomDomains 0`, `Branding true`.
  - **PlanPro:** `MaxCustomDomains 2`, `Branding false`.
  - **PlanUnlimited:** `MaxCustomDomains 0`, `Branding false`.
  - All other values are unchanged.
- Secrets come only from the environment:
  - `PASSWAY_GITHUB_CLIENT_SECRET` (as now)
  - `PASSWAY_STRIPE_SECRET_KEY`
  - `PASSWAY_STRIPE_WEBHOOK_SECRET`
  - `PASSWAY_CLOUDFLARE_API_TOKEN`
  - None of them get a flag.
- Billing is enabled only when `--stripe-price-pro`, `PASSWAY_STRIPE_SECRET_KEY` and `PASSWAY_STRIPE_WEBHOOK_SECRET` are all set. Without them, the dashboard keeps the disabled "Coming soon" button, and pricing shows Pro as "Coming soon".
- Prices are never hard-coded. The Pro price label (for example `$7 / month`) is read once at startup from Stripe's `GET /v1/prices/{id}`.
- Copy (exact strings):
  - `"could not reach %s after %s: %v"` (CLI give-up, R2)
  - `"GitHub accounts must be at least 2 days old to sign up"` (produced by the existing formatter at 48h)
  - `"you don't own %q or it isn't verified; add it in the dashboard"` (custom domain not usable)
  - `"tunnel is busy: too many concurrent connections"` (fast 503)
  - `"Payment received — your plan updates in a few seconds."`
- Public-page SEO contract (Task 13). Every public page has:
  - `<html lang="en">`
  - a unique `<title>` of at most 60 characters
  - a `<meta name="description">` of 50–160 characters
  - `<link rel="canonical" href="<BaseURL><path>">`
  - `og:title`, `og:description`, `og:url`, `og:image` (`<BaseURL>/og.png`) and `og:type`
  - `twitter:card=summary_large_image`
  - exactly one `<h1>`
  - no `noindex`

  Every dashboard, auth, billing and admin page keeps `noindex`.
- Commits:
  - No `Co-Authored-By` or any attribution trailer.
  - Commit as `mohammed19n <128026990+Mohammed19N@users.noreply.github.com>`.
  - Push after every commit with `git push -u origin passway-m4`.
- Tests run with `go test -race -count=1 ./...`. Cross-builds must pass: `GOOS=windows go build ./...` and `GOOS=darwin go build ./...`.
- Each task's Step 2 must show RED evidence, pasting the failing output into the report, before implementation.

## Review Focus

1. **Stripe events that are duplicated, out of order or forged.**
   - Stripe retries and reorders.
   - A replayed event must be a no-op.
   - An older `customer.subscription.updated` arriving after a newer `.deleted` must not resurrect Pro.
   - A bad or stale signature (older than 300 s) gets 400 and changes nothing.
   - Tests in Task 7.
2. **Custom-domain takeover.**
   - User A adds `hooks.example.com` but never verifies it. The real owner B must still be able to add it and verify it with their TXT token, and doing so evicts A's pending row.
   - On-demand TLS must refuse any hostname that is not verified with an owner whose plan allows custom domains, so random SNI can't trigger issuance.
   - Tests in Tasks 9 and 11.
3. **Downgrade leakage.**
   - After Pro becomes Free, the account can't keep using extra reserved subdomains, custom domains or more than 1 tunnel.
   - The extras stay reserved for a later re-upgrade, but `Names`/`Owns`/`OwnsDomain` only honour what fits the plan.
   - Live tunnels are disconnected on the plan change so they reconnect under Free rules.
   - Test in Task 8.
4. **A PROXY header from the wrong peer, malformed or slow.**
   - With `--proxy-protocol`, a connection without a valid v2 header is closed. It must never be served with the socket address passed off as a client IP.
   - A peer that sends nothing can't block other accepts.
   - A `LOCAL` command (nginx health check) uses the socket address.
   - Test in Task 10.
5. **Public pages and caches.**
   - A signed-in visitor's page (nav shows their login) must never be cacheable by a shared cache. It must use `Cache-Control: private, no-store` with `Vary: Cookie`.
   - The anonymous abuse form must reject cross-origin posts, a filled honeypot, and more than 5 reports per IP per hour.
   - Test in Task 13.

---

## File Structure

- `cmd/passway-server/main.go`: modified by Tasks 1, 2, 5, 7, 9, 10, 11 and 14 (`--mode`, `--block-words`, billing flags, `--proxy-protocol`, the TLS mode, `--admin-logins` and `version`).
- `cmd/passway-server/admin.go`: arguments are validated before the DB is opened, and the `backup` command is added (Tasks 5 and 14).
- `internal/server/names.go`: the brand list and look-alike normalisation (Task 2).
- `internal/server/{plans.go,pages.go,pages/layout.html,edge.go}`: the footer policy and the fast 503 (Task 3).
- `internal/client/client.go` and `cmd/passway/http.go`: the first-connect give-up (Task 4).
- `internal/server/store/users.go` and `internal/server/dashboard/{github.go,session.go}`: atomic signup, dev-login POST and session replacement (Task 5).
- `internal/inspector/{store.go,api.go,ui/index.html}`: memory budget, decoded bodies, bidi and the logo (Task 6).
- `internal/server/billing/` (new): `stripe.go` and `webhook.go` (Task 7).
- `internal/server/store/{billing.go,migrations/0002_billing.sql}` (Task 7).
- `internal/server/store/directory.go`: the plan-bounded `Names`/`Owns`, plus `OwnsDomain` (Tasks 8 and 9).
- `internal/server/dashboard/billing.go` and `templates/dashboard.html` (Task 8).
- `internal/server/store/{domains.go,migrations/0003_custom_domains.sql}`, `internal/server/dashboard/{domains.go,templates/domains.html}`, `internal/protocol/protocol.go`, `internal/server/{control.go,registry.go,server.go}` and `cmd/passway/http.go` (Task 9).
- `internal/proxyproto/` (new) (Task 10).
- `cmd/passway-server/tls.go` (new) and `internal/server/store/domains.go` (`AllowCert`) (Task 11).
- `install.sh`, `install.go` (root package `passway`), `.goreleaser.yaml`, `.github/workflows/{ci.yml,release.yml}` and `Dockerfile` (Task 12).
- `internal/server/dashboard/{site.go,abuse.go,seo.go,templates/site/*.html,static/{og.png,favicon.svg}}` and `internal/server/store/{abuse.go,migrations/0004_abuse.sql}` (Task 13).
- `internal/server/dashboard/{admin.go,templates/admin.html}` and `internal/server/store/backup.go` (Task 14).
- `deploy/` (new), `docs/deploy.md`, `README.md`, `docs/self-hosting.md`, `docs/launch/*.md`, `docs/img/demo.gif` and `internal/e2e/e2e_test.go` (Task 15).

---

### Task 1: Server mode from flag or environment, and the 48h GitHub age (R4, R6)

**Files:**
- Modify: `cmd/passway-server/main.go`, `cmd/passway-server/main_test.go`, and every test that passes `--self-hosted`
- Modify: `README.md` and `docs/self-hosting.md` (self-hosters no longer pass a mode flag)

**Interfaces:**
- Produces:
  - `--mode string`. Its default comes from `os.Getenv("PASSWAY_MODE")`, else `"self-hosted"`.
  - The accepted values are `cloud` and `self-hosted`. Anything else is the usage error `--mode must be cloud or self-hosted, got %q`.
  - The `--self-hosted` flag is deleted.
  - The cloud-mode missing-flags copy becomes `cloud mode needs --db, --github-client-id and --github-client-secret (or use --mode self-hosted)`.
  - The `--min-github-age` default is `48*time.Hour`.

- [ ] **Step 1: Write the failing tests** in `main_test.go`:
  - `TestModeDefaultSelfHosted`: no flag and no env gives a running self-hosted server on a localhost domain.
  - `TestModeFromEnv`: `t.Setenv("PASSWAY_MODE","cloud")` with no `--db` gives exit 2 and the new copy.
  - `TestModeFlagBeatsEnv`: `PASSWAY_MODE=cloud` plus `--mode self-hosted` runs.
  - `TestModeInvalid`: `--mode hybrid` gives exit 2 and the copy.
  - `TestMinGitHubAgeDefault48h`: the parsed default is `48h`.
  - In `internal/server/dashboard/github_test.go`, the existing young-account test now uses `MinGitHubAge: 48*time.Hour` with `created_at` 1 day ago, and asserts the exact 2-days copy.
- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/passway-server/ ./internal/server/dashboard/`. Expected: FAIL (`--mode` is undefined).
- [ ] **Step 3: Implement.** Replace `selfHosted` with the mode string, and change existing tests from `--self-hosted` to `--mode self-hosted`.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): choose the mode with --mode or PASSWAY_MODE; default GitHub age 48h`

---

### Task 2: Subdomain blocklist — brands, look-alikes, cloud only (R3)

**Files:**
- Modify: `internal/server/names.go`, `internal/server/control.go`, `internal/server/server.go` (Config), `cmd/passway-server/main.go`
- Test: `internal/server/names_test.go`, `internal/server/control_test.go`

**Interfaces:**
- Produces:
  ```go
  func ValidateName(name string) error      // syntax + reserved only (no blocklist)
  func BlockedWord(name string) string      // first blocked word found in any normalised form, or ""
  // Config gains: BlockWords bool  — handshake also rejects names with BlockedWord != "" (CodeInvalidName, existing "contains a blocked word" copy)
  ```
- The dashboard claim path calls `ValidateName` and then `BlockedWord`, always, because the dashboard only exists in cloud mode.
- `main.go` sets `BlockWords = cloud || *blockWords`. The new flag is `--block-words` (bool, self-hosted opt-in, help text "refuse subdomains containing phishing-prone words").
- Words to add to `blockedWords`:
  `stripe github gitlab bitbucket shopify slack twilio discord sendgrid mailgun square plaid hubspot zoom dropbox docusign adobe yahoo icloud steam roblox telegram tiktok twitter linkedin chase wellsfargo citibank hsbc venmo cashapp zelle usps fedex dhl`.
- Normalised forms checked (lowercase input):
  1. the raw name;
  2. the name with `-` removed;
  3. form 2 with the leet map `0→o 3→e 4→a 5→s 7→t 8→b 9→g` and `1→i`;
  4. the same as form 3 but with `1→l`;
  5. forms 3 and 4 with `rn→m` and `vv→w`.

- [ ] **Step 1: Write the failing tests:**
  - `TestBlockedWordLookalikes`: each of these is blocked: `stripe-hooks`, `pay-pal-x`, `paypa1`, `g00gle-sso`, `rnicrosoft`, `vvellsfargo`, `githubhooks`.
  - `TestBlockedWordAllowsOrdinary`: `acme`, `hooks-dev`, `my-shop-2`, `quick-otter-42` are allowed.
  - `TestRandomNamesNeverBlocked`: every adjective×animal pair, with suffixes 0–99, passes `BlockedWord` (iterate the slices directly).
  - `TestSelfHostedBlockWordsOff`: a `Directory == nil` server without `BlockWords` grants `--name paypal-test`.
  - `TestBlockWordsOnRefuses`: `CodeInvalidName`.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above. `RandomName` must keep generating only names that pass.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): block brand words and look-alikes in subdomains (cloud by default)`

---

### Task 3: Edge pages — footer policy (R1) and a fast 503 at the connection cap

**Files:**
- Modify: `internal/server/plans.go`, `pages.go`, `pages/layout.html`, `edge.go`
- Test: `internal/server/plans_test.go`, `pages_test.go`, `edge_test.go`

**Interfaces:**
- Consumes: `Plan.Branding` (Global Constraints).
- Produces:
  - `pageData` gains `Footer bool` and `HomeURL string`.
  - `HomeURL` is `<PublicScheme>://<Domain>[:port]`, which replaces the hard-coded `https://passway.run`.
  - The layout renders the footer only `{{if .Footer}}`.
  - The edge sets `Footer = s.cfg.Directory != nil && (t == nil || t.Account.Plan.Branding)`.
  - `Tunnel` gains `active atomic.Int32`. When `active >= MaxConnsPerTunnel`, the edge returns 503 with `Retry-After: 1`. It renders a new `pageBusy` kind (HTML title "Tunnel busy", plain text from the copy).

- [ ] **Step 1: Write the failing tests:**
  - `TestPlanValues` (extend): `MaxCustomDomains` and `Branding` for all three plans.
  - `TestFooterSelfHostedHidden`: the offline page of a `Directory == nil` server has no `Served by`.
  - `TestFooterCloudFreeShown`: a Free tunnel's warning page has `Served by` linking to `http://<domain>:<port>`.
  - `TestFooterCloudProHidden`: a Pro tunnel's offline or banned page has no footer.
  - `TestFooterCloudUnknownHostShown`
  - `TestEdgeBusyFast503`:
    - with `MaxConnsPerTunnel: 1`, one request is held open by a blocking handler;
    - a second request gets 503 within 200 ms, with `Retry-After: 1` and the copy;
    - after the first finishes, a third gets 200.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/`. Expected: FAIL.
- [ ] **Step 3: Implement.** Increment `active` before proxying, and decrement it in a `defer`, including on hijacked upgrades when the copy ends.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): footer only for Free cloud tunnels; fast 503 when a tunnel is saturated`

---

### Task 4: CLI gives up after 2 minutes before the first connection (R2)

**Files:**
- Modify: `internal/client/client.go`, `cmd/passway/http.go`
- Test: `internal/client/client_test.go`, `cmd/passway/main_test.go`

**Interfaces:**
- Produces:
  ```go
  // Config gains (test override): giveUpAfter time.Duration // 0 = 2*time.Minute
  var ErrGaveUp = errors.New("client: gave up connecting")
  // Run returns fmt.Errorf("%w: "+copy, ErrGaveUp, server, giveUpAfter, lastErr) when no Ready
  // was ever received and time since Run started >= giveUpAfter at the moment a retry would be scheduled.
  ```
- After the first `Ready`, reconnects stay unlimited. Laptop sleep and server deploys must recover on their own. Say so in a comment.
- The CLI prints `error: could not reach <server> after 2m0s: <err>` and exits 1.

- [ ] **Step 1: Write the failing tests:**
  - `TestRunGivesUpBeforeFirstConnect`: a server always answering 503, with `giveUpAfter: 300ms` and `initialBackoff: 20ms`, makes `Run` return `ErrGaveUp` within 2 s, and the message contains the server URL.
  - `TestRunKeepsReconnectingAfterReady`: the server sends Ready, closes, and then answers 503. With `giveUpAfter: 300ms`, `Run` is still running after 1 s (cancel ctx and expect nil).
  - `TestRunHTTPGaveUpMessage` (cmd): the stderr line matches the copy and the exit code is 1.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/client/ ./cmd/passway/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(client): stop retrying after 2 minutes if the server was never reached`

---

### Task 5: Account hygiene carry-overs

**Files:**
- Modify: `internal/server/store/users.go`, `internal/server/dashboard/{github.go,session.go,dashboard.go}`, `templates/` (dev-login form), `cmd/passway-server/admin.go`, `internal/server/store/store.go`, `internal/e2e/e2e_test.go` (dev-login helper)
- Modify: `docs/superpowers/notes/2026-10-23-weeks-2-3-handoff.md`, demo commands (dev login is now a form)

**Interfaces:**
- Produces:
  ```go
  // store
  // SignupGitHubUser upserts u; on creation, in the same IMMEDIATE transaction, it waitlists the
  // user when admitted Free users >= freeCap. Concurrent signups never exceed freeCap.
  func (s *Store) SignupGitHubUser(ctx context.Context, u User, freeCap int) (User, bool, error)
  func OpenExisting(path string) (*Store, error) // error "no database at <path>" if the file is missing
  ```
- **Dev login:**
  - `GET /auth/dev` renders a form with a `login` field.
  - `POST /auth/dev` signs in. It requires `Origin` or `Referer` equal to the BaseURL; if both are missing, it returns 403.
  - The old `GET /auth/dev?login=X` no longer signs in.
- **Sign-in** (GitHub callback or dev): if the request carries a session cookie that resolves, call `DeleteSession` on it before creating the new one.
- **Admin:**
  - Every subcommand validates its arguments before opening the DB.
  - Every command except `token create` uses `OpenExisting`. `token create` may create the DB, which is the self-hosted first run.

- [ ] **Step 1: Write the failing tests:**
  - `TestSignupCapConcurrent` (store): cap 5, 20 goroutines with distinct GitHub IDs; exactly 5 users are not waitlisted.
  - `TestSignupWaitlistedNotCounted`: cap 1; A is admitted; B and C are waitlisted; `CountUsers("free") == 1`.
  - `TestDevLoginGetDoesNotSignIn`: GET with `?login=x` gives no `Set-Cookie` for the session.
  - `TestDevLoginPostCrossOriginRefused`: `Origin: http://evil.localhost` gives 403.
  - `TestSignInReplacesOldSession`: sign in twice with the same jar; the first session ID returns `ErrNotFound` from `SessionByID`.
  - `TestAdminUsageErrorCreatesNoFile`: `admin --db <tmp>/typo.db token create` with no name gives exit 2, and the file does not exist.
  - `TestAdminMissingDBError`: `admin --db <tmp>/none.db whois x` gives exit 1 with `no database at`.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/... ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement.** The callback uses `SignupGitHubUser` instead of its own count-then-upsert.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `fix(dashboard): atomic signup cap, POST-only dev login, replace session on sign-in; admin creates no stray DB`

---

### Task 6: Inspector carry-overs — memory budget, decoded bodies, bidi, logo

**Files:**
- Modify: `internal/inspector/{store.go,api.go,ui/index.html}`, `internal/server/pages/layout.html` (only if the mark differs), `internal/server/dashboard/templates/layout.html`
- Test: `internal/inspector/store_test.go`, `api_test.go`

**Interfaces:**
- Produces:
  ```go
  const DefaultMaxTotalBytes = 64 << 20 // sum of request+response body bytes kept; oldest evicted first
  func NewStore(capacity int, maxTotal int64) *Store // existing callers pass DefaultMaxTotalBytes
  // Body JSON gains: Decoded []byte `json:"decoded,omitempty"`; DecodedTruncated bool `json:"decoded_truncated,omitempty"`
  // set when Content-Encoding is gzip or deflate; decompressed up to MaxBody; invalid data leaves Decoded empty.
  ```
- The UI shows `Decoded` when it is present, with a small "decoded from gzip" tag. Replay and cURL always use the raw `Data`.
- UI: add a JS function `revealControls(s)`. It replaces U+200B–U+200F, U+202A–U+202E, U+2066–U+2069 and U+FEFF with the visible marker `⟨U+XXXX⟩`. Apply it to the path, header names and values, and the text bodies wherever they are rendered.
- The logo: one mark everywhere. The canonical mark is the edge/dashboard SVG (rounded square `#5b5bf0` with the white path and dot). Replace the inspector's `mark` icon with it.

- [ ] **Step 1: Write the failing tests:**
  - `TestStoreEvictsByBytes`: maxTotal 3 MiB; add four exchanges of 1 MiB each; the first is gone and `List` has 3.
  - `TestStoreSingleHugeExchangeKept`: one exchange bigger than the budget is kept alone, never an empty store.
  - `TestAPIDecodesGzip`: a gzip response body gives `decoded` equal to the plain text, and `data` is unchanged.
  - `TestAPIInvalidGzipNoDecoded`
  - `TestAPIDecodedCapped`: a 5 MiB gzip bomb gives `decoded` of 1 MiB and `decoded_truncated` true.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/inspector/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Verify** with `go test -race -count=1 ./...` (PASS). Then run a headless-Chromium script in the scratchpad: send a request with path `/a%E2%80%AEb` and a gzip JSON response, and screenshot the inspector. Expected: the path shows `⟨U+202E⟩`, the body shows pretty JSON with the "decoded from gzip" tag, and the header mark matches the dashboard's.
- [ ] **Step 5: Commit and push** `feat(inspector): memory budget, decoded gzip bodies, visible bidi controls, one logo`

---

### Task 7: Billing core — Stripe client, webhook verification, subscription store

**Files:**
- Create: `internal/server/billing/{stripe.go,webhook.go}`, `internal/server/store/{billing.go,migrations/0002_billing.sql}`
- Test: `internal/server/billing/{stripe_test.go,webhook_test.go}` and `testdata/*.json` (recorded-shape fixtures: the Stripe objects with only the fields used), `internal/server/store/billing_test.go`

**Interfaces:**
- Produces (package `billing`):
  ```go
  type Client struct { Key string; BaseURL string /* default https://api.stripe.com */; HTTP *http.Client }
  type Price struct { ID, Currency, Interval string; UnitAmount int64 }
  func (p Price) Label() string // "$7 / month" for usd; otherwise "7.00 EUR / month"
  func (c *Client) Price(ctx context.Context, id string) (Price, error)
  type CheckoutParams struct { PriceID, CustomerID, ClientReferenceID, SuccessURL, CancelURL string } // CustomerID optional
  func (c *Client) CheckoutURL(ctx context.Context, p CheckoutParams) (string, error)  // POST /v1/checkout/sessions mode=subscription
  func (c *Client) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) // POST /v1/billing_portal/sessions
  type Event struct { ID, Type string; Created int64; Data json.RawMessage /* data.object */ }
  var ErrBadSignature = errors.New("billing: bad signature")
  // VerifyWebhook checks the Stripe-Signature header (t=…, one or more v1=…): HMAC-SHA256 of "<t>.<payload>"
  // with secret, constant-time, |now - t| <= 300s.
  func VerifyWebhook(payload []byte, header, secret string, now time.Time) (Event, error)
  ```
  - Requests are form-encoded, with `Authorization: Bearer <Key>`.
  - Stripe error JSON (`error.message`) becomes the error text. The key is never logged.
- Produces (package `store`; `0002_billing.sql` creates the tables below):
  - `subscriptions(user_id PRIMARY KEY REFERENCES users, customer_id UNIQUE, subscription_id, status, event_created INTEGER, updated_at)`
  - `stripe_events(id PRIMARY KEY, type, received_at)`
  ```go
  type Subscription struct { UserID int64; CustomerID, SubscriptionID, Status string; EventCreated int64 }
  func (s *Store) SubscriptionByUser(ctx context.Context, userID int64) (Subscription, error)
  // ApplyStripeEvent runs in one IMMEDIATE tx: if eventID was seen → (false, nil). Otherwise it records the id
  // and calls apply(tx-scoped *BillingTx). Returns applied=true.
  func (s *Store) ApplyStripeEvent(ctx context.Context, eventID, typ string, apply func(*BillingTx) error) (bool, error)
  type BillingTx struct{ /* tx */ }
  func (b *BillingTx) UserByCustomer(customerID string) (int64, error)       // ErrNotFound
  func (b *BillingTx) Subscription(userID int64) (Subscription, error)       // ErrNotFound
  func (b *BillingTx) PutSubscription(sub Subscription) error                // upsert
  func (b *BillingTx) SetPlan(userID int64, plan string) error               // also un-waitlists when plan == "pro" (same admit path as `admin admit`, incl. default subdomain)
  ```
- Event rules live in `billing.Apply(ctx, st *store.Store, ev Event) (planChanged []int64, err error)`:
  - **`checkout.session.completed`** with `mode=subscription` and `payment_status=paid`: `client_reference_id` is the user ID. Store the customer and subscription with status `active`, then `SetPlan pro`.
  - **`customer.subscription.created` / `.updated` / `.deleted`:**
    - Find the user by `customer`. If the customer is unknown, return nil and log it.
    - If `ev.Created < stored.EventCreated`, ignore the event (stale).
    - The status is `active`, `trialing` or `past_due` → `pro`. Any other status, or `.deleted`, → `free`.
  - **`invoice.payment_failed`:** an `Audit` entry only.
  - **Other types:** applied (recorded) with no effect.
- `planChanged` lists the user IDs whose plan actually changed.

- [ ] **Step 1: Write the failing tests:**
  - `TestVerifyWebhookValid`: sign a fixture with the test secret.
  - `TestVerifyWebhookBadSig`, `TestVerifyWebhookStale` (301 s), `TestVerifyWebhookMultipleV1` (the second matches).
  - `TestPriceLabel`: usd 700 month gives `$7 / month`; eur 1250 gives `12.50 EUR / month`.
  - `TestCheckoutURLRequest`: a fake Stripe server asserts the form fields `mode=subscription`, `line_items[0][price]`, `line_items[0][quantity]=1`, `client_reference_id`, `success_url`, `cancel_url` and the bearer header, and returns `{"url":"https://checkout.stripe.com/x"}`.
  - `TestPortalURLRequest`
  - `TestStripeErrorMessage`
  - `TestApplyCheckoutMakesPro`: a waitlisted user becomes pro, is not waitlisted, and has a default subdomain.
  - `TestApplyDuplicateEventNoop`: the same ID twice; the second reports applied=false (Review Focus 1).
  - `TestApplyOutOfOrder`: `.deleted` (created 200) and then `.updated` active (created 100); the plan stays free (Review Focus 1).
  - `TestApplyPastDueKeepsPro`
  - `TestApplyUnknownCustomerNoError`
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/billing/ ./internal/server/store/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above. `billing` imports `store`; `store` must not import `billing`.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(billing): Stripe checkout, portal and idempotent, ordered webhooks`

---

### Task 8: Billing in the dashboard, and plan-bounded resources on downgrade

**Files:**
- Create: `internal/server/dashboard/billing.go`
- Modify: `internal/server/dashboard/{dashboard.go,pages.go,templates/dashboard.html}`, `internal/server/store/directory.go`, `cmd/passway-server/main.go`
- Test: `internal/server/dashboard/billing_test.go`, `internal/server/store/directory_test.go`, `cmd/passway-server/main_test.go`

**Interfaces:**
- Consumes: Task 7.
- Produces:
  ```go
  // dashboard.Config gains:
  Billing *billing.Client // nil = billing off
  ProPrice billing.Price  // fetched at startup by main
  PriceID, WebhookSecret string
  PlanChanged func(userID int64) // main: srv.CloseAccountTunnels(strconv.FormatInt(id,10))
  // server
  func (s *Server) CloseAccountTunnels(accountID string)
  ```
- Routes:
  - **`POST /billing/checkout`** (CSRF): 303 to `CheckoutURL`, with:
    - `ClientReferenceID` set to the user ID;
    - `CustomerID` set to the existing customer, if any;
    - `SuccessURL` = `<BaseURL>/dashboard?upgraded=1`;
    - `CancelURL` = `<BaseURL>/dashboard`.
  - **`POST /billing/portal`** (CSRF): 303 to `PortalURL`. Users without a customer get 404.
  - **`POST /stripe/webhook`:**
    - no session, CSRF or origin checks;
    - body limited to 1 MiB;
    - `VerifyWebhook` then `billing.Apply`; `PlanChanged` is called for each changed user;
    - bad signature gives 400, store errors 500, otherwise 200.
- The dashboard Plan section shows, when billing is on:
  - Free: "Upgrade to Pro — <ProPrice.Label()>".
  - Pro with a customer: "Manage billing".
  - `?upgraded=1` shows the "Payment received" copy.
- CSP `form-action` gains `https://checkout.stripe.com https://billing.stripe.com`.
- `main.go` flag: `--stripe-price-pro`. With all three billing inputs present, startup calls `Price` and fails fast if it errors. With only some present, it is a usage error naming the missing ones.
- **Plan-bounded directory** (Review Focus 3):
  - `Names` returns at most `plan.MaxSubdomains` names (0 = all), default first and then oldest.
  - `Owns(name)` is true only for names in that set.
- [ ] **Step 1: Write the failing tests:**
  - `TestCheckoutRedirect` (fake Stripe)
  - `TestPortalRequiresCustomer`
  - `TestWebhookBadSignature400`
  - `TestWebhookUpgradeCallsPlanChanged`
  - `TestCheckoutRequiresCSRF`
  - `TestUpgradeButtonHiddenWithoutBilling`
  - `TestCSPAllowsStripeFormAction`
  - `TestDowngradeBoundsNames` (store):
    - a Pro user has a default plus `acme` and `beta`;
    - after SetPlan free, `Names` is `[default]` and `Owns("acme")` is false;
    - after SetPlan pro again, both are back.
  - `TestBillingFlagsPartial` (cmd): only `--stripe-price-pro` gives exit 2, naming both env vars.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/... ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(dashboard): upgrade and manage billing; downgrades keep extras reserved but unusable`

---

### Task 9: Custom domains

**Files:**
- Create: `internal/server/store/{domains.go,migrations/0003_custom_domains.sql}`, `internal/server/dashboard/{domains.go,templates/domains.html}`
- Modify: `internal/protocol/protocol.go`, `internal/server/{directory.go,control.go,registry.go,server.go}`, `internal/server/store/directory.go`, `internal/client/client.go`, `cmd/passway/http.go`
- Test: `internal/server/store/domains_test.go`, `internal/server/dashboard/domains_test.go`, `internal/server/control_test.go`, `internal/server/edge_test.go`, `cmd/passway/main_test.go`

**Interfaces:**
- `0003_custom_domains.sql`:
  - Creates `custom_domains(host, user_id REFERENCES users ON DELETE CASCADE, token, verified_at, created_at, PRIMARY KEY(host, user_id))`.
  - Adds `CREATE UNIQUE INDEX custom_domains_verified ON custom_domains(host) WHERE verified_at IS NOT NULL`.
- Produces:
  ```go
  // protocol: Hello gains Domain string `json:"domain,omitempty"` (Version stays 1)
  // store
  type CustomDomain struct { Host string; UserID int64; Token string; VerifiedAt *time.Time; CreatedAt time.Time }
  func (s *Store) AddDomain(ctx context.Context, userID int64, host string, max int) (CustomDomain, error) // ErrTaken if verified by someone else, ErrLimit
  func (s *Store) VerifyDomain(ctx context.Context, userID int64, host string) error  // sets verified_at, deletes other users' pending rows for host (one tx)
  func (s *Store) RemoveDomain(ctx context.Context, userID int64, host string) error
  func (s *Store) Domains(ctx context.Context, userID int64) ([]CustomDomain, error)
  // server.Directory gains:
  OwnsDomain(ctx context.Context, acc Account, host string) (bool, error) // verified, and within plan.MaxCustomDomains (oldest verified first)
  // dashboard
  type Resolver interface { LookupCNAME(ctx context.Context, host string) (string, error); LookupTXT(ctx context.Context, name string) ([]string, error) }
  // Config gains Resolver Resolver (nil = net.DefaultResolver); EdgeHost string (default "edge.<base host>")
  // CLI: passway http 3000 --domain hooks.example.com ; client.Config gains Domain string
  ```
- **Host validation** (`AddDomain` is called after the dashboard validates):
  - lowercase, trailing dot removed;
  - at most 253 characters and at least one dot;
  - each label matches `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`;
  - not an IP address;
  - not the base domain and not under it.
- **The token** is 16 random bytes, hex-encoded.
- **Verification** (`POST /dashboard/domains/verify`) passes when both hold:
  - the CNAME of the host, with the trailing dot trimmed, equals `EdgeHost`;
  - `_passway.<host>` TXT contains `passway-verify=<token>`.

  On failure, the page names which check failed.
- **Domains page:** each domain shows the two DNS records to create and its status.
  - Claiming is limited by `plan.MaxCustomDomains`. On Free, the page says "Custom domains are a Pro feature".
- **Handshake:**
  - `hello.Domain` set together with `hello.Name` gives `CodeInvalidName` (`"use either --name or --domain"`).
  - `Domain` without `OwnsDomain` gives `CodeNameNotOwned` with the copy.
  - Otherwise the registry claims the key `hello.Domain`; full hostnames can't collide with labels. Ready.URL is `<scheme>://<host>[:port]`.
  - With `Directory == nil`, `Domain` gives `CodeInvalidName` (`"custom domains need cloud mode"`).
- **Edge lookup:** a host under the base domain uses the label, as now. Any other host looks up the full lowercase host without a port.

- [ ] **Step 1: Write the failing tests:**
  - `TestAddDomainPendingDoesNotBlockOwner`: A adds, B adds and B verifies, so A's row is gone (Review Focus 2).
  - `TestAddDomainVerifiedTaken`: after B verifies, C adding gives `ErrTaken`.
  - `TestAddDomainLimit`
  - `TestDomainValidation`: refused are `1.2.3.4`, `x.<base>`, `localhost`, `-a.com` and a label of 64 characters.
  - `TestVerifyNeedsBothRecords`: a fake resolver with the CNAME only fails and names the TXT check.
  - `TestDomainsPageFree`: shows the Pro copy and the claim form is absent.
  - `TestHandshakeCustomDomain`: a Pro account with a verified domain gets Ready with the URL on the custom host.
  - `TestHandshakeDomainNotOwned`
  - `TestHandshakeDomainAndName`
  - `TestHandshakeDomainSelfHosted`
  - `TestEdgeRoutesCustomHost`: a request with `Host: hooks.example.com` reaches the tunnel.
  - `TestDowngradeDisablesDomain`: after free, `OwnsDomain` is false (Review Focus 3).
  - `TestRunHTTPDomainFlag` (cmd): `--domain` reaches the Hello.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/... ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat: custom domains with CNAME+TXT verification and --domain`

---

### Task 10: PROXY protocol v2 listener

**Files:**
- Create: `internal/proxyproto/{proxyproto.go,proxyproto_test.go}`
- Modify: `cmd/passway-server/main.go`

**Interfaces:**
- Produces:
  ```go
  // NewListener wraps ln so each accepted conn must start with a PROXY v2 header.
  // The header is parsed lazily on the first Read or RemoteAddr call (in the
  // connection's own goroutine, never in Accept), with a read deadline of timeout.
  func NewListener(ln net.Listener, timeout time.Duration) net.Listener
  var ErrNoHeader = errors.New("proxyproto: missing or invalid PROXY v2 header")
  ```
- **Parsing:**
  - The 12-byte signature `\r\n\r\n\x00\r\nQUIT\n`.
  - The version must be 2.
  - Command `LOCAL` keeps the socket address. Command `PROXY` with `AF_INET` or `AF_INET6` over `STREAM` uses the source address. `UNSPEC` keeps the socket address.
  - TLVs are skipped.
  - A declared length over 4096 is invalid.
  - On any error, the conn is closed, Read returns `ErrNoHeader`, and `RemoteAddr` returns the socket address.
- **Flag** `--proxy-protocol` (bool) wraps only the TLS listener's raw TCP listener, before `tls.NewListener`. The plain :80 listener sits behind nginx `http` and is never wrapped.

- [ ] **Step 1: Write the failing tests:**
  - `TestProxyV2IPv4`: RemoteAddr is `203.0.113.7:4321` and the payload after the header reads intact.
  - `TestProxyV2IPv6`
  - `TestProxyV2Local`: the socket address.
  - `TestProxyV2TLVsSkipped`
  - `TestNoHeaderClosed`: a client that sends `GET / HTTP/1.1` gets `ErrNoHeader` from Read on the server side (Review Focus 4).
  - `TestProxyV1Rejected`: `PROXY TCP4 …` is invalid.
  - `TestSlowPeerDoesNotBlockAccept`: one conn sends nothing; a second conn with a valid header is accepted and served while the first is pending.
  - `TestHeaderTimeout`: 100 ms with nothing sent gives `ErrNoHeader`.
  - `TestServeHTTPWithProxy` (cmd/passway-server): `--proxy-protocol` with TLS; a raw client writes a v2 header, then a TLS handshake and `GET /healthz`, and gets 200.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/proxyproto/ ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement** with `encoding/binary` and stdlib only.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): PROXY protocol v2 on the TLS listener`

---

### Task 11: Automatic TLS with certmagic

**Files:**
- Create: `cmd/passway-server/tls.go`, `cmd/passway-server/tls_test.go`
- Modify: `cmd/passway-server/main.go`, `internal/server/store/domains.go`, `go.mod`, `go.sum`

**Interfaces:**
- Produces:
  ```go
  // store
  // AllowCert reports whether an on-demand certificate may be issued for host:
  // a verified custom domain whose owner's plan has MaxCustomDomains > 0 and isn't banned.
  func (s *Store) AllowCert(ctx context.Context, host string) error // nil = allow; error explains refusal
  // cmd/passway-server
  type acmeOpts struct{ email, dir, ca string; cloudflareToken string }
  func newCertManager(ctx context.Context, domain string, o acmeOpts, st *store.Store, log *slog.Logger) (*tls.Config, http.Handler /* wraps HTTP-01 */, error)
  ```
- **Flags:**
  - `--tls-mode` takes `files` (the default, as today with `--tls-cert`/`--tls-key`) or `acme`.
  - `--acme` mode requires `--acme-email` and `PASSWAY_CLOUDFLARE_API_TOKEN`.
  - `--acme-dir` defaults to `<dir of --db>/certs`, or `./certs` when there is no `--db`.
  - `--acme-ca` defaults to Let's Encrypt production. The docs mention the staging URL for dry runs.
- **certmagic setup:**
  - Manage `<domain>` and `*.<domain>` up front via DNS-01, using a libdns Cloudflare provider.
  - The on-demand `DecisionFunc` calls `st.AllowCert`. It is enabled only in cloud mode with a store.
  - The `:80` listener serves the HTTP-01 handler wrapping the existing redirect handler.
- `ReadHeaderTimeout` and the other listener settings are unchanged.

- [ ] **Step 1: Write the failing tests:**
  - `TestAllowCertVerifiedPro` (store): allowed.
  - `TestAllowCertUnverified`, `TestAllowCertFreeOwner`, `TestAllowCertUnknownHost`, `TestAllowCertBannedOwner`: refused (Review Focus 2).
  - `TestACMEFlagsRequired` (cmd): `--tls-mode acme` without an email gives exit 2. Without the token it also gives exit 2, and the message names the env var.
  - `TestNewCertManagerDecision`: build with a fake store, call the config's on-demand decision for an unknown host, and get an error, without contacting any CA.
- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/passway-server/ ./internal/server/store/`. Expected: FAIL.
- [ ] **Step 3: Implement.** Run `go get` for certmagic and libdns/cloudflare. If the latest versions disagree on the libdns interface, pick the newest pair that compiles and note it in the report.
- [ ] **Step 4: Run** `go test -race -count=1 ./...` and the cross-builds. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): automatic TLS — wildcard via Cloudflare DNS-01, on-demand for verified custom domains`

---

### Task 12: Release pipeline — install.sh, GoReleaser, CI, Docker, Homebrew

**Files:**
- Create:
  - `install.sh`
  - `install.go` (package `passway`: `//go:embed install.sh` `var InstallScript []byte`)
  - `install_test.go`
  - `.goreleaser.yaml`
  - `.github/workflows/ci.yml`
  - `.github/workflows/release.yml`
  - `Dockerfile`
- Modify: `cmd/passway-server/main.go` (a `version` subcommand, and `var version = "dev"`)

**Interfaces:**
- **Release assets:**
  - CLI: `passway_<os>_<arch>.tar.gz`, or `.zip` on windows.
  - Server: `passway-server_<os>_<arch>.tar.gz`.
  - Checksums: `checksums.txt`.
  - There is no version in the archive names, so `releases/latest/download/` works.
  - Targets are linux, darwin and windows on amd64 and arm64, built with `CGO_ENABLED=0`.
  - ldflags: `-s -w -X main.version={{.Version}} -X main.defaultServer={{ .Env.PASSWAY_DEFAULT_SERVER }}` (the CLI only has `defaultServer`).
- **`install.sh`** (POSIX sh, `set -eu`):
  - Detect `uname -s` (Linux, Darwin; anything else exits 1 with a hint to download from the releases page) and `uname -m` (`x86_64→amd64`, `aarch64|arm64→arm64`).
  - Download from `${PASSWAY_DOWNLOAD_URL:-https://github.com/mohammed19n/passway/releases/latest/download}`, or from `…/download/v$PASSWAY_VERSION` when `PASSWAY_VERSION` is set.
  - Verify the archive's sha256 against `checksums.txt` using `sha256sum` or `shasum -a 256`. On a mismatch, exit 1 and install nothing.
  - Install into `${PASSWAY_INSTALL_DIR}`, else `/usr/local/bin` if it is writable, else `$HOME/.local/bin` (print a PATH hint if it isn't on PATH).
  - Print `passway <version> installed to <path>` and then `next: passway login <token>`.
- **GoReleaser** (`version: 2`):
  - **Homebrew:** use the current non-deprecated Homebrew section of GoReleaser v2. `goreleaser check` must report no deprecations.
    - Tap repository `mohammed19n/homebrew-tap`.
    - Token `{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}`.
    - On macOS, remove the quarantine attribute after install, because the binaries are unsigned.
  - **Docker:** image `ghcr.io/mohammed19n/passway-server`, tags `{{.Version}}` and `latest`, platforms linux/amd64 and linux/arm64.
  - **Dockerfile:**
    - Base image `gcr.io/distroless/static-debian12:nonroot`.
    - Copies only `passway-server`.
    - `ENV PASSWAY_MODE=self-hosted`.
    - `VOLUME /data`.
    - `ENTRYPOINT ["/passway-server"]`.
- **`ci.yml`:**
  - Runs on push and pull_request.
  - Steps: `go vet ./...`, `go test -race -count=1 ./...`, and cross-builds for windows and darwin.
  - Uses the Go version from go.mod.
- **`release.yml`:**
  - Runs on tags `v*`.
  - Permissions: `contents: write`, `packages: write`.
  - Uses `goreleaser/goreleaser-action` with `~> v2` and `args: release --clean`.
  - Env:
    - `GITHUB_TOKEN`
    - `HOMEBREW_TAP_GITHUB_TOKEN` (secret)
    - `PASSWAY_DEFAULT_SERVER` (repository variable)
  - Has a docker login step to ghcr.

- [ ] **Step 1: Write the failing tests** in `install_test.go` (skipped on windows). Each test serves a fake release from `httptest` containing a tarball with a stub `passway` script that echoes a version, plus `checksums.txt`, and runs `sh install.sh` with `PASSWAY_DOWNLOAD_URL` and `PASSWAY_INSTALL_DIR=t.TempDir()`:
  - `TestInstallScriptInstalls`: the file exists, is executable, and stdout has `installed to`.
  - `TestInstallScriptBadChecksum`: non-zero exit and no file.
  - `TestInstallScriptPinnedVersion`: `PASSWAY_VERSION=1.2.3` requests `/download/v1.2.3/…`.
  - `TestInstallScriptEmbedded`: `InstallScript` equals the file on disk.
  - `TestServerVersion` (cmd/passway-server): `passway-server version` prints `passway-server dev`.
- [ ] **Step 2: Run** `go test -race -count=1 . ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement** all files. Run `shellcheck install.sh` if it is available, otherwise `sh -n install.sh`.
- [ ] **Step 4: Verify:**
  - `go test -race -count=1 ./...` passes.
  - `go run github.com/goreleaser/goreleaser/v2@latest check` prints no errors or deprecations.
  - `go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish,docker` produces the expected archive names in `dist/`. `dist/` stays gitignored.
- [ ] **Step 5: Commit and push** `build: install script, GoReleaser (binaries, Homebrew, Docker) and CI workflows`

---

### Task 13: Public site with SEO, legal pages and the abuse form

**Files:**
- Create:
  - `internal/server/dashboard/{site.go,seo.go,abuse.go}`
  - `templates/site/{home.html,pricing.html,docs.html,docs_webhooks.html,terms.html,privacy.html,aup.html,abuse.html}`
  - `static/{og.png,favicon.svg}`
  - `internal/server/store/{abuse.go,migrations/0004_abuse.sql}`
- Modify: `internal/server/dashboard/{dashboard.go,templates/layout.html,static/app.css}`
- Test: `internal/server/dashboard/{site_test.go,seo_test.go,abuse_test.go}`, `internal/server/store/abuse_test.go`

**Interfaces:**
- Consumes: `passway.InstallScript` (Task 12), `ProPrice` and billing-on (Task 8), and `server.PlanFree` / `server.PlanPro` for the limits on the pricing page.
- `0004_abuse.sql` creates `abuse_reports(id, url, category, details, email, ip, created_at, resolved_at)`.
- Produces:
  ```go
  type AbuseReport struct { ID int64; URL, Category, Details, Email, IP string; CreatedAt time.Time; ResolvedAt *time.Time }
  func (s *Store) AddAbuseReport(ctx context.Context, r AbuseReport) (int64, error)
  func (s *Store) AbuseReports(ctx context.Context, unresolvedOnly bool, limit int) ([]AbuseReport, error)
  func (s *Store) ResolveAbuseReport(ctx context.Context, id int64) error
  // dashboard.Config gains: AbuseEmail string (default "abuse@<base host>"); RepoURL string (default "https://github.com/mohammed19n/passway")
  type seo struct { Title, Description, Path string; JSONLD template.JS /* built from Go structs via json.Marshal, never user text */ }
  ```
- **Routes** (all public, no session required):

  | Path | Notes |
  |---|---|
  | `GET /` | Landing page. Signed-in users see "Open dashboard" instead of "Sign in", with **no redirect** |
  | `GET /pricing` | |
  | `GET /docs` | Quickstart, CLI reference, links to self-hosting on GitHub |
  | `GET /docs/webhooks` | Stripe, GitHub and Shopify recipes with inspector and replay |
  | `GET /terms`, `GET /privacy`, `GET /aup` | |
  | `GET /abuse`, `POST /abuse` | |
  | `GET /sitemap.xml` | |
  | `GET /robots.txt` | |
  | `GET /install.sh` | |
  | `GET /og.png` | |
  | `GET /favicon.svg` | |

- **Content:**
  - The landing page has: a hero ("Public URLs for localhost, with a webhook inspector built in"), the install command, the inspector screenshot (a committed copy of `docs/img/inspector.png` at `static/inspector.png`; embed can't reach outside the package), a three-point feature list, an open-source/self-host section and a Free vs Pro summary.
  - **The legal pages are templates.** They are marked at the top with an HTML comment `<!-- OWNER: review before launch -->` and use `{{.Operator}}` / `{{.Jurisdiction}}` placeholders (`Config.LegalOperator` and `Config.LegalJurisdiction`, flags `--legal-operator` and `--legal-jurisdiction`; empty values render "the operator of this service").
- **SEO** follows the Global Constraints contract on every route above, except `robots.txt`, `sitemap.xml`, `install.sh` and the images.
  - **JSON-LD:**
    - The landing page has `SoftwareApplication` (name, `applicationCategory: DeveloperApplication`, operatingSystem `Linux, macOS, Windows`, an `offers` Free price 0 and, when billing is on, a Pro offer from `ProPrice`, `license` the repo URL).
    - The docs pages have `TechArticle`.
  - **`sitemap.xml`** lists every public HTML route with absolute URLs.
  - **`robots.txt`:**
    - `Disallow:` for `/dashboard`, `/login`, `/logout`, `/auth/`, `/billing/`, `/stripe/`, `/waitlist` and `/admin`.
    - `Sitemap: <BaseURL>/sitemap.xml`.
- **Caching and speed:**
  - A request without the session cookie gets `Cache-Control: public, max-age=300` and `Vary: Cookie`.
  - A request with the session cookie gets `Cache-Control: private, no-store` and `Vary: Cookie` (Review Focus 5).
  - Static assets are served at `/static/app.css?v=<first 8 hex of sha256>` with `Cache-Control: public, max-age=31536000, immutable`.
  - Gzip is applied to html, css, xml and txt responses when the client sends `Accept-Encoding` containing gzip.
- **`/install.sh`:** `Content-Type: text/plain; charset=utf-8`, `Cache-Control: public, max-age=300`.
- **Abuse form** (`POST /abuse`):
  - Fields: `url` (required, must be http(s)), `category` (one of `phishing malware spam other`), `details` (required, at most 5000 characters), `email` (optional), and a honeypot `website` that must be empty.
  - Origin/Referer must equal the BaseURL.
  - Limited to 5 per IP per hour with the existing `RateLimiter` keyed by `abuse:<ip>`, using `r.RemoteAddr`, which Task 10 makes real behind nginx.
  - On success, it logs a WARN `abuse report` and 303s to `/abuse?sent=1`.
  - A filled honeypot gives a silent 303 with nothing stored.
- **`og.png`:** 1200×630. Render it from an HTML card (logo, "Passway", the tagline) with headless Chromium `--screenshot --window-size=1200,630` and commit the PNG.
- The footer of every site and dashboard page links Docs, Pricing, Terms, Privacy, Acceptable use, Report abuse and GitHub.

- [ ] **Step 1: Write the failing tests:**
  - `TestPublicPagesSEO`: one table over every public HTML route, parsed with `golang.org/x/net/html` only if it is already in go.sum, otherwise with regexes. It asserts every SEO contract item, including unique titles across pages and no `noindex`.
  - `TestDashboardPagesNoindex`
  - `TestHomeSignedInNoRedirect`: a session gives 200, contains "Open dashboard", and has `private, no-store`.
  - `TestHomeAnonymousCacheable`
  - `TestSitemapListsPublicRoutes`
  - `TestRobotsDisallows`
  - `TestJSONLDValid`: extract the script and `json.Unmarshal` it. The Pro offer is present only with billing on.
  - `TestPricingShowsPlanLimits`: the numbers come from `PlanFree` and `PlanPro`.
  - `TestInstallScriptServed`
  - `TestGzip`
  - `TestStaticImmutable`
  - `TestAbuseReportStored`
  - `TestAbuseCrossOrigin403`
  - `TestAbuseHoneypot`
  - `TestAbuseRateLimit`: the 6th report gives 429.
  - `TestAbuseInvalidURL`
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/...`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above. Write the copy plainly: no superlatives, and no claims about competitors that we can't verify.
- [ ] **Step 4: Verify:**
  - `go test -race -count=1 ./...` passes.
  - Start a `--dev-login` server and take headless-Chromium screenshots of `/`, `/pricing`, `/docs` and `/abuse` at 1280 px and 390 px into the scratchpad. Check that there is no horizontal scroll at 390 px.
  - If `npx lighthouse` runs offline, record the SEO score; the target is 100.
- [ ] **Step 5: Commit and push** `feat(site): public pages with SEO, legal templates, abuse form, install.sh`

---

### Task 14: Admin page and backups

**Files:**
- Create: `internal/server/dashboard/{admin.go,templates/admin.html}`, `internal/server/store/backup.go`
- Modify: `internal/server/server.go` (Stats), `cmd/passway-server/{main.go,admin.go}`
- Test: `internal/server/dashboard/admin_test.go`, `internal/server/store/backup_test.go`, `cmd/passway-server/admin_test.go`

**Interfaces:**
- Produces:
  ```go
  // server
  type Stats struct { Tunnels int; ByPlan map[string]int; Requests int64 }
  func (s *Server) Stats() Stats
  // dashboard.Config gains: Admins []string (GitHub logins, case-insensitive); Stats func() AdminStats; PollBans func()
  type AdminStats struct { Tunnels int; ByPlan map[string]int; Requests int64 }
  // store
  func (s *Store) Backup(ctx context.Context, dest string) error // VACUUM INTO; error if dest exists
  ```
- **Flag** `--admin-logins` (comma-separated).
- **`GET /admin`** (session required, and the user must be in `Admins`, otherwise 404). It shows:
  - live tunnels, in total and by plan, and total requests;
  - users by plan and the waitlist count;
  - this month's bandwidth for Free and Pro, with the Free cap percentage;
  - unresolved abuse reports. Each has "Ban host" (label parsed from the report URL), "Ban user" (resolved via the subdomain owner) and "Resolve" buttons.
- **`POST /admin/ban`** and **`POST /admin/resolve`:** CSRF-checked and admin-only. They call `AddBan` and then `PollBans()`, which closes live tunnels immediately, and write an `Audit` entry.
- **CLI** `passway-server admin --db PATH backup <dest>` prints `backup written to <dest>`.

- [ ] **Step 1: Write the failing tests:**
  - `TestAdminPageNonAdmin404`
  - `TestAdminPageShowsStats`
  - `TestAdminBanFromReport`: a ban row exists, `PollBans` was called and the report stays unresolved.
  - `TestAdminPostsRequireCSRF`
  - `TestBackupRoundTrip`: open the backup and find the users.
  - `TestBackupRefusesExisting`
  - `TestAdminBackupCLI`
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/... ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(admin): metrics and abuse queue page; database backup command`

---

### Task 15: Deploy kit, end-to-end, README and launch assets, demo dry-run

**Files:**
- Create:
  - `deploy/nginx/passway-stream.conf`
  - `deploy/nginx/passway-http.conf`
  - `deploy/systemd/passway-server.service`
  - `deploy/passway.env.example`
  - `deploy/compose.yaml`
  - `deploy/backup.cron`
  - `docs/deploy.md`
  - `docs/launch/{show-hn.md,reddit-golang.md,reddit-selfhosted.md,product-hunt.md}`
  - `docs/img/demo.gif`
- Modify: `internal/e2e/e2e_test.go`, `README.md`, `docs/self-hosting.md`

**Interfaces:**
- Consumes: everything above. The e2e `opts` gains:
  - `billing *fakeStripe`, an httptest Stripe that serves checkout and signs webhooks with a test secret;
  - `resolver`, a fake `dashboard.Resolver`.
- **nginx stream config:**
  - `map $ssl_preread_server_name $passway_upstream` lists the owner's existing hostnames → `127.0.0.1:8443`, with `default` → `127.0.0.1:9443`.
  - `proxy_protocol on` is set only for the Passway upstream, which needs a separate `server` block per upstream. Comment this.
- **systemd unit:**
  - `User=passway`, `StateDirectory=passway`, `EnvironmentFile=/etc/passway/env`, `Environment=PASSWAY_MODE=cloud`.
  - Hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ReadWritePaths=/var/lib/passway`, `PrivateTmp`, `ProtectHome`.
  - `ExecStart` uses `--tls-mode acme --proxy-protocol --tls-addr 127.0.0.1:9443 --addr 127.0.0.1:8081 --public-port 443 --db /var/lib/passway/passway.db`.
- **`docs/deploy.md`** is the owner-gated runbook:
  1. Prerequisites: domain on Cloudflare, API token, GitHub OAuth app, Stripe products and webhook endpoint `<BaseURL>/stripe/webhook` with the four events from spec §6.7, SSH access, the list of existing nginx hostnames.
  2. Back up `/etc/nginx` to `/etc/nginx.bak-<date>`.
  3. Show the owner the diff and get approval (spec §5).
  4. Run `nginx -t`, then reload.
  5. Test checklist: each existing app with `curl -I`, `https://<domain>/healthz`, a tunnel from the laptop, the `acme` certificate issuance log, the real client IP in logs.
  6. Rollback steps.
  7. Nightly backup via `backup.cron` (`admin backup` to a dated file, then the owner's off-site copy).
- **End-to-end tests:**
  - `TestCloudUpgradeViaStripe`:
    - A Free user signs in;
    - POST checkout gives a fake Stripe URL;
    - post a signed `checkout.session.completed` to `/stripe/webhook`;
    - the user is Pro and a second tunnel is accepted;
    - post `customer.subscription.deleted`, newer;
    - the live tunnels close, and on reconnect the second client gets `limit_reached`.
  - `TestCloudCustomDomain`:
    - Pro;
    - add `hooks.example.test`;
    - the fake resolver answers;
    - verify;
    - `client.Run` with `Domain` gets Ready;
    - a request with that Host reaches the app.
  - `TestSelfHostedNoFooterNoBlocklist`
- **README:**
  - The hero with `docs/img/demo.gif`.
  - The cloud quickstart (`curl -fsSL <domain>/install.sh | sh` and `brew install mohammed19n/tap/passway`).
  - Self-hosting with Docker and the binary (no mode flag).
  - Features.
  - Plans, linking to the pricing page.
  - Licences.
- **`demo.gif`:**
  - Script the inspector in headless Chromium. It shows a Stripe-style webhook arriving, the detail view and a replay.
  - Take frames with `--screenshot`, then run `ffmpeg -framerate 2 … -vf "scale=960:-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse"`.
  - The result must be at most 5 MB. It replaces `docs/img/inspector.png` in the README, and the PNG stays for the site.
- **Launch drafts:** short, factual and in the first person, written for the owner to edit and post. Show HN needs a title of at most 80 characters and a first comment explaining the why.

- [ ] **Step 1: Write the failing e2e tests** above.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/e2e/`. Expected: FAIL until the wiring is in. Fix behaviour bugs in the owning package, with a unit test.
- [ ] **Step 3: Write** the deploy kit, README, GIF and launch drafts. Validate the nginx files if Docker is available, with `docker run --rm -v $PWD/deploy/nginx:/etc/nginx/conf.d:ro nginx:1.27 nginx -t` and a wrapper `nginx.conf` that includes the stream block. Otherwise note that this check is pending for the VPS.
- [ ] **Step 4: Verify:**
  - `go vet ./... && go test -race -count=1 ./...` and the cross-builds pass.
  - Run the headless demo dry-run with the fake Stripe (`--dev-login` on localhost):
    1. sign in;
    2. upgrade through the fake checkout plus a signed webhook (a small Go program in the scratchpad);
    3. add and verify a custom domain via a test resolver build tag, or skip this step and say so;
    4. run `install.sh` against a snapshot `dist/` served locally;
    5. screenshot each step.
  - The real Stripe test-mode run and `brew install` wait for the owner's accounts.
- [ ] **Step 5: Commit and push** `docs: deploy kit, README with demo GIF, launch drafts; e2e for billing and custom domains`

---

## Deferred and owner-gated

- **The first VPS deploy follows `docs/deploy.md`.** It happens in a session with the owner after these:
  - the domain;
  - the Cloudflare token;
  - SSH access;
  - the nginx hostnames;
  - approval of the nginx diff.
- **Owner inputs:**
  - Stripe account, Pro product and price, webhook secret.
  - GitHub OAuth app.
  - The `homebrew-tap` repo and a `HOMEBREW_TAP_GITHUB_TOKEN` secret.
  - The `PASSWAY_DEFAULT_SERVER` repository variable.
  - Renaming the GitHub repo `devtunnel` to `passway` (spec §8; install.sh and GoReleaser assume the new name).
  - Legal operator name and jurisdiction, and approval of the legal text.
  - The Public Suffix List submission.
  - Posting the launch drafts.
- **If the week slips, cut in this order (spec §9):**
  1. custom domains: Task 9 and the on-demand half of Task 11 move to v1.1;
  2. the Homebrew section of Task 12.
- **After launch:** htmx polish, email for abuse reports, server-side request history, TCP tunnels.
