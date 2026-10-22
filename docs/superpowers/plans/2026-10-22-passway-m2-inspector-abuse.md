# Passway M2: Inspector + Abuse Basics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the local request inspector (live list, detail, replay, edit & replay, copy as cURL) and the first abuse controls (subdomain blocklist, per-tunnel rate limit and connection cap, browser warning page, branded error pages), plus the M1 review items deferred to this week.

**Architecture:** A new `internal/inspector` package (MIT) wraps the client's local reverse proxy in a capture middleware. That middleware records each exchange into an in-memory ring buffer, and a small loopback-only HTTP server exposes the buffer as JSON and SSE behind a single embedded HTML page. Replay builds a request from a stored exchange and runs it through the same wrapped handler in-process, so it reaches the local app exactly as live traffic does and is captured as a new entry. On the server, the edge gains a branded page renderer, a token-bucket limiter keyed by tunnel name, an optional browser-warning interstitial, and a concurrency cap on each tunnel's transport.

**Tech Stack:** Go 1.26 stdlib (`net/http` ServeMux patterns, `httputil.ReverseProxy`, `embed`, `html/template`), existing `coder/websocket` and `hashicorp/yamux`. No new dependencies. Vanilla JS with no build step for the UI.

**Spec:** `docs/superpowers/specs/2026-10-21-passway-design.md` (§6.3, §6.5, §6.7, §6.8, §7, §9 week 2). Deferred items come from the M1 reviews.

## Global Constraints

- Module `github.com/mohammed19n/passway`; no new third-party dependencies.
- Licensing: `internal/inspector` and `internal/client` are MIT, and `internal/server` and `cmd/passway-server` are AGPL-3.0 (the existing LICENSE files cover them, so add no headers).
- The inspector binds **only to loopback**. The default address is `127.0.0.1:4040`, and a non-loopback `--inspect-addr` is rejected.
- Ring buffer default is **500** exchanges. Each captured body is capped at **1 MB** (`1 << 20` bytes) and larger ones are marked truncated. The full body is always forwarded.
- The UI is one embedded HTML file with no build step and no external network loads (no CDN).
- Skip-warning header: `Passway-Skip-Warning` (any value). Warning cookie: `passway_warned=1`, host-only, `Path=/`, `HttpOnly`, `SameSite=Lax`, max age 7 days.
- Reserved edge path prefix on tunnel hosts: `/_passway/`.
- The server keeps self-hosted behaviour: the warning page and rate limit are **off by default** and enabled with flags. Week 3 ties them to plans.
- Commits: no `Co-Authored-By` or any attribution trailer. Push after each commit.
- Tests run with `go test -race -count=1 ./...`.

## Review Focus

1. **DNS rebinding / CSRF against the inspector.** A malicious web page that the developer visits must not be able to read captured webhooks or trigger replays. Every inspector request must have a loopback `Host`, and every mutating request must be `application/json` with no foreign `Origin` (tests in Task 6).
2. **Streaming and upgraded responses through the capture middleware.** SSE responses, chunked downloads and WebSocket upgrades must keep working with the inspector on: Flush and Hijack must still reach the real writer, and the request is recorded when it ends (tests in Tasks 5 and 9).
3. **Replaying a truncated body.** A >1 MB request can't be replayed faithfully. Replay without an edited body must answer 409 with a clear message, never send a cut-off body (test in Task 6).
4. **Open redirect via the warning page's continue link.** `/_passway/continue?to=//evil.com` or `to=https://evil.com` must redirect to `/`, not off-site (test in Task 4).
5. **Two `passway` processes at once.** The second one must not crash when 4040 is taken. With the default address it falls back to 4041–4049. With an explicit address it fails clearly (test in Task 8).

---

## File Structure

```
internal/server/
  names.go          + blockedWords, blocklist check in ValidateName
  registry.go       + purgeExpired sweep
  edge.go           ~ DialContext honours ctx, MaxConnsPerHost; edge applies limiter + warning
  pages.go          NEW  branded page renderer (offline / rate-limited / warning)
  pages/layout.html NEW  shared layout + the three page bodies
  offline.html      DELETE (moved into pages/)
  ratelimit.go      NEW  token-bucket limiter keyed by tunnel name
  warning.go        NEW  browser-warning decision + /_passway/continue
  server.go         ~ Config: MaxConnsPerTunnel, RateLimit, BrowserWarning; limiter field
internal/inspector/                         (MIT; covered by repo-root LICENSE)
  store.go          NEW  Exchange types, ring buffer, subscribers
  capture.go        NEW  capture middleware
  replay.go         NEW  replay / edit & replay
  curl.go           NEW  cURL rendering
  api.go            NEW  loopback HTTP API (JSON + SSE) + guard + UI handler
  ui/index.html     NEW  the inspector page
internal/client/
  listener.go       ~ export NewProxy; Config.Handler override
  client.go         ~ fatal "not a Passway server" on non-101 handshake
  target.go         ~ accept ":3000"
cmd/passway/http.go       ~ --inspect, --inspect-addr, fallback ports, status block
cmd/passway-server/main.go ~ --max-conns-per-tunnel, --rate-limit, --browser-warning; publicPort()
internal/e2e/e2e_test.go  ~ inspector, 429 and warning e2e; timeouts on channel waits
README.md                 ~ inspector + new flags
```

---

### Task 1: Server hardening (blocklist, hold sweep, tunnel connection cap)

**Files:**
- Modify: `internal/server/names.go`, `internal/server/registry.go`, `internal/server/edge.go`, `internal/server/server.go`, `internal/server/control.go` (pass the cap), `cmd/passway-server/main.go`
- Test: `internal/server/names_test.go`, `internal/server/registry_test.go`, `internal/server/control_test.go`, `cmd/passway-server/main_test.go`

**Interfaces:**
- Produces: `server.Config.MaxConnsPerTunnel int` (0 = 64); `newTunnelProxy(t *Tunnel, maxConns int, log *slog.Logger)`; `(*Registry).purgeExpired()` (caller holds mu); `publicPort(scheme string, listenPort, override int) int` in `cmd/passway-server`.

- [ ] **Step 1: Write the failing tests**

```go
// names_test.go
func TestValidateNameBlocklist(t *testing.T) {
	for _, n := range []string{"paypal", "my-paypal-help", "login-now", "secure-bank", "applesupport"} {
		if err := ValidateName(n); err == nil {
			t.Errorf("%q accepted", n)
		} else { wantCode(t, err, protocol.CodeInvalidName) }
	}
	if err := ValidateName("happy-otter-12"); err != nil { t.Fatal(err) }
}
func TestWordListsAvoidBlocklist(t *testing.T) // every adjective and animal passes blockedWord(w) == ""

// registry_test.go
func TestReleasePurgesExpiredHolds(t *testing.T) // claim+release "aaa" (s1); advance clock past hold; claim+release "bbb" (s2); len(r.held) == 1 and only "bbb"

// control_test.go
func TestTunnelConnCap(t *testing.T)
// New(Config{..., MaxConnsPerTunnel: 2}); dialRaw a tunnel; serve its streams with an
// http.Server whose handler increments an in-flight counter, records the max, and blocks
// on a channel; fire 5 concurrent GETs to <name>.localhost:<port>; wait until in-flight == 2,
// sleep 200ms, assert max == 2; release; all 5 return 200.

func TestDialHonoursContext(t *testing.T)
// Tunnel whose client side never accepts streams (dialRaw, never AcceptStream) and with
// MaxConnsPerTunnel: 1; first GET with a 300ms client timeout fails; the request returns
// within 2s (it doesn't hang in DialContext).

// cmd/passway-server/main_test.go
func TestPublicPort(t *testing.T) // table: {"http",8080,0}->8080 {"http",80,0}->0 {"https",443,0}->0 {"https",9443,0}->9443 {"https",9443,443}->0 {"http",8080,9000}->9000
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ ./cmd/passway-server/`. Expected: FAIL (the blocklist isn't enforced, `MaxConnsPerTunnel` and `publicPort` are undefined).

- [ ] **Step 3: Implement**
  - `names.go`: add `var blockedWords = []string{"paypal", "login", "signin", "logon", "bank", "secure", "verify", "wallet", "password", "apple", "google", "microsoft", "office365", "outlook", "amazon", "facebook", "instagram", "whatsapp", "netflix", "coinbase", "binance", "metamask", "stripe", "github"}` and `blockedWord(name string) string` (returns the first word contained in name, or ""). `ValidateName` returns `CodeInvalidName` with message `name "<n>" contains a blocked word` after the regexp and reserved checks.
  - `registry.go`: `purgeExpired()` deletes every hold older than `r.hold`. Call it at the top of `Release`.
  - `edge.go`: `newTunnelProxy` gets a `maxConns` parameter and sets `MaxConnsPerHost: maxConns` and `MaxIdleConnsPerHost: min(16, maxConns)`. `DialContext` runs `session.Open` and the header write in a goroutine that sends `(net.Conn, error)` on a buffered chan. It selects on that chan and `ctx.Done()`. If ctx wins, it starts a goroutine that closes the stream when it arrives and returns `ctx.Err()`.
  - `server.go`: `MaxConnsPerTunnel` defaults to 64 in `New`; `control.go` passes `s.cfg.MaxConnsPerTunnel`.
  - `cmd/passway-server`: add the `--max-conns-per-tunnel` flag (default 64) and extract `publicPort` from the inline logic in `run`.

- [ ] **Step 4: Run** the same command. Expected: PASS. Then run `go test -race -count=1 ./...` and expect PASS.

- [ ] **Step 5: Commit + push** `feat(server): subdomain blocklist, hold sweep, per-tunnel connection cap`

---

### Task 2: Branded edge pages (polished offline page)

**Files:**
- Create: `internal/server/pages.go`, `internal/server/pages/layout.html`
- Delete: `internal/server/offline.html`
- Modify: `internal/server/edge.go` (use `renderPage`)
- Test: `internal/server/pages_test.go`

**Interfaces:**
- Produces:
  ```go
  type pageKind int
  const (pageOffline pageKind = iota; pageRateLimited; pageWarning)
  type pageData struct {
      Host      string        // r.Host as received
      Continue  string        // warning only: "/_passway/continue?to=<escaped path>"
      RetryIn   int           // rate-limited only: seconds
  }
  // renderPage writes the page with its status (offline 502, rate-limited 429, warning 200),
  // Cache-Control: no-store, and Content-Type text/html when r accepts HTML, otherwise a
  // one-line text/plain body (`tunnel <host> is offline`, `rate limit exceeded; retry in <n>s`).
  func renderPage(w http.ResponseWriter, r *http.Request, kind pageKind, d pageData)
  ```
  `writeOffline(w, host)` is removed, and its callers use `renderPage(w, r, pageOffline, pageData{Host: r.Host})`.

- [ ] **Step 1: Write the failing tests.** `TestRenderPageHTML` is a table over the three kinds with `Accept: text/html`. It checks the status, that the body contains the escaped host (`<script>` host → `&lt;script&gt;`), and that the body contains `Passway`. `TestRenderPagePlain` checks that with `Accept: application/json` the body is the plain-text line and the status is the same. `TestRenderPageWarningLink` checks that the body contains `href="/_passway/continue?to=%2Fdocs%3Fa%3D1"` for `Continue` built from `/docs?a=1`.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ -run RenderPage`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** `pages/layout.html` uses `{{define}}` blocks `offline`, `ratelimited`, `warning` in one shared layout. The layout is a centered card with a small Passway wordmark (inline SVG/CSS, no external assets), a headline, an explanation, and a footer line "Served by Passway · passway.run". It has light and dark styles via `prefers-color-scheme`. Copy:
  - offline: "This tunnel is offline". Body: "**{{.Host}}** isn't connected right now. If it's yours, start it with `passway http <port>`." It also mentions that it reconnects automatically.
  - rate-limited: "Too many requests". Body: "**{{.Host}}** is receiving more requests than its limit allows. Try again in {{.RetryIn}}s."
  - warning: "You are about to visit {{.Host}}". Body: "This site is served through a Passway tunnel from someone's computer. Only continue if you know and trust who sent you this link. Never enter passwords or payment details unless you're sure." A primary button "Visit site" links to `{{.Continue}}`. A small note for developers says to send the `Passway-Skip-Warning` header to skip this page.
  
  The rate-limited page also sets `Retry-After: <RetryIn>`.
- [ ] **Step 4: Run** the server tests. Expected: PASS. Run `go test -race -count=1 ./...` (the e2e offline tests must still pass). Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(server): branded offline, rate-limit and warning pages`

---

### Task 3: Per-tunnel rate limit

**Files:**
- Create: `internal/server/ratelimit.go`
- Modify: `internal/server/server.go`, `internal/server/edge.go`, `cmd/passway-server/main.go`
- Test: `internal/server/ratelimit_test.go`, `internal/server/control_test.go`

**Interfaces:**
- Produces:
  ```go
  // RateLimiter is a token bucket per key: capacity = perMinute, refill perMinute/60 per second.
  type RateLimiter struct{ /* mu, perMinute, now func() time.Time, buckets map[string]*bucket, lastSweep */ }
  func NewRateLimiter(perMinute int) *RateLimiter
  // Allow takes one token for key. When none is left it returns false and how long until one is.
  func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration)
  ```
  The limiter sweeps buckets that have been full and idle for ≥10 min, at most once a minute, inside `Allow`. `server.Config.RateLimit int` sets requests per minute per tunnel (0 = unlimited, so no limiter). The key is the tunnel **name**, so reconnecting doesn't reset it.

- [ ] **Step 1: Write the failing tests** (with an injected clock via the unexported `now` field):
  - `TestRateLimiterBurstThenRefill`: perMinute 60 gives 60 allows and then a false with `retryAfter` ≈ 1s (between 900ms and 1s). After advancing 1s, one allow succeeds.
  - `TestRateLimiterKeysIndependent`: draining "a" doesn't affect "b".
  - `TestRateLimiterSweep`: after "a" is used once and 11 minutes pass, an `Allow("b")` leaves `len(buckets) == 1`.
  - `TestEdgeRateLimited` (control_test): `RateLimit: 2`. A tunnel whose client side serves 200. Three GETs give 200, 200, 429, and the 429 has a `Retry-After` header that parses as an int ≥ 1.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ -run 'RateLimit'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** `New` builds `s.limiter` when `RateLimit > 0`. In `serveEdge`, after the tunnel lookup succeeds and before proxying, a denied `Allow(t.Name)` calls `renderPage(w, r, pageRateLimited, pageData{Host: r.Host, RetryIn: ceil seconds, min 1})`. Offline hosts aren't limited. In `cmd/passway-server`, add `--rate-limit` (requests/min per tunnel, default 0).
- [ ] **Step 4: Run** the same command plus `./...`. Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(server): per-tunnel request rate limit`

---

### Task 4: Browser warning page

**Files:**
- Create: `internal/server/warning.go`
- Modify: `internal/server/server.go`, `internal/server/edge.go`, `cmd/passway-server/main.go`
- Test: `internal/server/warning_test.go`

**Interfaces:**
- Produces:
  ```go
  // needsWarning: GET or HEAD, Accept contains "text/html", no Passway-Skip-Warning header,
  // no passway_warned cookie, and the path is not under /_passway/.
  func needsWarning(r *http.Request) bool
  // handleContinue serves GET /_passway/continue?to=<path>: sets the cookie, then 303 to safeReturnPath(to).
  func handleContinue(w http.ResponseWriter, r *http.Request)
  // safeReturnPath returns to when it is a local absolute path ("/" prefix, not "//" or "/\\",
  // and url.Parse yields no scheme and no host); otherwise "/".
  func safeReturnPath(to string) string
  ```
  `server.Config.BrowserWarning bool`. In `serveEdge`, after the label check: if the path is `/_passway/continue`, call `handleContinue` (when BrowserWarning is on; otherwise 404). Requests to an active tunnel with `BrowserWarning && needsWarning(r)` get `renderPage(..., pageWarning, pageData{Host, Continue: "/_passway/continue?to=" + url.QueryEscape(r.URL.RequestURI())})`. The warning comes before the rate limit, so the interstitial doesn't use up the tunnel's tokens. Any other `/_passway/` path goes to the tunnel unchanged.

- [ ] **Step 1: Write the failing tests**
  - `TestNeedsWarning` is a table: a browser GET → true; POST with `Accept: text/html` → false; `Accept: application/json` → false; a skip header → false; a `passway_warned=1` cookie → false; `/_passway/continue` → false.
  - `TestSafeReturnPath` is a table: `/docs?a=1` stays the same; `""`, `//evil.com`, `/\evil.com`, `https://evil.com`, `javascript:alert(1)` and `evil.com` all → `/`.
  - `TestHandleContinue`: the response is 303 with `Location: /docs`. Its `Set-Cookie` has `passway_warned=1`, `HttpOnly`, `SameSite=Lax` and `Max-Age=604800`, and no `Domain`.
  - `TestEdgeWarningFlow` (with a tunnel, `BrowserWarning: true`): a browser GET gives 200 with the warning page, and the local app wasn't hit (a counter stays 0). The same GET with the cookie reaches the app. A webhook POST (`Content-Type: application/json`) reaches the app without the cookie.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ -run 'Warning|SafeReturn|Continue'`. Expected: FAIL.
- [ ] **Step 3: Implement** per the interfaces. Add the `--browser-warning` flag to `cmd/passway-server` (default false).
- [ ] **Step 4: Run** the server tests and `./...`. Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(server): browser warning interstitial for tunnels`

---

### Task 5: Inspector store and capture middleware

**Files:**
- Create: `internal/inspector/store.go`, `internal/inspector/capture.go`
- Test: `internal/inspector/store_test.go`, `internal/inspector/capture_test.go`

**Interfaces:**
- Produces:
  ```go
  package inspector // Package inspector captures tunnel traffic and serves the local inspector UI.

  const DefaultCapacity = 500
  const MaxBody = 1 << 20

  type Body struct {
      Data      []byte `json:"data"`      // at most MaxBody bytes (base64 in JSON)
      Size      int64  `json:"size"`      // full size seen on the wire
      Truncated bool   `json:"truncated"`
  }
  type Request struct {
      Method string      `json:"method"`
      URI    string      `json:"uri"`    // r.URL.RequestURI()
      Host   string      `json:"host"`
      Proto  string      `json:"proto"`
      Header http.Header `json:"header"`
      Body   Body        `json:"body"`
  }
  type Response struct {
      Status int         `json:"status"`
      Header http.Header `json:"header"`
      Body   Body        `json:"body"`
  }
  type Exchange struct {
      ID       int64         `json:"id"`        // assigned by Store.Add, starting at 1
      Start    time.Time     `json:"start"`
      Duration time.Duration `json:"duration"`  // ns
      Request  Request       `json:"request"`
      Response Response      `json:"response"`
      ReplayOf int64         `json:"replay_of,omitempty"` // 0 = live traffic
      Upgraded bool          `json:"upgraded,omitempty"`  // connection was hijacked (WebSocket)
  }
  type Summary struct {
      ID int64; Start time.Time; Duration time.Duration; Method, URI string
      Status int; ReplayOf int64; Upgraded bool   // with the same snake_case json tags
  }
  func (e *Exchange) Summary() Summary

  type Store struct{ /* mu, ring []*Exchange, next int64, subs map[chan Summary]struct{} */ }
  func NewStore(capacity int) *Store                // capacity <= 0 → DefaultCapacity
  func (s *Store) Add(e *Exchange) int64            // sets e.ID, evicts oldest, notifies subscribers non-blocking
  func (s *Store) Get(id int64) (*Exchange, bool)
  func (s *Store) List() []Summary                  // newest first
  func (s *Store) Clear()
  func (s *Store) Subscribe() (<-chan Summary, func()) // buffered 64; slow subscribers drop events

  // Capture records every request served by next into s. The request body is teed (capped
  // at MaxBody for the record, unbounded for next). The response writer wrapper captures
  // status, header snapshot at WriteHeader, and up to MaxBody of body. It implements Unwrap
  // so Flush/Hijack reach the real writer. A hijack sets Upgraded=true and Status 101. The
  // exchange is added when next returns. The context value set by WithReplayOf(ctx, id) is
  // copied into ReplayOf.
  func Capture(s *Store, next http.Handler) http.Handler
  func WithReplayOf(ctx context.Context, id int64) context.Context
  ```
  An entry is added when its request **finishes**, so a long-lived SSE or WebSocket request appears when it closes. That is acceptable for webhook debugging.

- [ ] **Step 1: Write the failing tests**
  - `TestStoreRing`: capacity 3 with 5 adds. `List` returns IDs 5,4,3, and `Get(1)` is missing.
  - `TestStoreSubscribe`: a subscriber receives the summary of an add. After cancel, the channel is closed and `Add` doesn't block.
  - `TestStoreSlowSubscriberDoesNotBlock`: a subscriber that is never read, followed by 200 adds, completes within 1s.
  - `TestCaptureRecordsExchange`: POST `{"a":1}` to a handler that echoes the body with status 201 and header `X-Test: y`. The stored request has the method, URI `/hook?x=1`, header and body. The stored response has 201, `X-Test` and the echoed body. Duration is > 0 and ReplayOf is 0.
  - `TestCaptureTruncatesLargeBody`: a 3 MB request with a handler that counts the bytes it reads. The handler reads all 3 MB. The record has `Size == 3<<20`, `len(Data) == MaxBody` and `Truncated`. The same holds for a 3 MB response body.
  - `TestCaptureKeepsFlusher`: with a `httptest.NewServer(Capture(...))` handler that writes, flushes, then blocks until released, the client reads the first chunk before the release.
  - `TestCaptureHijack`: the handler hijacks via `http.NewResponseController(w).Hijack()` without an error. The record has `Upgraded` and `Status == 101`.
  - `TestCaptureReplayOf`: a request whose ctx has `WithReplayOf(ctx, 7)` is stored with `ReplayOf == 7`.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/inspector/`. Expected: FAIL (the package doesn't exist yet).
- [ ] **Step 3: Implement** per the interfaces. Copy the header with `Header.Clone()`. The capping body reader wraps `r.Body` and counts the full size while it keeps the first MaxBody bytes.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(inspector): exchange store and capture middleware`

---

### Task 6: Replay, cURL and the inspector API

**Files:**
- Create: `internal/inspector/replay.go`, `internal/inspector/curl.go`, `internal/inspector/api.go`, `internal/inspector/ui/index.html` (a placeholder `<!doctype html><title>Passway Inspector</title>`; Task 7 replaces it)
- Test: `internal/inspector/replay_test.go`, `internal/inspector/curl_test.go`, `internal/inspector/api_test.go`

**Interfaces:**
- Consumes: Task 5's `Store`, `Capture` and `WithReplayOf`.
- Produces:
  ```go
  // Edit overrides parts of a captured request. Nil fields keep the original.
  type Edit struct {
      Method *string      `json:"method,omitempty"`
      URI    *string      `json:"uri,omitempty"`    // must start with "/"
      Header http.Header  `json:"header,omitempty"` // replaces the whole header when non-nil
      Body   *string      `json:"body,omitempty"`   // UTF-8 text; replaces body
  }
  var ErrTruncated = errors.New("inspector: captured body was truncated; edit the body to replay")
  var ErrNotFound  = errors.New("inspector: no such request")

  type Inspector struct{ /* store *Store, handler http.Handler (Capture-wrapped), publicURL atomic string */ }
  // New wraps next (the client's local proxy) with Capture. Handler() is what the tunnel serves.
  func New(capacity int, next http.Handler) *Inspector
  func (in *Inspector) Handler() http.Handler
  func (in *Inspector) Store() *Store
  func (in *Inspector) SetPublicURL(u string) // from OnReady; used by cURL
  // Replay rebuilds request id (with edit applied), runs it through Handler() in-process with
  // WithReplayOf(ctx,id), and returns the new exchange's ID. Content-Length is recomputed.
  func (in *Inspector) Replay(ctx context.Context, id int64, edit *Edit) (int64, error)
  // CurlCommand renders e as a single-line POSIX-shell cURL command against base+URI
  // (base = public URL, or "http://"+Host when unset). It drops Host, Content-Length,
  // Connection, Accept-Encoding and X-Forwarded-* headers, single-quotes every argument,
  // and uses --data-binary for non-empty bodies (a truncated body appends the comment " # body truncated").
  func CurlCommand(e *Exchange, base string) string
  // UI returns the loopback-only HTTP handler for the inspector page and API.
  func (in *Inspector) UI() http.Handler
  ```
  Routes on `UI()`: `GET /` (index.html), `GET /api/requests` (`[]Summary`), `DELETE /api/requests` (204), `GET /api/requests/{id}` (`Exchange`), `GET /api/requests/{id}/curl` (text/plain), `POST /api/requests/{id}/replay` (optional `Edit` JSON body; returns `{"id":N}`; ErrNotFound → 404, ErrTruncated → 409 with the message, a bad edit → 400), and `GET /api/events` (SSE: `event: exchange\ndata: <Summary JSON>\n\n` per add, plus a `: ping` comment every 15s).
  Guard (wraps everything): if the Host's hostname is not `localhost`, `127.0.0.1` or `::1`, return 403. A POST or DELETE needs `Content-Type: application/json` (DELETE may also have no body or content type, but then it needs `Sec-Fetch-Site` absent or `same-origin`), otherwise 415. If `Origin` is present and not `http://<request Host>`, return 403. No CORS headers are ever set.

- [ ] **Step 1: Write the failing tests**
  - `TestReplaySendsSameRequest`: the local handler records the method, URI, a header and the body. After one captured POST, `Replay(id, nil)` makes the handler see identical values. The store has a new entry with `ReplayOf == id`.
  - `TestReplayWithEdit`: an edit to the method `PUT`, URI `/other`, body `{"b":2}`. The handler sees them, and `Content-Length` == 7.
  - `TestReplayTruncated` returns `ErrTruncated` for a 2 MB body. With `Edit{Body: ptr("x")}` it succeeds.
  - `TestReplayNotFound` returns `ErrNotFound`.
  - `TestCurlCommand` checks the exact string for POST `/hook?a=1` with header `Stripe-Signature: t=1,v1=abc`, body `{"id":"evt_1"}` and base `https://x.passway.run`:
    `curl -X 'POST' 'https://x.passway.run/hook?a=1' -H 'Stripe-Signature: t=1,v1=abc' --data-binary '{"id":"evt_1"}'`. A second case checks that a body containing `'` is escaped as `'\''`.
  - `TestAPIGuard`: `Host: evil.com` → 403. A replay POST without the JSON content type → 415. A POST with `Origin: http://evil.com` → 403. A GET `/api/requests` with `Host: 127.0.0.1:4040` → 200.
  - `TestAPIListGetReplay`: drive a capture, then check that GET list has 1 item, GET `/api/requests/1` decodes to an Exchange with a matching body, and POST replay returns `{"id":2}`. A replay of a truncated entry returns 409.
  - `TestAPIEvents`: open `/api/events` with `httptest.NewServer`, add an exchange, and read one `event: exchange` frame within 2s.
  - `TestAPIClear`: DELETE → 204, and then the list is empty.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/inspector/`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** per the interfaces. Use Go 1.22 `http.ServeMux` method+path patterns and `r.PathValue("id")`. Build the replay request with `http.NewRequestWithContext(WithReplayOf(ctx,id), method, uri, body)`, set `req.Host = captured.Host`, `req.RemoteAddr = "127.0.0.1:0"`, and clone the headers. Serve it into a discard-capturing writer (`httptest.NewRecorder` is fine). Get the new ID by subscribing before serving and reading the summary whose `ReplayOf == id`, or make `Capture` return the ID through a ctx-provided pointer. **Pick the ctx pointer:** `WithReplayOf` stores `*replayInfo{of int64; newID int64}` and `Capture` fills in `newID`. `index.html` is embedded with `//go:embed ui/index.html`.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(inspector): replay, edit & replay, cURL export and local API`

---

### Task 7: Inspector UI

**Files:**
- Modify: `internal/inspector/ui/index.html`
- Test: `internal/inspector/api_test.go` (smoke test only)

**Interfaces:**
- Consumes: the Task 6 API routes and JSON shapes, exactly as defined there.

This is the README GIF and the headline feature, so it deserves real design care. The constraints are one file, vanilla JS, no external loads, and it must work on screens 1024px wide and up (it should stay usable at 375px).

- [ ] **Step 1: Write the failing smoke test.** `TestUIIndex` checks that GET `/` returns 200 with `text/html`. The body has `<title>Passway Inspector</title>` and contains the strings `/api/events`, `/replay` and `/curl` (the page wires to the real API). It also checks that the body contains neither `http://` nor `https://` URLs for script or stylesheet `src`/`href` (no external loads).
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/inspector/ -run UIIndex`. Expected: FAIL (it's still the placeholder).
- [ ] **Step 3: Build the page:**
  - Layout: a top bar with the Passway wordmark, the tunnel URL (from a new `GET /api/status` that returns `{"public_url":..., "target":...}`; add the route and `Inspector.SetTarget(string)` with a test asserting its JSON, in `api_test.go`), a live-connection dot, and a **Clear** button. Below that is a two-pane split: the request list on the left and the detail pane on the right.
  - List rows show method, URI, status (colored 2xx/3xx/4xx/5xx), duration and relative time. They also show a "replay" badge when `replay_of` is set and "ws" when upgraded. New rows animate in at the top. There are keyboard ↑/↓ for selection and a text filter box (matching method, URI or status).
  - Detail pane tabs are **Request** and **Response**. Each has collapsible headers and a body viewer that pretty-prints JSON (with syntax colors), `application/x-www-form-urlencoded` (as a key/value table) and XML (indented). Other text is shown raw, and binary shows "N bytes (binary)". A truncated body shows a "truncated at 1 MB" notice. A summary line shows method, full URL, status, duration and timestamp.
  - Actions: **Replay** (POST replay, then select the new entry), **Edit & Replay** (an inline editor for method, URI, headers as `Name: value` lines, and body, with Send and Cancel), and **Copy as cURL** (fetch `/curl`, `navigator.clipboard.writeText`, and a "Copied" toast). A 409 response shows its message in a toast and opens the editor.
  - Live updates: `EventSource('/api/events')` prepends a summary, and it reconnects with the status dot going grey while disconnected. The initial load uses `GET /api/requests`.
  - An empty state shows "Waiting for requests…" with the tunnel URL and a sample `curl` line.
  - Styling: CSS variables with light and dark themes via `prefers-color-scheme`, a system font stack, a monospace font for bodies, and nothing external.
  - Bodies decode from base64 with `TextDecoder('utf-8', {fatal:true})`. A failed decode is treated as binary.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/inspector/`. Expected: PASS. Then check by hand: run a throwaway program from the scratchpad (`inspector.New` wrapping an echo handler and serving `UI()` on 127.0.0.1:4040), send a JSON POST, a form POST and an XML POST, and use Replay, Edit & Replay and Copy as cURL in a browser. Take screenshots.
- [ ] **Step 5: Commit + push** `feat(inspector): inspector web UI`

---

### Task 8: CLI and client integration

**Files:**
- Modify: `internal/client/listener.go`, `internal/client/client.go`, `internal/client/target.go`, `cmd/passway/http.go`, `cmd/passway/main.go` (usage line), `internal/config/config.go` only if needed
- Test: `internal/client/target_test.go`, `internal/client/client_test.go`, `cmd/passway/main_test.go`

**Interfaces:**
- Consumes: `inspector.New`, `(*Inspector).Handler`, `UI`, `SetPublicURL` and `SetTarget` from Tasks 6 and 7.
- Produces:
  ```go
  // client
  func NewProxy(target *url.URL, rewriteHost bool, log *slog.Logger) http.Handler // was newProxy(cfg, log)
  // Config.Handler, when non-nil, replaces NewProxy(cfg.Target, cfg.RewriteHost, log) as the
  // handler for tunnel streams (OnRequest logging still wraps it).
  Handler http.Handler
  // ErrNotPassway is returned (fatal) when the first-ever dial gets an HTTP response other than 101.
  var ErrNotPassway = errors.New("client: server is not a Passway server")

  // cmd/passway
  // listenInspector binds addr; if addr == defaultInspectAddr and it is in use, tries
  // 127.0.0.1:4041..4049. A non-loopback host is rejected before binding.
  func listenInspector(addr string) (net.Listener, error)
  const defaultInspectAddr = "127.0.0.1:4040"
  ```
  The CLI flags are `--inspect` (bool, default true) and `--inspect-addr` (default `defaultInspectAddr`). The status block becomes:
  ```
  Passway dev                                   (Ctrl+C to quit)

  Forwarding   https://x.localhost:8080 -> http://localhost:3000
  Inspector    http://127.0.0.1:4040

  ```
  With `--inspect=false`, the Inspector line is omitted and no listener is opened.

- [ ] **Step 1: Write the failing tests**
  - `target_test.go`: add `":3000"` → `http://localhost:3000` to the table.
  - `client_test.go`:
    - `TestRunNotPasswayServer`: `client.Run` against an `httptest.Server` that 404s everything returns an error that matches `errors.Is(err, ErrNotPassway)` within 5s. Its message includes `HTTP 404`.
    - `TestRunCustomHandler`: `Config.Handler` set to a handler that writes `custom` causes a public GET to return `custom`.
  - `main_test.go`:
    - `TestRunHTTPInspector`: `--inspect-addr 127.0.0.1:0` (for the test, the ephemeral port is allowed). Wait for the `Inspector    http://127.0.0.1:` line, send a public GET through the tunnel, then GET `<inspector>/api/requests` with 1 entry.
    - `TestRunHTTPInspectorRejectsPublicAddr`: `--inspect-addr 0.0.0.0:4040` → exit 2 with "inspector must listen on a loopback address".
    - `TestListenInspectorFallback`: hold 127.0.0.1:4040 (skip if it can't be bound), and `listenInspector(defaultInspectAddr)` returns a listener on 4041–4049. An explicit taken address returns an error.
    - `TestRunHTTPInspectDisabled`: `--inspect=false` output has no `Inspector` line.
    - `TestRunHTTPTokenPrecedence`: a config token `cfgtok` and `t.Setenv("PASSWAY_TOKEN","envtok")` against a server with `--tokens envtok` connect, and with only the config token set against `--tokens cfgtok` they also connect. Use the existing harness in `main_test.go`.
    - Fix `TestRunHTTPFlagsAfterTarget` so it asserts the printed URL contains the `--name` value.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/client/ ./cmd/passway/`. Expected: FAIL.
- [ ] **Step 3: Implement**
  - `ParseTarget`: a leading `:` gets `localhost` prepended.
  - `NewProxy` is exported, `newHTTPServer` uses `cfg.Handler` when it's set.
  - In `once`, a dial error whose `*http.Response` is non-nil with a status other than 101 is wrapped as `fmt.Errorf("%w (HTTP %d from %s)", ErrNotPassway, resp.StatusCode, c.cfg.Server)`, but only while the runner has never connected (add a `connectedOnce bool` to `runner`). `Run` treats `ErrNotPassway` as fatal.
  - `runHTTP` builds `proxy := client.NewProxy(...)`. When inspecting, it also builds `insp := inspector.New(inspector.DefaultCapacity, proxy)`, `insp.SetTarget(target.String())`, and serves `insp.UI()` on the listener with `http.Server{ReadHeaderTimeout: 10s}`. It's shut down when ctx ends. `client.Config{Handler: insp.Handler()}`, and `OnReady` also calls `insp.SetPublicURL(r.URL)`.
  - Update the `usage` strings in `main.go` and `http.go`.
- [ ] **Step 4: Run** the same command and then `./...`. Expected: PASS.
- [ ] **Step 5: Commit + push** `feat(cli): local inspector on :4040, clearer server errors, :port targets`

---

### Task 9: End-to-end coverage, README and the M2 demo

**Files:**
- Modify: `internal/e2e/e2e_test.go`, `README.md`
- Create: `internal/e2e/testdata/stripe-checkout-completed.json` (a realistic Stripe `checkout.session.completed` event, about 40 lines, with fake IDs)

**Interfaces:**
- Consumes: everything above. `opts` gains `inspector bool`, `rateLimit int` and `browserWarning bool`. `setupEnv` passes them into `server.Config` and, when inspecting, into `client.Config{Handler: insp.Handler()}`. `env` gains `insp *inspector.Inspector`.

- [ ] **Step 1: Write the failing tests**
  - `TestInspectorCaptureAndReplay`: POST the Stripe fixture with `Stripe-Signature` to `/webhooks/stripe`. The local app counts hits and records the bodies. The store has 1 exchange with the fixture body and status 200. `insp.Replay(ctx, id, nil)` brings the app's hit count to 2 with an identical body and the same `Stripe-Signature`.
  - `TestInspectorStreamingStillWorks`: the existing streaming handler, with the inspector on, still yields the first chunk before the handler finishes.
  - `TestInspectorWebSocket`: the existing WebSocket echo works with the inspector on, and after the close the exchange has `Upgraded`.
  - `TestRateLimitE2E`: `rateLimit: 3` gives 3×200 and then a 429 with `Retry-After`.
  - `TestBrowserWarningE2E`: a browser GET gets the warning page. Following the continue link with the jar-enabled client reaches the app on the original path. A webhook POST without the cookie reaches the app.
  - Replace every bare `<-ch` in the file with a `recvTimeout(t, ch, 5*time.Second)` helper.
- [ ] **Step 2: Run** `go test -race -count=1 ./internal/e2e/`. Expected: the new tests fail only where wiring is missing (`opts` fields). Add the wiring until they pass. If a test fails for a behaviour reason, fix the owning package and add a unit test there.
- [ ] **Step 3: Update README.md**
  - Add an "Inspect & replay webhooks" section with the 4040 URL, a description of Replay, Edit & Replay and cURL, and a screenshot placeholder `docs/img/inspector.png` (TODO for the GIF in week 4).
  - Document the new CLI flags `--inspect` and `--inspect-addr`.
  - Document the new server flags `--rate-limit`, `--max-conns-per-tunnel` and `--browser-warning`.
  - Add one line on the subdomain blocklist.
- [ ] **Step 4: Verify.** Run `go vet ./... && go test -race -count=1 ./...` and expect all packages to pass. Then do the demo dry-run for the owner:
  1. Run `go run ./cmd/passway-server --domain localhost --rate-limit 60 --browser-warning`.
  2. Start a local echo app on :3000 and run `go run ./cmd/passway http 3000`.
  3. Run `curl -X POST <url>/webhooks/stripe -H 'Content-Type: application/json' -H 'Stripe-Signature: t=…' --data-binary @internal/e2e/testdata/stripe-checkout-completed.json`.
  4. Open http://127.0.0.1:4040 and inspect, Replay, Edit & Replay, and Copy as cURL.
  5. Open the URL in a browser to see the warning page.
  6. Stop the client to see the offline page.
- [ ] **Step 5: Commit + push** `test(e2e): inspector, rate limit and warning coverage; docs for M2`

---

## Deferred (not this week)

- Bandwidth caps, plan-based limits, the "@user" owner on the warning page, the `X-Passway-Tunnel` account header and the CLI message when a limit is hit all need accounts, so they move to week 3.
- Showing in-flight requests in the inspector before they complete moves to after launch.
