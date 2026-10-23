# Passway M3: Accounts, Dashboard, Plan Limits, Self-Hosted Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the relay into Passway Cloud's core. It gets GitHub sign-in, a server-rendered dashboard, hashed CLI tokens, reserved subdomains, and plan limits enforced in the handshake and at the edge. An explicit `--self-hosted` mode keeps today's behaviour. The M2 carry-over (warning-cookie bypass) is fixed first.

**Architecture:** A new `internal/server/store` package owns SQLite: it uses the pure-Go `modernc.org/sqlite` driver in WAL mode, with embedded numbered SQL migrations. `internal/server` gains an `Account`/`Plan` model and a `Directory` interface for resolving names and limits; the store implements it. A nil `Directory` means self-hosted mode, so every existing server test keeps passing unchanged. A new `internal/server/dashboard` package serves GitHub OAuth, sessions and the dashboard on the base domain with `html/template`. It uses plain HTML forms with POST-redirect-GET, and no JS is needed. `cmd/passway-server` gains `--self-hosted`, the cloud flags, and an `admin` subcommand that works on the database directly.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `html/template`, `embed`, `crypto/*`), `modernc.org/sqlite` (the only new dependency), and the existing `coder/websocket` and `hashicorp/yamux`. GitHub OAuth is hand-rolled over `net/http`, with no oauth2 library.

**Spec:** `docs/superpowers/specs/2026-10-21-passway-design.md` (§6.3, §6.6, §6.7, §6.8, §6.9, §7, §9 week 3). Carry-overs come from the M2 final review (memory `passway-m3-followups`).

## Global Constraints

- Module `github.com/mohammed19n/passway`. The only new dependency is `modernc.org/sqlite v1.57.0`. Do not add oauth2 or session libraries, and do not add htmx or any JS framework.
- Licensing: new packages live under `internal/server/`, so the existing AGPL LICENSE there covers them. `internal/protocol` and `internal/client` stay MIT. Add no license headers.
- SQLite is opened with `_pragma=journal_mode(WAL)`, `_pragma=busy_timeout(5000)` and `_pragma=foreign_keys(1)`, using `db.SetMaxOpenConns(1)` for writes. Storage is behind the `store.Store` methods, so Postgres can be added later.
- Plans (spec §6.7; the owner sets final numbers later, so keep them in one table in `internal/server/plans.go`):
  - **Free:** 1 concurrent tunnel, 1 reserved subdomain, bandwidth `1 << 30` bytes per month, 60 requests per minute per tunnel, browser warning on.
  - **Pro:** 5 concurrent tunnels, 5 reserved subdomains, bandwidth `50 << 30` bytes per month, 600 requests per minute, warning off.
  - **Unlimited** (self-hosted and static tokens): 0 on every limit means unlimited. The warning follows `--browser-warning`.
- Global caps (spec §6.7):
  - `--free-bandwidth-cap` defaults to `500 << 30` bytes per month across all Free accounts. When hit, it stops Free tunnels only, and it logs a `WARN` once at 80%.
  - `--free-signup-cap` defaults to `1000` Free accounts. After that, new signups are waitlisted.
- Tokens:
  - Format is `pw_` + 32 random bytes, base64url-encoded without padding.
  - Only `sha256(token)` is stored, along with a display prefix (the first 10 characters).
  - A token is shown once, on the page that created it.
- Dashboard session cookie:
  - `__Host-passway_session` when the public scheme is https, otherwise `passway_session`.
  - Attributes: `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` on https, Max-Age 30 days, and no `Domain`.
  - The DB stores sha256 of the session ID.
  - Every POST form carries a per-session CSRF token, checked with `hmac.Equal`.
- New protocol error codes: `limit_reached`, `name_not_owned`, `banned`. All three are fatal in `protocol.IsFatal`. `Ready` gains optional `plan`, `rate_limit` and `bandwidth` fields. `protocol.Version` stays `1`, because the new fields are additive.
- Copy (exact strings):
  - `"all your subdomains are in use; use --name or upgrade"`
  - `"tunnel limit reached: your %s plan allows %d concurrent tunnel(s)"`
  - `"you don't own %q; claim it in the dashboard"`
  - `"account suspended"`
  - `"monthly bandwidth limit reached"`
- Self-hosted keeps the current behaviour: random names, the `--tokens` list, no dashboard, and the warning only via `--browser-warning`.
- Commits must have no `Co-Authored-By` or any attribution trailer. Push after each commit (`git push origin passway-m3`).
- Tests run with `go test -race -count=1 ./...`.

## Review Focus

1. **Cookie tossing from a tunnel subdomain.** A tunnel's app at `x.<domain>` can set cookies with `Domain=<domain>`. It must not be able to fix or forge a dashboard session or a CSRF token: the `__Host-` prefix on https and CSRF bound to the server-side session. Test in Task 6.
2. **Token and name races.** The same token can connect twice at once with different session IDs, or two accounts can claim the same subdomain at the same moment. Exactly one must win, and the counts must stay correct. The registry account count is under the registry lock, and subdomain claims rely on the `PRIMARY KEY`. Tests in Tasks 2 and 4.
3. **Reconnect after a server restart or a dropped connection.** A Free user's client reconnects with the same session ID while the old tunnel is still registered. It must take over its own tunnel, not hit `limit_reached`. Test in Task 4.
4. **OAuth callback abuse.** A missing or mismatched `state`, a replayed `code`, a GitHub error response, or a GitHub account younger than `--min-github-age` must give a clear page. It must never create a session. Tests in Task 6.
5. **Ban takes effect on live tunnels.** Banning a user or host through the admin CLI, which is a separate process, closes that user's live tunnels within 15 s, and they can't reconnect. Test in Task 5 (poller) and Task 8 (CLI).

---

## File Structure

- `internal/server/warning.go`: Task 1 fixes `dropWarnSetCookie`.
- `internal/protocol/protocol.go`: new codes and the new `Ready` fields (Task 3).
- `internal/server/plans.go` (new): `Plan`, `PlanFree`, `PlanPro`, `PlanUnlimited`, `PlanByName` (Task 3).
- `internal/server/auth.go`: `Account`, and `Authenticator` returning an `Account` (Task 3).
- `internal/server/directory.go` (new): the `Directory` interface consumed by the handshake, edge and dashboard (Task 3).
- `internal/server/store/` (new):
  - `store.go`: Open, migrations, Close.
  - `migrations/0001_init.sql`
  - `users.go`, `tokens.go`, `subdomains.go`, `sessions.go`, `usage.go`, `bans.go`, `settings.go`
  - `directory.go`: implements `server.Directory`.
  - (Task 2 writes everything above except `directory.go`, which is Task 3.)
- `internal/server/control.go` and `registry.go`: account-aware claim, tunnel limit and Ready plan info (Task 4).
- `internal/server/edge.go`, `ratelimit.go`, `meter.go` (new), `pages.go`, `pages/layout.html`: per-plan rate limit, warning with owner, bandwidth metering, 402 page, and the ban poller in `server.go` (Task 5).
- `internal/server/dashboard/` (new):
  - `dashboard.go`: Handler and routes.
  - `github.go`: OAuth.
  - `session.go`: cookie, CSRF, dev login.
  - `pages.go`: page handlers.
  - `templates/*.html`
  - `static/app.css`
  - (Tasks 6 and 7.)
- `internal/server/server.go`: base-domain routing to the dashboard (Task 6).
- `cmd/passway-server/main.go`, `admin.go` (new): mode flags and admin subcommands (Task 8).
- `cmd/passway/http.go`, `main.go`: Plan line in the status block, plus a friendly message for each fatal code (Task 4).
- `internal/e2e/e2e_test.go`, `README.md`, `docs/self-hosting.md` (new): Task 9.

---

### Task 1: Close the nameless warned-cookie bypass

**Files:**
- Modify: `internal/server/warning.go` (`dropWarnSetCookie`)
- Test: `internal/server/warning_test.go`

**Interfaces:**
- Produces: no new API. Behaviour change: an upstream `Set-Cookie` is dropped when its first pair's trimmed, lowercased name is `passway_warned`, or the name is empty and the value's own `name=` part is `passway_warned` (exactly the Step 3 rule).

- [ ] **Step 1: Write the failing test** `TestDropWarnSetCookieNameless`. It is a table over `dropWarnSetCookie`, expecting kept or dropped:
  - `=passway_warned=abc; Path=/` is dropped.
  - ` =passway_warned=abc` is dropped.
  - `Passway_Warned=abc` is dropped.
  - `passway_warned=abc; Domain=localhost` is dropped.
  - `session=1; Path=/` is kept.
  - `=other` (nameless but unrelated) is kept.
  - A header with two values, one bad and one good, keeps only the good one.
- [ ] **Step 2: Run** `go test -race -run TestDropWarnSetCookieNameless ./internal/server/`. Expected: FAIL on the first three rows.
- [ ] **Step 3: Implement.** Parse the first pair with `strings.Cut(v, ";")` and then `strings.Cut(pair, "=")`. Lowercase and trim the name. Drop the cookie if `name == warnCookie`, or if `name == ""` and the value's own name part, read with `strings.Cut(value, "=")`, equals `warnCookie`.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/server/`. Expected: PASS.
- [ ] **Step 5: Commit and push** `fix(server): drop nameless Set-Cookie that would plant the warned cookie`

---

### Task 2: SQLite store

**Files:**
- Create: `internal/server/store/{store.go,users.go,tokens.go,subdomains.go,sessions.go,usage.go,bans.go,settings.go,migrations/0001_init.sql}`
- Test: `internal/server/store/*_test.go` (one per file; every test uses `Open(filepath.Join(t.TempDir(),"p.db"))`)
- Modify: `go.mod`, `go.sum` (`go get modernc.org/sqlite@v1.57.0`)

**Interfaces:**
- Produces (package `store`; times are `time.Time` UTC; `ErrNotFound`, `ErrTaken`, `ErrLimit` and `ErrDefault` are sentinel errors):
  ```go
  func Open(path string) (*Store, error) // runs pending migrations
  func (s *Store) Close() error

  type User struct {
      ID int64; GitHubID int64; Login, Name, AvatarURL string
      Plan string // "free" | "pro"
      Waitlisted bool; BannedAt *time.Time; CreatedAt time.Time
  }
  // UpsertGitHubUser inserts or updates by GitHubID (login/name/avatar refreshed; plan and waitlist kept).
  func (s *Store) UpsertGitHubUser(ctx context.Context, u User) (User, bool /*created*/, error)
  func (s *Store) UserByID(ctx context.Context, id int64) (User, error)
  func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) // case-insensitive
  func (s *Store) SetPlan(ctx context.Context, userID int64, plan string) error
  func (s *Store) SetWaitlisted(ctx context.Context, userID int64, w bool) error
  func (s *Store) CountUsers(ctx context.Context, plan string) (int, error) // excludes waitlisted

  type Token struct { ID, UserID int64; Name, Prefix string; CreatedAt time.Time; LastUsedAt, RevokedAt *time.Time }
  // CreateToken returns the plaintext once; userID 0 = self-hosted admin token (user_id NULL).
  func (s *Store) CreateToken(ctx context.Context, userID int64, name string) (plain string, t Token, err error)
  func (s *Store) TokenByPlain(ctx context.Context, plain string) (Token, error) // ErrNotFound if unknown or revoked; touches last_used_at
  func (s *Store) ListTokens(ctx context.Context, userID int64) ([]Token, error) // 0 = admin tokens
  func (s *Store) RevokeToken(ctx context.Context, userID, tokenID int64) error // ErrNotFound if not that user's

  type Subdomain struct { Name string; UserID int64; IsDefault bool; CreatedAt time.Time }
  // ClaimSubdomain fails with ErrTaken if anyone holds name, ErrLimit if the user already has max (max 0 = unlimited).
  func (s *Store) ClaimSubdomain(ctx context.Context, userID int64, name string, isDefault bool, max int) error
  func (s *Store) ReleaseSubdomain(ctx context.Context, userID int64, name string) error // ErrDefault for the default subdomain; ErrNotFound if not the user's
  func (s *Store) Subdomains(ctx context.Context, userID int64) ([]Subdomain, error) // default first, then created_at
  func (s *Store) SubdomainOwner(ctx context.Context, name string) (int64, error) // ErrNotFound

  type Session struct { UserID int64; CSRF string; ExpiresAt time.Time }
  func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (id string, sess Session, err error) // id: 32 random bytes base64url
  func (s *Store) SessionByID(ctx context.Context, id string) (Session, error) // ErrNotFound if unknown/expired
  func (s *Store) DeleteSession(ctx context.Context, id string) error

  func (s *Store) AddUsage(ctx context.Context, userID int64, month string, bytes int64) error // month "2006-01", upsert-add
  func (s *Store) Usage(ctx context.Context, userID int64, month string) (int64, error)
  func (s *Store) PlanUsage(ctx context.Context, plan, month string) (int64, error) // sum over users on plan

  type Ban struct { Kind, Value, Reason string; CreatedAt time.Time } // Kind "user" (login) | "host" (subdomain)
  func (s *Store) AddBan(ctx context.Context, b Ban) error
  func (s *Store) RemoveBan(ctx context.Context, kind, value string) error
  func (s *Store) Bans(ctx context.Context) ([]Ban, error)
  func (s *Store) IsBanned(ctx context.Context, kind, value string) (bool, error)

  func (s *Store) Audit(ctx context.Context, userID int64, action, detail string) error
  // Setting returns the value for key, creating it with gen() on first use (atomic).
  func (s *Store) Setting(ctx context.Context, key string, gen func() []byte) ([]byte, error)
  ```
- `0001_init.sql` creates:
  - `schema_migrations(version)`
  - `users(id, github_id UNIQUE, login, name, avatar_url, plan DEFAULT 'free', waitlisted, banned_at, created_at)`, with a `UNIQUE INDEX` on `lower(login)`
  - `tokens(id, user_id NULL REFERENCES users ON DELETE CASCADE, name, hash BLOB UNIQUE, prefix, created_at, last_used_at, revoked_at)`
  - `subdomains(name PRIMARY KEY, user_id NOT NULL REFERENCES users, is_default, created_at)`
  - `sessions(id_hash PRIMARY KEY, user_id, csrf, created_at, expires_at)`
  - `usage_monthly(user_id, month, bytes, PRIMARY KEY(user_id, month))`
  - `bans(kind, value, reason, created_at, PRIMARY KEY(kind, value))`
  - `audit_log(id, user_id, action, detail, created_at)`
  - `settings(key PRIMARY KEY, value BLOB)`
  - `custom_domains` and `subscriptions` come in week 4 as `0002`.

- [ ] **Step 1: Write the failing tests:**
  - `TestOpenMigratesOnce`: opening twice keeps `schema_migrations` at 1 row.
  - `TestOpenWAL`: `PRAGMA journal_mode` is `wal`.
  - `TestUpsertGitHubUser`: created is true then false; login is refreshed; plan is kept.
  - `TestTokenLifecycle`:
    - The plaintext matches `^pw_[A-Za-z0-9_-]{43}$`.
    - `TokenByPlain` finds it.
    - After `RevokeToken`, the lookup returns `ErrNotFound`.
    - The DB contains no plaintext; scan the `tokens` table for the string.
    - Revoking another user's token returns `ErrNotFound`.
  - `TestClaimSubdomainRace`: 20 goroutines with distinct users claim `"acme"` and exactly 1 succeeds; the rest get `ErrTaken`.
  - `TestClaimSubdomainLimit`: with max=1 the second claim gets `ErrLimit`; with max=0 it is unlimited.
  - `TestReleaseDefaultRefused`
  - `TestSessionExpiry`: with ttl=-1s, `SessionByID` returns `ErrNotFound`. The stored `id_hash` is not equal to the id.
  - `TestUsageAdd`: 100 plus 50 gives 150. `PlanUsage("free", m)` sums 2 users.
  - `TestBans`: add, `IsBanned` is true, remove, then false.
  - `TestSettingStable`: a second call returns the same bytes and `gen` is called once.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/store/`. Expected: FAIL (the package doesn't exist yet).
- [ ] **Step 3: Implement.** Migrations are files embedded with `embed.FS`, applied in a transaction in lexical order, and recorded in `schema_migrations`. Token and session hashes use `sha256.Sum256`. `ClaimSubdomain` runs in one transaction: count the user's subdomains, then INSERT, mapping a UNIQUE constraint failure to `ErrTaken`.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(store): SQLite store for users, tokens, subdomains, sessions, usage, bans`

---

### Task 3: Accounts, plans and the Directory interface

**Files:**
- Create: `internal/server/plans.go`, `internal/server/directory.go`, `internal/server/store/directory.go`
- Modify: `internal/server/auth.go`, `internal/protocol/protocol.go`, `internal/server/control.go` (call-site only), and every `Authenticate` caller and test
- Test: `internal/server/plans_test.go`, `internal/server/store/directory_test.go`, `internal/protocol/protocol_test.go`

**Interfaces:**
- Produces:
  ```go
  // protocol
  const CodeLimitReached = "limit_reached"; const CodeNameNotOwned = "name_not_owned"; const CodeBanned = "banned"
  type Ready struct { URL, Name string; Plan string `json:"plan,omitempty"`; RateLimit int `json:"rate_limit,omitempty"`; Bandwidth int64 `json:"bandwidth,omitempty"` }

  // server
  type Plan struct { Name string; MaxTunnels, MaxSubdomains, RateLimit int; Bandwidth int64; Warning bool }
  var PlanFree, PlanPro, PlanUnlimited Plan // values from Global Constraints; PlanUnlimited.Name = "unlimited"
  func PlanByName(name string) Plan // unknown -> PlanFree
  type Account struct { ID string; Login string; Plan Plan } // ID: decimal user id, or "static"/"anonymous"
  type Authenticator interface { Authenticate(ctx context.Context, token string) (Account, error) }
  // StaticTokens now returns Account{ID:"static"|"anonymous", Plan: PlanUnlimited}.

  // Directory is the cloud account directory; nil Config.Directory = self-hosted.
  type Directory interface {
      Authenticator
      // Names returns the subdomains acc may use, default first.
      Names(ctx context.Context, acc Account) ([]string, error)
      // Owns reports whether acc owns name.
      Owns(ctx context.Context, acc Account, name string) (bool, error)
      // Banned reports whether the account or host label is banned.
      Banned(ctx context.Context, acc Account, label string) (bool, error)
      // AddUsage records bytes for acc this month; Usage returns this month's total.
      AddUsage(ctx context.Context, acc Account, bytes int64) error
      Usage(ctx context.Context, acc Account) (int64, error)
      PlanUsage(ctx context.Context, plan string) (int64, error)
      // BannedLists lists bans for the poller: user logins and host labels.
      BannedLists(ctx context.Context) (users, hosts []string, err error)
  }
  // Config gains: Directory Directory; FreeBandwidthCap int64 (0 = 500<<30 when Directory != nil).

  // store
  type Directory struct{ S *Store; Now func() time.Time } // implements server.Directory
  ```
  - A token unknown to `store.Directory.Authenticate` returns an error. A user who is banned or waitlisted returns a `*protocol.Error` with `CodeBanned` (`"account suspended"`) or `CodeUnauthorized` (`"your account is on the waitlist"`).
  - Use a month key of `Now().UTC().Format("2006-01")`.
- `IsFatal` adds the three new codes.

- [ ] **Step 1: Write the failing tests:**
  - `TestPlanValues`: assert every field of `PlanFree` and `PlanPro` against Global Constraints.
  - `TestPlanByNameUnknown`
  - `TestStaticTokensAccount`
  - `TestIsFatalNewCodes`
  - `TestReadyOmitsEmptyPlan`: `json.Marshal(Ready{URL:"u",Name:"n"})` has no `"plan"` key.
  - `TestDirectoryAuthenticate` (store): valid token gives the Account with the plan from the user; revoked token errors; banned user gives `CodeBanned`; waitlisted user gives the waitlist error.
  - `TestDirectoryNamesDefaultFirst`
  - `TestDirectoryBannedHost`
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/protocol/ ./internal/server/...`. Expected: FAIL.
- [ ] **Step 3: Implement.** Change the `Authenticator` signature and update `control.go` so it uses only `acc` (self-hosted behaviour unchanged). No limits are applied yet.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): accounts, plans and the account directory`

---

### Task 4: Cloud handshake — names, tunnel limits, Ready plan info, CLI messages

**Files:**
- Modify: `internal/server/control.go`, `internal/server/registry.go`, `cmd/passway/http.go`
- Test: `internal/server/control_test.go`, `internal/server/registry_test.go`, `cmd/passway/main_test.go`

**Interfaces:**
- Consumes: `Directory`, `Account`, `Plan` from Task 3.
- Produces:
  ```go
  // Tunnel gains: Account Account; ClientVersion string; ConnectedAt time.Time; requests atomic.Int64 (Requests() int64)
  func (r *Registry) CountAccount(accountID, exceptSession string) int
  func (r *Registry) ByAccount(accountID string) []*Tunnel 
  type TunnelSnapshot struct { URL, ClientVersion string; ConnectedAt time.Time; Requests int64 }
  func (s *Server) TunnelsFor(accountID string) []TunnelSnapshot // for the dashboard; server must NOT import dashboard (dashboard imports server)
  // fakeDirectory in control_test.go (test helper, in-memory) implements Directory for server tests.
  ```
- Handshake when `Directory != nil`, in this order:
  1. Run version and auth checks. Auth errors are mapped: a `*protocol.Error` passes through, anything else becomes `CodeUnauthorized`.
  2. A banned account gives `CodeBanned`.
  3. If `CountAccount(acc.ID, hello.SessionID) >= plan.MaxTunnels` (with `MaxTunnels > 0`), return `CodeLimitReached` using the copy from Global Constraints.
  4. Resolve the name:
     - With `hello.Name` set: `!Owns` gives `CodeNameNotOwned`; otherwise claim it.
     - Without a name: try `PreferredName` first if it is owned, then each `Names()` entry in order. The first one the registry grants wins.
     - If none is granted, return `CodeNameTaken` with `"all your subdomains are in use; use --name or upgrade"`.
  5. Ready carries `Plan`, `RateLimit` and `Bandwidth` from the plan.
  - The count check and the claim must not race. Add `ClaimRequest.AccountID` and `ClaimRequest.MaxTunnels`, and have `Registry.Claim` enforce the limit under its lock.
- CLI:
  - When `Ready.Plan != ""`, the status block prints `Plan         Free · 60 req/min · 1 GB/month` after `Forwarding`. Pro reads `Pro · 600 req/min · 50 GB/month`, and `unlimited` is not printed.
  - A fatal `protocol.Error` prints `error: <message>` and nothing else (no code prefix). For `limit_reached` and `name_not_owned`, a second line is added: `manage your account at <server>/dashboard`.

- [ ] **Step 1: Write the failing tests** (`control_test.go`, using `fakeDirectory` with a Free account `"u1"` that owns `["quick-otter-42","acme"]`):
  - `TestCloudDefaultName`: no Name gives `quick-otter-42`.
  - `TestCloudNameNotOwned`: Name `"other"` gives `name_not_owned`.
  - `TestCloudTunnelLimit`: a second tunnel with a different session ID gives `limit_reached` with the exact copy.
  - `TestCloudReconnectSameSessionNotLimited`: same session ID with the old tunnel still registered takes over, with no error (Review Focus 3).
  - `TestCloudConcurrentClaimsRespectLimit`: 10 concurrent handshakes for the same Free account with distinct sessions; exactly 1 gets Ready (Review Focus 2).
  - `TestCloudAllNamesBusy`: on Pro, with 2 owned names both active under other sessions and `MaxTunnels` 5, a third client gets `name_taken` with the "all your subdomains are in use" copy.
  - `TestCloudBanned`
  - `TestCloudReadyPlan`: Ready has `Plan "free"`, `RateLimit 60` and `Bandwidth 1<<30`.
  - `TestSelfHostedUnchanged`: with `Directory` nil, the existing behaviour holds.
  - `main_test.go`, `TestRunHTTPPlanLine`: against a server whose fake Ready includes the plan, the output contains `Plan         Free · 60 req/min · 1 GB/month`.
  - `main_test.go`, `TestRunHTTPFatalMessage`: `limit_reached` gives stderr `error: tunnel limit reached: your free plan allows 1 concurrent tunnel(s)` plus the dashboard line, and exit 1.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ ./cmd/passway/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): account-aware handshake with owned names and tunnel limits`

---

### Task 5: Edge enforcement — per-plan rate limit, warning with owner, bandwidth, bans

**Files:**
- Create: `internal/server/meter.go`
- Modify: `internal/server/edge.go`, `ratelimit.go`, `pages.go`, `pages/layout.html`, `server.go`
- Test: `internal/server/meter_test.go`, `edge_plan_test.go` (new), `ratelimit_test.go`, `pages_test.go`

**Interfaces:**
- Consumes: `Tunnel.Account`, `Directory`.
- Produces:
  ```go
  func (l *RateLimiter) AllowN(key string, perMinute int) (bool, time.Duration) // per-key rate; Allow(key) = AllowN(key, l.perMinute)
  type Meter struct{ ... } // per-account byte counters
  func NewMeter(d Directory, freeCap int64, flushEvery time.Duration, log *slog.Logger) *Meter
  func (m *Meter) Add(acc Account, n int64)
  func (m *Meter) Over(acc Account) bool        // this month's DB usage + unflushed >= plan.Bandwidth (Bandwidth 0 = never)
  func (m *Meter) FreeCapHit() bool             // free plan total >= FreeBandwidthCap
  func (m *Meter) Run(ctx context.Context)      // flushes every flushEvery (default 60s) and on ctx end; logs WARN once when free total crosses 80% of the cap
  // pageKind gains pageBandwidth (HTTP 402, title "Bandwidth limit reached", plain "monthly bandwidth limit reached").
  // pageData gains Owner string (GitHub login; warning page shows "You are about to visit a tunnel run by @<Owner>" when set).
  func (s *Server) RunBackground(ctx context.Context) // starts Meter.Run and the ban poller (every 15s) when Directory != nil
  ```
- Edge order on a tunnel host, after lookup:
  1. Ban check, from a cached set refreshed by the poller. A banned host or account gives 403 via a `pageBanned` kind, plain text `"this tunnel has been disabled"`.
  2. Warning check: show the warning if `Directory == nil ? cfg.BrowserWarning : acc.Plan.Warning`.
  3. Bandwidth check (`Over` or, for Free, `FreeCapHit`): gives 402.
  4. Rate limit `AllowN(t.Name, plan.RateLimit or cfg.RateLimit)`.
  5. Proxy.
- Bytes are counted as request body bytes plus response body bytes, plus both directions of upgraded connections. Wrap the request body, the ResponseWriter, and a hijacked conn. The edge increments `t.requests` once per proxied request.
- The poller closes the live sessions of banned users and hosts.

- [ ] **Step 1: Write the failing tests:**
  - `TestAllowNPerKey`: key a at 2/min and key b at 5/min are independent.
  - `TestMeterOverAndFlush`: with a fake Directory, 600 MB plus 500 MB on Free means `Over` is true, and after flush, `AddUsage` received 1100 MB total.
  - `TestMeterFreeCapWarnOnce`: crossing 80% logs once (capture the slog handler).
  - `TestEdgeFreeRateLimit`: a Free tunnel allows 60, then the 61st gets 429 (use the limiter's `now` override to stop refill).
  - `TestEdgeProNoWarning`: a Pro browser navigation is proxied; a Free one gets the warning page containing `@u1login`.
  - `TestEdgeBandwidth402`
  - `TestEdgeWebSocketBytesCounted`: an echo of 1000 bytes means the meter counted at least 2000.
  - `TestBanPollerClosesTunnel`: after adding a ban to the fake directory and calling `pollBans` directly, the tunnel's session is closed and the edge returns 403 (Review Focus 5).
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): enforce plan limits, bandwidth and bans at the edge`

---

### Task 6: GitHub sign-in, sessions and CSRF

**Files:**
- Create: `internal/server/dashboard/{dashboard.go,github.go,session.go}`, `templates/{layout.html,home.html,error.html,waitlist.html}`, `static/app.css`
- Modify: `internal/server/server.go`: when `Config.Dashboard != nil`, base-domain paths other than `/healthz` and `/_tunnel` go to it
- Test: `internal/server/dashboard/github_test.go`, `session_test.go`

**Interfaces:**
- Consumes: `*store.Store`, `server.PlanByName`, `server.RandomName`, `server.ValidateName`.
- Produces:
  ```go
  type Config struct {
      Store *store.Store
      BaseURL string            // e.g. "https://passway.run" (no trailing slash)
      GitHubClientID, GitHubClientSecret string
      GitHubAuthURL, GitHubTokenURL, GitHubAPIURL string // defaults: https://github.com/login/oauth/authorize, https://github.com/login/oauth/access_token, https://api.github.com
      MinGitHubAge time.Duration // default 7*24h; 0 disables
      FreeSignupCap int          // default 1000
      DevLogin bool              // allow /auth/dev?login=X (only accepted when BaseURL host is localhost or *.localhost)
      Tunnels func(accountID string) []TunnelInfo // live tunnels for the dashboard (Task 7)
      Now func() time.Time
      Logger *slog.Logger
  }
  type TunnelInfo struct { URL, ClientVersion string; ConnectedAt time.Time; Requests int64 }
  func New(cfg Config) (*Dashboard, error) // validates; errors if DevLogin with non-localhost BaseURL
  func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request)
  // server.Config gains: Dashboard http.Handler
  ```
- Routes:
  - `GET /`: home, with a "Sign in with GitHub" button, or a redirect to `/dashboard` when signed in.
  - `GET /login`: sets the `passway_oauth_state` cookie (random, 10 min, HttpOnly, Lax, `__Host-` prefix on https) and 302s to GitHub with `client_id`, `redirect_uri=<BaseURL>/auth/github/callback`, `state` and `allow_signup=true`. No scope is requested; the public profile is enough.
  - `GET /auth/github/callback`:
    - Checks `state` against the cookie with `hmac.Equal` and clears it.
    - Exchanges the code with `Accept: application/json`.
    - Calls `GET /user` with `Authorization: Bearer`.
    - Rejects an account younger than `MinGitHubAge` with a 403 error page: "GitHub accounts must be at least 7 days old to sign up".
    - Calls `UpsertGitHubUser`.
    - On create:
      - The user is waitlisted if `CountUsers("free") >= FreeSignupCap`.
      - Otherwise it claims a default subdomain: `RandomName()`, retried up to 20 times on `ErrTaken`, with `isDefault` true and max 0.
      - It writes an `Audit("signup")` entry.
    - Creates a session and 303s to `/dashboard`, or to `/waitlist` when the user is waitlisted.
  - `POST /logout` (CSRF): deletes the session, clears the cookie and 303s to `/`.
  - `GET /auth/dev?login=X`, only with `DevLogin`: acts like a GitHub user with id `-hash(login)` and an age of 365 days.
  - `GET /dashboard` and `GET /waitlist`: minimal stub pages (signed-in check only); Task 7 fills in `/dashboard`.
- Every handler sets:
  - `Cache-Control: no-store`
  - `X-Frame-Options: DENY`
  - `X-Content-Type-Options: nosniff`
  - `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self' https://avatars.githubusercontent.com; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'`
- `csrfOK(r, sess)` compares the form field `csrf` with `sess.CSRF` using `hmac.Equal`.
- POST requests also require `Origin` or `Referer` to match `BaseURL` when present.

- [ ] **Step 1: Write the failing tests.** Use a fake GitHub on `httptest.NewServer` serving `/login/oauth/access_token` and `/user`.
  - `TestLoginRedirect`: the Location has the `client_id`, the `redirect_uri` and a `state` equal to the cookie.
  - `TestCallbackCreatesUserAndSession`: after the callback, the user exists with a default subdomain matching the `RandomName` pattern, the session cookie is set with no `Domain` attribute, and GET `/dashboard` returns 200 (the stub page from Task 7 is fine; for now assert it is not a redirect to `/`).
  - `TestCallbackStateMismatch`: no session, 400 (Review Focus 4).
  - `TestCallbackMissingStateCookie`
  - `TestCallbackGitHubError`: the token endpoint returns `{"error":"bad_verification_code"}`, giving 400 and no session.
  - `TestCallbackYoungAccount`: `created_at` is 2 days ago, giving 403 with the copy and no user row.
  - `TestSignupCapWaitlists`: with cap=1 and 2 signups, the second is waitlisted and redirected to `/waitlist`.
  - `TestSessionCookieHostPrefixOnHTTPS`: with BaseURL `https://…`, the cookie name starts with `__Host-` and is `Secure`.
  - `TestTossedCookieRejected`: a request carrying a random `passway_session` value gets the signed-out home (Review Focus 1).
  - `TestPostWithoutCSRFRejected`: POST `/logout` without `csrf` gives 403 and the session still works.
  - `TestDevLoginRefusedOffLocalhost`: `New` errors.
  - `TestSecurityHeaders`
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/dashboard/`. Expected: FAIL.
- [ ] **Step 3: Implement.** Reuse the visual language of `internal/server/pages/layout.html` (colors, logo mark) in `templates/layout.html` and `static/app.css`, served at `/static/app.css` from `embed`.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(dashboard): GitHub sign-in with sessions and CSRF`

---

### Task 7: Dashboard pages — getting started, tunnels, subdomains, tokens, plan

**Files:**
- Create: `internal/server/dashboard/pages.go`, `templates/{dashboard.html,tokens.html,subdomains.html}`
- Modify: `dashboard.go` (routes), `static/app.css`
- Test: `internal/server/dashboard/pages_test.go`

**Interfaces:**
- Consumes: Task 6 session helpers, `Config.Tunnels`, store methods, `server.PlanByName`, `server.ValidateName`.
- Routes (all require a session, otherwise 303 to `/`; all POSTs check CSRF and then 303 back):
  - `GET /dashboard` has four sections:
    - **Getting started:** `curl -fsSL <BaseURL>/install.sh | sh` (week 4 makes this real) plus `passway login <token>`, linking to tokens.
    - **Active tunnels:** a table of URL, client version, connected since (relative) and requests.
    - **Plan:** the plan name and its limits, plus bandwidth used this month as `X MB of 1 GB` with a bar. An "Upgrade to Pro" button is disabled, labelled "Coming soon".
    - **Reserved subdomains:** a short list.
  - Subdomains:
    - `GET /dashboard/subdomains`: the list, with a claim form.
    - `POST /dashboard/subdomains`: field `name`. It runs `ValidateName`, then `ClaimSubdomain(max = plan.MaxSubdomains)`. On error it re-renders with one of:
      - `"name is taken"`
      - `"your Free plan allows 1 reserved subdomain"`
      - the `ValidateName` message
    - `POST /dashboard/subdomains/release`: field `name`.
  - Tokens:
    - `GET /dashboard/tokens`: shows the name, the prefix plus `…`, created and last used, with a revoke button for each.
    - `POST /dashboard/tokens`: field `name`, 1–40 characters. It renders the token page with the plaintext once, in a `<code>` block labelled "Copy it now — you won't see it again", plus the `passway login <token>` line.
    - `POST /dashboard/tokens/revoke`: field `id`.
  - `GET /waitlist`: a waitlist notice; tokens can't be created.
  - Every mutation writes `Audit`.
- Pass all user-controlled text through `html/template` (no `template.HTML` from user input).

- [ ] **Step 1: Write the failing tests** (signed in via the Task 6 dev-login helper, with a fake `Tunnels`):
  - `TestDashboardShowsTunnelsAndUsage`
  - `TestClaimSubdomainFlow`:
    - claiming `acme` on Free shows the limit copy, because the default already uses the 1 slot;
    - after `SetPlan` pro, `acme` is claimed;
    - a second user claiming `acme` gets "name is taken".
  - `TestClaimInvalidName`: `-bad` and `paypal-x` show the validation message.
  - `TestReleaseSubdomain`
  - `TestCreateTokenShownOnce`: the POST response contains a `pw_…` token; a later GET of `/dashboard/tokens` doesn't contain it but does contain the prefix.
  - `TestRevokeOtherUsersTokenFails`: 404.
  - `TestWaitlistedCannotCreateToken`: 403.
  - `TestAllPostsRequireCSRF`: iterate over every POST route and expect 403 without the token.
  - `TestUserTextEscaped`: a token name `<script>x</script>` renders escaped.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/dashboard/`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(dashboard): tunnels, reserved subdomains, tokens and plan pages`

---

### Task 8: Server modes and admin CLI

**Files:**
- Create: `cmd/passway-server/admin.go`
- Modify: `cmd/passway-server/main.go`
- Test: `cmd/passway-server/main_test.go`, `admin_test.go`

**Interfaces:**
- Consumes: everything above.
- Flags on `passway-server` (serve):
  - `--self-hosted` (bool). Its behaviour is today's, plus optional `--db` tokens: when `--db` is set, `Authenticator` accepts `--tokens` and admin DB tokens, both giving `PlanUnlimited`.
  - Cloud mode is the default without `--self-hosted`. It requires:
    - `--db PATH`
    - `--github-client-id`
    - `--github-client-secret`, or `PASSWAY_GITHUB_CLIENT_SECRET` from the environment, preferred
  - If any is missing, it fails with the usage error `cloud mode needs --db, --github-client-id and --github-client-secret (or run your own relay with --self-hosted)`.
  - The cloud-mode exception is `--dev-login` on a localhost domain, which needs only `--db`.
  - Other cloud flags: `--base-url` (default `<scheme>://<domain>[:port]`), `--min-github-age` (duration, default `168h`), `--free-signup-cap` (default 1000), `--free-bandwidth-cap` (bytes, default `536870912000`).
  - `--tokens`, `--open` and the open-relay check apply only in self-hosted mode. In cloud mode, `--tokens` is a usage error.
  - The cookie secret is `store.Setting("cookie_secret", 32 random bytes)` whenever `--db` is set, so the warned cookie survives restarts.
  - `srv.RunBackground(ctx)` is started in cloud mode.
- `passway-server admin --db PATH <cmd>`:
  - `ban user <login> [--reason R]`
  - `ban host <label> [--reason R]`
  - `unban user|host <value>`
  - `set-plan <login> free|pro`
  - `whois <label>`: prints `<label> → @<login> (plan, user id)`, or `unclaimed`.
  - `token create <name>`: a self-hosted admin token; prints the plaintext once.
  - `token list`
  - `token revoke <id>`
  - Each prints one line and exits 0. An unknown user or label exits 1 with `error: no user "<login>"`.
- The existing `TestRun*` tests for the server pass `--self-hosted`.

- [ ] **Step 1: Write the failing tests:**
  - `TestCloudModeRequiresFlags`: exit 2 with the copy.
  - `TestCloudTokensFlagRejected`
  - `TestDevLoginLocalhostOnly`
  - `TestSelfHostedDBTokens`: create a token via `admin token create`, then a client connects with it.
  - `TestAdminBanUser`: after the ban, `IsBanned` is true; an unknown login exits 1.
  - `TestAdminSetPlan`
  - `TestAdminWhois`
  - `TestCookieSecretPersisted`: two `run` calls on the same DB see the same `Setting` bytes.
  - Update the existing tests to pass `--self-hosted`.
- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3: Implement.** Dispatch `admin` before parsing the serve flags. Wire `server.Config{Directory: &store.Directory{S: st}, Dashboard: dash}` in cloud mode. `dashboard.Config.Tunnels` maps `srv.TunnelsFor` (Task 4) to `[]dashboard.TunnelInfo` in a small adapter in main.go; `server` must not import `dashboard`.
- [ ] **Step 4: Run** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): cloud and self-hosted modes, admin CLI`

---

### Task 9: End-to-end, docs and the M3 demo dry-run

**Files:**
- Modify: `internal/e2e/e2e_test.go`, `README.md`
- Create: `docs/self-hosting.md`

**Interfaces:**
- Consumes: everything above. The `opts` struct gains `cloud bool`. When it is set, `setupEnv` opens a temp store, builds a `dashboard` with `DevLogin` on a `*.localhost`-style BaseURL (the test server's host), and sets `server.Config{Directory, Dashboard}`. It returns `env.store` and `env.base` (the base-domain URL).

- [ ] **Step 1: Write the failing tests:**
  - `TestCloudSignupLoginClaim`:
    - Dev-login as `alice` with a cookie-jar client.
    - POST `/dashboard/tokens` with the CSRF value parsed from the page, and extract `pw_…`.
    - Connect `client.Run` with that token and no Name. The URL is `alice`'s default subdomain.
    - A public GET reaches the app.
    - `SetPlan` pro, claim `acme` through the dashboard, and a second client with `--name acme` works.
    - The dashboard shows 2 active tunnels.
  - `TestCloudFreeSecondTunnelRefused`: `limit_reached`, fatal, with the exact copy.
  - `TestCloudFreeWarningShowsOwner`: a browser GET shows the warning containing `@alice`; a webhook POST passes.
  - `TestCloudBanDisconnects`: `AddBan` user plus a direct `pollBans` closes the client; `client.Run` returns `banned` on reconnect.
  - `TestSelfHostedStillRandomNames`: the existing default path keeps working.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/e2e/`. Expected: FAIL until the wiring is in. Fix behaviour bugs in the owning package, with a unit test.
- [ ] **Step 3: Docs.**
  - **README:** a "Passway Cloud" section (sign in, `passway login`, plans table) and the server modes. Move the flag reference for self-hosters to `docs/self-hosting.md`.
  - **`docs/self-hosting.md`:** `--self-hosted`, `--tokens`, `admin token create`, the DNS wildcard and TLS notes from spec §6.9, systemd and `docker run` placeholders (week 4 ships the image), and the admin commands.
  - **Owner to-dos**, listed in the README "Running Passway Cloud" subsection:
    - create a GitHub OAuth app with callback `<base-url>/auth/github/callback`;
    - put the base domain on the Public Suffix List before launch.
- [ ] **Step 4: Verify** with `go vet ./... && go test -race -count=1 ./...` and cross-builds for windows and darwin. Then do the demo dry-run headlessly:
  1. Run `passway-server --domain localhost --db /tmp/…/p.db --dev-login`.
  2. Dev-login as `demo`.
  3. Create a token in the dashboard.
  4. Run `passway login <token>`, then `passway http 3000`. The URL is the default subdomain, and the status block shows the Plan line.
  5. Run `admin set-plan demo pro`, claim `acme`, then `passway http 3000 --name acme`.
  6. Take headless-Chromium screenshots of the dashboard, tokens, subdomains and warning-with-owner pages into the scratchpad.
- [ ] **Step 5: Commit and push** `test(e2e): cloud signup, limits, bans; docs for M3`

---

## Deferred (not this week)

- Week 4:
  - Stripe Checkout, Customer Portal and webhooks
  - the `subscriptions` and `custom_domains` tables
  - custom domains and on-demand TLS
  - static pages (docs, pricing, terms, privacy, AUP, abuse report form)
  - install script
  - GoReleaser
- PROXY protocol v2 and real client IPs (spec §6.4) ship with the first VPS deploy.
- htmx progressive enhancement for the dashboard is optional after launch.
- Owner inputs:
  - final prices and limits
  - the GitHub OAuth app credentials
  - the domain, Public Suffix List entry and VPS deploy
