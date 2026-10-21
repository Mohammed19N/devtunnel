# Passway M1 — Core Tunnel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the prototype with Passway's core: `passway-server` plus the
`passway http <target>` CLI. Together they forward public HTTP(S) requests,
including streaming responses, large bodies and WebSockets, through one
multiplexed WebSocket connection to a local app. The client reconnects
automatically.

**Architecture:** The CLI dials `ws(s)://<domain>/_tunnel`, wraps the WebSocket
as a `net.Conn`, and runs a yamux session over it. The first client-opened
stream is the control stream (a `Hello` and a `HelloResponse` as
length-prefixed JSON). After that, the server opens one stream per proxied
connection, and each stream starts with a `StreamHeader`. Both ends use
`httputil.ReverseProxy`. On the server, its `Transport.DialContext` opens a
yamux stream. On the client, an `http.Server` serves the accepted streams and
proxies them to the local target. Upgrades, streaming and hop-by-hop headers
come from the standard library.

**Tech Stack:** Go (per `go.mod`), `github.com/hashicorp/yamux`,
`github.com/coder/websocket`, `gopkg.in/yaml.v3`, `log/slog`, and stdlib
`net/http/httputil`.

**Spec:** `docs/superpowers/specs/2026-10-21-passway-design.md`. This plan
covers §9 week 1: §6.1 layout, §6.2 transport, §6.3 CLI (`http`, `login`,
`version`), the §6.4 offline page and reserved names, plus TLS from cert files,
and §7 e2e. Out of scope here: certmagic/ACME, the nginx/PROXY-protocol
deploy (it waits for the domain), the inspector, abuse controls, accounts and
billing.

## Global Constraints

- Module path: `github.com/mohammed19n/passway`. Keep the `go` directive already in `go.mod`.
- Licenses: MIT at the repo root (it covers the CLI, `internal/client`, `internal/protocol`, `internal/config`). AGPL-3.0 for `internal/server/` and `cmd/passway-server/`, each with its own `LICENSE` file.
- Tunnel endpoint path: `/_tunnel`. Protocol version constant: `1`.
- Reserved subdomains that can never be claimed: `www`, `api`, `app`, `edge`, `_tunnel`, `admin`, `status`, `docs`.
- Until accounts exist, the server uses **self-hosted behaviour**: a random name when `--name` is omitted, kept across reconnects.
- The config file is `os.UserConfigDir()/passway/config.yaml`. The env var `PASSWAY_TOKEN` overrides the file's token.
- Logging uses `log/slog` only.
- Every test command runs with `-race`.
- **Commit messages must not contain any `Co-Authored-By:` or other attribution trailer** (owner's global rule).

## Review Focus

1. **The local app isn't running.** The public caller should get a fast `502` that says Passway couldn't reach the target, not a hang. Test: Task 6, `TestLocalAppDown`.
2. **The public caller disconnects mid-request.** The local app's request context should be canceled (no leaked streams). Test: Task 6, `TestCallerCancelPropagates`.
3. **The Host header has a port and mixed case** (`Quick-Otter-42.LOCALHOST:8080`). It should still route to the right tunnel. Test: Task 6, `TestHostNormalization`.
4. **The client quits and another client wants the name.** The name is held for its old session only, and becomes free after the hold expires. Test: Task 3, `TestHoldExpires` / `TestHoldBlocksOthers`.
5. **The server restarts while a client is connected.** The client reconnects and keeps its URL, because it sends `PreferredName` and the fresh server grants it if the name is free. Test: Task 4, `TestPreferredNameGrantedOnFreshServer`.

---

### Task 1: Restructure repo, module rename, licenses

**Files:**
- Delete: `main.go`, `internal/proxy/`, `internal/tunnel/`, `localServer/`
- Modify: `go.mod` (module line), `.gitignore` (add `/dist/`)
- Create: `LICENSE` (MIT), `internal/server/LICENSE` and `cmd/passway-server/LICENSE` (AGPL-3.0 full text from https://www.gnu.org/licenses/agpl-3.0.txt), `internal/protocol/doc.go` (package comment only)

- [ ] **Step 1:** Run `git status --short`. If `main.go` or `internal/tunnel/*` have uncommitted changes (they did when this plan was written: the owner's v0.3 TCP experiments), **stop and ask the owner** whether to commit them as their WIP first. Never discard them silently.
- [ ] **Step 2:** Delete the old code. Set `module github.com/mohammed19n/passway`. Add the license files. The MIT copyright line is `Copyright (c) 2026 mohammed19n`.
- [ ] **Step 3:** Run `go build ./... && go vet ./...`. Expected: no output, exit 0.
- [ ] **Step 4: Commit**
```bash
git add -A
git commit -m "chore: restructure repo as Passway, add licenses"
```

### Task 2: Protocol package

**Files:**
- Create: `internal/protocol/protocol.go`
- Test: `internal/protocol/protocol_test.go`

**Interfaces:**
- Produces:
```go
const Version = 1
const TunnelPath = "/_tunnel"
const MaxFrameSize = 64 << 10
const KindHTTP = "http"
const (
    CodeVersionMismatch = "version_mismatch"
    CodeUnauthorized    = "unauthorized"
    CodeInvalidName     = "invalid_name"
    CodeNameTaken       = "name_taken"
    CodeInternal        = "internal"
)
type Hello struct {
    Version       int    `json:"version"`
    Token         string `json:"token,omitempty"`
    Name          string `json:"name,omitempty"`           // explicit; must be granted or fail
    PreferredName string `json:"preferred_name,omitempty"` // best effort (reconnect)
    SessionID     string `json:"session_id"`               // random per CLI process
    ClientVersion string `json:"client_version,omitempty"`
}
type Ready struct{ URL, Name string }        // json tags "url","name"
type Error struct{ Code, Message string }    // json tags "code","message"; implements error: "<code>: <message>"
type HelloResponse struct {
    Ready *Ready `json:"ready,omitempty"`
    Error *Error `json:"error,omitempty"`
}
type StreamHeader struct{ Kind string `json:"kind"` }
func WriteFrame(w io.Writer, v any) error // 4-byte big-endian length + JSON
func ReadFrame(r io.Reader, v any) error  // rejects length > MaxFrameSize
func IsFatal(err error) bool              // true for *Error with code version_mismatch, unauthorized, invalid_name, name_taken
```

- [ ] **Step 1: Write failing tests:**
  - `TestFrameRoundTrip`: write a `Hello{Version:1, Token:"t", SessionID:"s"}` to a `bytes.Buffer`, read it back, and require deep-equal.
  - `TestFrameTooLarge`: a buffer with a length prefix of `MaxFrameSize+1` makes `ReadFrame` return an error.
  - `TestFrameTruncated`: a length prefix of 10 followed by 3 bytes makes `ReadFrame` return an error.
  - `TestErrorString`: `(&Error{Code:"name_taken", Message:"x"}).Error() == "name_taken: x"`.
  - `TestIsFatal`: true for `&Error{Code: CodeUnauthorized}` and for a wrapped one (`fmt.Errorf("%w", ...)`). False for `CodeInternal` and for `io.EOF`.
- [ ] **Step 2:** Run `go test -race ./internal/protocol/`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement. `WriteFrame` marshals first and errors if the result is larger than `MaxFrameSize`.
- [ ] **Step 4:** Run `go test -race ./internal/protocol/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(protocol): add framing and handshake messages`

### Task 3: Names and registry

**Files:**
- Create: `internal/server/names.go`, `internal/server/registry.go`
- Test: `internal/server/names_test.go`, `internal/server/registry_test.go`

**Interfaces:**
- Consumes: `protocol.Error`, `protocol.Code*`
- Produces:
```go
func ValidateName(name string) error // *protocol.Error{Code: CodeInvalidName} on failure
func RandomName() string             // "<adjective>-<animal>-<10..99>", always passes ValidateName
type Tunnel struct {
    Name      string
    SessionID string
    // unexported: session *yamux.Session, proxy *httputil.ReverseProxy, transport *http.Transport (added in Tasks 4 and 6)
}
type ClaimRequest struct{ Name, PreferredName, SessionID string }
func NewRegistry(hold time.Duration) *Registry
func (r *Registry) Claim(req ClaimRequest, t *Tunnel) error // sets t.Name, t.SessionID on success
func (r *Registry) Release(t *Tunnel)                       // no-op unless t is the active holder; starts the hold
func (r *Registry) Lookup(name string) (*Tunnel, bool)
func (r *Registry) All() []*Tunnel
```
- `ValidateName` rule: matches `^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$` (3–32 chars) and isn't in the reserved list from Global Constraints.
- The `Registry` has an unexported `now func() time.Time` field, so tests can swap the clock.
- `Claim` rules, in order:
  1. `Name` set: it must be valid. If it's active, or held by a different SessionID, return `CodeNameTaken`. Otherwise grant it.
  2. Else, `PreferredName` set, valid, and either free or held by the same SessionID: grant it.
  3. Else: a `RandomName()` that is neither active nor held. Retry on collision, up to 100 times, then return `CodeInternal`.
  - A hold is released when the same SessionID claims the name, or when it expires (`now() > releasedAt + hold`).

- [ ] **Step 1: Write failing tests:**
  - `TestValidateName`: valid: `"abc"`, `"quick-otter-42"`, a 32-char name. Invalid: `"ab"`, a 33-char name, `"-abc"`, `"abc-"`, `"ABC"`, `"a.b"`, `"www"`, `"admin"`, `"docs"`.
  - `TestRandomNameValid`: 1000 calls all pass `ValidateName`.
  - `TestClaimExplicit`: a claim for `"acme"` succeeds and `Lookup("acme")` returns the same `*Tunnel`. A second claim for `"acme"` from another session returns an error with code `name_taken`.
  - `TestClaimRandomWhenEmpty`: a claim with an empty request sets a non-empty valid `t.Name`.
  - `TestHoldBlocksOthers`: claim `"acme"` as session A, then release. A claim of `Name:"acme"` from session B returns `name_taken`. A claim of `PreferredName:"acme"` from B gets a different random name.
  - `TestHoldSameSession`: after release, session A claiming `PreferredName:"acme"` gets `"acme"`.
  - `TestHoldExpires`: release, then advance the fake clock past the hold. Session B claiming `Name:"acme"` succeeds.
  - `TestReleaseStaleTunnel`: A claims `"acme"` and B claims it after expiry. `Release(tunnelA)` must not remove B (`Lookup` still returns B).
- [ ] **Step 2:** Run `go test -race ./internal/server/ -run 'Name|Claim|Hold|Release'`. Expected: FAIL.
- [ ] **Step 3:** Implement with a `sync.Mutex`, an `active map[string]*Tunnel`, and `held map[string]hold{sessionID string; releasedAt time.Time}`. The word lists for `RandomName` are about 50 adjectives and 50 animals, all lowercase ASCII.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): add subdomain validation and tunnel registry`

### Task 4: Server control endpoint and handshake

**Files:**
- Create: `internal/server/server.go`, `internal/server/auth.go`, `internal/server/control.go`
- Test: `internal/server/control_test.go`

**Interfaces:**
- Consumes: Task 2 and Task 3
- Produces:
```go
type Authenticator interface {
    Authenticate(ctx context.Context, token string) (accountID string, err error) // err → CodeUnauthorized
}
type StaticTokens []string // empty slice = allow every token (dev only; New logs a warning)
type Config struct {
    Domain       string        // e.g. "localhost", "passway.dev"; lowercased by New
    PublicScheme string        // "http" or "https"
    PublicPort   int           // 0 = scheme default (omitted from URLs)
    Auth         Authenticator // nil = StaticTokens(nil)
    NameHold     time.Duration // 0 = 2 * time.Minute
    Logger       *slog.Logger  // nil = slog.Default()
}
func New(cfg Config) *Server
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) // Task 4: only the base-domain routes; Task 6 adds subdomains
func (s *Server) PublicURL(name string) string                    // "http://acme.localhost:8080", "https://acme.passway.dev"
func (s *Server) CloseTunnels()                                   // closes every active session; server keeps accepting
```
- Base-domain routes (host == Domain after lowercasing and stripping the port): `GET /healthz` → 200 `ok`, and `TunnelPath` → the control handler. Anything else → 404.
- Control handler:
  1. `websocket.Accept`, then `c.SetReadLimit(-1)`. **This is required.** The default 32 KB message limit breaks yamux frames.
  2. `websocket.NetConn(r.Context(), c, websocket.MessageBinary)`, then `yamux.Server(conn, cfg)`. The config has keepalive on, a 15s interval, and `LogOutput: io.Discard`.
  3. `AcceptStreamWithContext` with a 10s timeout. Read the `Hello` with a 10s read deadline.
  4. Check in order: version, auth, `Claim`. Each failure writes `HelloResponse{Error}` and closes.
  5. On success, write `HelloResponse{Ready{URL: PublicURL(name), Name}}`. Then block until the session closes. EOF on the control stream also closes the session.
  6. Finally `Release` the tunnel and close idle proxy connections.

- [ ] **Step 1: Write failing tests.** Use a test helper `dialRaw(t, ts *httptest.Server, hello protocol.Hello) (protocol.HelloResponse, *yamux.Session)` that does the client side by hand, and `httptest.NewServer(srv)`. Because the test dials `127.0.0.1`, the helper sets `Host` to the configured domain: use `Config{Domain: "localhost", PublicScheme: "http", PublicPort: <ts port>}` and dial `ws://localhost:<port>/_tunnel`.
  - `TestHandshakeReady`: hello v1 with no name gives `Ready.URL == "http://"+Ready.Name+".localhost:<port>"`.
  - `TestHandshakeVersionMismatch`: `Version: 999` gives an error code of `version_mismatch`.
  - `TestHandshakeUnauthorized`: `Auth: StaticTokens{"good"}` with token `"bad"` gives `unauthorized`. With token `"good"` you get Ready.
  - `TestHandshakeNameTaken`: two sessions both ask for `Name:"acme"`. The second gets `name_taken`.
  - `TestPreferredNameGrantedOnFreshServer`: a fresh server and `PreferredName:"quick-otter-42"` gives `Ready.Name == "quick-otter-42"`.
  - `TestDisconnectReleases`: claim `"acme"` as session A, then close the client session. Within 1s (poll), a new session A claiming `Name:"acme"` succeeds.
  - `TestHealthz`: `GET http://localhost:<port>/healthz` returns 200 with body `ok`.
- [ ] **Step 2:** Run `go test -race ./internal/server/ -run 'Handshake|Preferred|Disconnect|Healthz'`. Expected: FAIL.
- [ ] **Step 3:** Implement. Run `go get github.com/hashicorp/yamux github.com/coder/websocket`.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): add tunnel control endpoint and handshake`

### Task 5: Client connect, local proxy, reconnect

**Files:**
- Create: `internal/client/target.go`, `internal/client/client.go`, `internal/client/listener.go`
- Test: `internal/client/target_test.go`, `internal/client/client_test.go`

**Interfaces:**
- Consumes: Task 2. The tests use `server.New` from Task 4.
- Produces:
```go
func ParseTarget(s string) (*url.URL, error) // "3000"→http://localhost:3000, "localhost:8000"→http://localhost:8000, "https://x:9"→kept; rejects "", "abc def", "ftp://x"
type RequestLog struct {
    Method, Path string
    Status       int
    Duration     time.Duration
}
type Config struct {
    Server        string   // "http://localhost:8080" or "https://passway.dev"
    Token         string
    Name          string   // explicit --name
    Target        *url.URL
    RewriteHost   bool     // --host-header=rewrite
    Insecure      bool     // skip TLS verification (dev/self-signed only)
    ClientVersion string
    Logger        *slog.Logger
    OnReady       func(protocol.Ready)  // called after every successful (re)connect
    OnRequest     func(RequestLog)      // called once per proxied request
}
func Run(ctx context.Context, cfg Config) error // blocks; nil on ctx cancel; returns *protocol.Error when protocol.IsFatal
```
- **Session:** generate a random 16-byte hex `SessionID` once per `Run`. After the first Ready, set `PreferredName` to the last `Ready.Name` on every reconnect.
- **Dial:** change the scheme `http→ws` / `https→wss`, add `protocol.TunnelPath`, call `websocket.Dial` (with an `HTTPClient` that has `InsecureSkipVerify` when `Insecure`), then `SetReadLimit(-1)`, `NetConn`, and `yamux.Client` with the same yamux config as the server. Open the control stream, send the Hello, and read the response.
- **Serving:** `listener.go` adapts the session into a `net.Listener`. `Accept` gets the next yamux stream and reads its `StreamHeader` with a 10s deadline. It returns `KindHTTP` streams and closes any other kind. An `http.Server{Handler: logRequests(proxy), ReadHeaderTimeout: 30s}` serves the listener.
- **Local proxy:** `httputil.ReverseProxy` with `FlushInterval: -1` and a `Rewrite` that does three things:
  - `pr.SetURL(target)`.
  - Unless `RewriteHost`, it sets `pr.Out.Host = pr.In.Host`.
  - It copies `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` from `pr.In.Header` to `pr.Out.Header`. Rewrite strips them otherwise, and the server already set them.

  `ErrorHandler` writes 502 `text/plain`: `Passway couldn't reach <target>: <err>`.
- **Reconnect:** on any non-fatal failure or session close, wait with backoff and retry. The backoff starts at 500ms, doubles each time, and caps at 30s, with ±20% jitter. It resets after a successful Ready. Fatal errors are returned right away.

- [ ] **Step 1: Write failing tests:**
  - `TestParseTarget`: table test covering the mappings and rejections listed above.
  - `TestRunReady`: start `server.New` behind httptest. Run the client with a context, and `OnReady` gets a URL containing `.localhost:`. Cancelling the context makes `Run` return nil within 2s.
  - `TestRunFatalUnauthorized`: a server with `StaticTokens{"good"}` and a client token `"bad"`. `Run` returns within 2s, and `protocol.IsFatal(err)` is true.
  - `TestRunFatalNameTaken`: two clients with `Name:"acme"`. The second `Run` returns a `name_taken` error.
  - `TestRunReconnectsSameName`: after the first Ready, call `srv.CloseTunnels()`. A second `OnReady` arrives within 5s with the same `Name`.
- [ ] **Step 2:** Run `go test -race ./internal/client/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/client/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(client): add tunnel client with local proxy and reconnect`

### Task 6: Server edge forwarding and offline page

**Files:**
- Create: `internal/server/edge.go`, `internal/server/offline.html` (embedded with `//go:embed`)
- Modify: `internal/server/server.go`, `internal/server/control.go` (build the tunnel's proxy on claim)
- Test: `internal/e2e/e2e_test.go` (package `e2e`; starts `server.New` behind httptest, `client.Run`, and a local `httptest.Server`)

**Interfaces:**
- Consumes: Task 4 (`Server`, `Tunnel`), Task 5 (`client.Run`, `client.Config`)
- Produces: there are no new exported symbols. `ServeHTTP` now routes `<label>.<Domain>`.
- **Routing:** lowercase the host and strip the port.
  - Equal to `Domain`: Task 4 routes.
  - Exactly one label followed by `.Domain`: `Lookup`. If found, `t.proxy.ServeHTTP`. If not found, the offline page.
  - Anything else: 404 `unknown host`.
- **Per-tunnel proxy**, created when the claim succeeds: an `httputil.ReverseProxy` with `FlushInterval: -1` and a `Rewrite` that calls `pr.SetURL(&url.URL{Scheme: "http", Host: t.Name})`, sets `pr.Out.Host = pr.In.Host`, and calls `pr.SetXForwarded()`.
  - Its `Transport` is an `http.Transport` with `DialContext`: `session.Open()`, then `WriteFrame(StreamHeader{Kind: KindHTTP})`. Also `MaxIdleConnsPerHost: 16`, `IdleConnTimeout: 90s`, and `ForceAttemptHTTP2: false`.
  - `ErrorHandler`: the offline page.
- **Offline page:** HTML, status 502, `Content-Type: text/html; charset=utf-8`. The title is "Tunnel offline". The body names the host, contains the word `offline`, and says "If this is your tunnel, start it with `passway http <port>`." Use `html/template` so the host is escaped.

- [ ] **Step 1: Write the failing e2e tests.** A helper `setup(t, opts) (publicGet func(path) *http.Response, publicHost string, srv *server.Server)` sends requests to `127.0.0.1:<port>` and sets `req.Host` to the tunnel host.
  - `TestForwardGET`: `GET /hello?name=x` on the local app gives 200, the body `hello x`, and a custom response header `X-Test: 1` that is preserved.
  - `TestForwardLargePOST`: a 5 MB random body is echoed back, and its SHA-256 matches.
  - `TestForwardStreaming`: the local handler writes `"a"`, flushes, sleeps 300ms, then writes `"b"`. The public reader receives `"a"` less than 250ms after the response headers arrive.
  - `TestForwardWebSocket`: a local WebSocket echo server (coder/websocket). Dial it through the public host and send `"ping"`, and you receive `"ping"`.
  - `TestForwardedHeaders`: the local app sees `X-Forwarded-For` starting with `127.0.0.1`, `X-Forwarded-Host == publicHost` and `X-Forwarded-Proto == "http"`. With the default settings, `r.Host == publicHost`.
  - `TestRewriteHost`: with `RewriteHost: true`, the local app sees `r.Host == <local listener host:port>`.
  - `TestOfflineUnknown`: `nobody.localhost:<port>` returns 502, `text/html`, and a body containing `offline`.
  - `TestMultiLabelHost`: `a.b.localhost:<port>` returns 404 with the body `unknown host`.
  - `TestConcurrent`: 50 parallel GETs all return 200 with the right bodies.
  - `TestTLSForwarding`: `httptest.NewTLSServer(srv)` with `PublicScheme: "https"` and a client with `Insecure: true`. A GET through `ts.Client()` returns 200, and `X-Forwarded-Proto == "https"`.
  - `TestLocalAppDown` (Review Focus 1): the client targets a closed port. The public GET returns 502 within 3s, and the body contains `couldn't reach`.
  - `TestCallerCancelPropagates` (Review Focus 2): the local handler blocks on `<-r.Context().Done()` and signals a channel when it unblocks. The public request's context is canceled 200ms in, and the channel fires within 2s.
  - `TestHostNormalization` (Review Focus 3): `req.Host = strings.ToUpper(publicHost)` still returns 200.
- [ ] **Step 2:** Run `go test -race ./internal/e2e/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./...`. Expected: PASS for all packages.
- [ ] **Step 5: Commit** `feat(server): forward public requests through tunnels, add offline page`

### Task 7: CLI (`passway`) and config

**Files:**
- Create: `internal/config/config.go`, `cmd/passway/main.go`, `cmd/passway/http.go`
- Test: `internal/config/config_test.go`, `cmd/passway/main_test.go`

**Interfaces:**
- Consumes: `client.Run`, `client.Config`, `client.ParseTarget`, `client.RequestLog`
- Produces:
```go
// internal/config
type Config struct {
    Token  string `yaml:"token,omitempty"`
    Server string `yaml:"server,omitempty"`
}
func DefaultPath() (string, error)       // filepath.Join(os.UserConfigDir(), "passway", "config.yaml")
func Load(path string) (Config, error)   // a missing file gives a zero Config and nil
func Save(path string, c Config) error   // dir 0700, file 0600

// cmd/passway
var version = "dev"                        // set with -ldflags at release
var defaultServer = "http://localhost:8080" // set with -ldflags once the domain exists
func run(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) int
```
- **Commands** (stdlib `flag`, one `FlagSet` per subcommand):
  - `version` prints `passway <version>`.
  - `login <token>` saves the token and prints `Token saved to <path>`.
  - `http [--name N] [--server URL] [--host-header preserve|rewrite] [--insecure] <target>`.
  - No args or an unknown command prints usage to stderr and exits 2.
- **Server resolution:** `--server`, then the config's `Server`, then `defaultServer`. **Token resolution:** `PASSWAY_TOKEN`, then the config's `Token`.
- **`http` output:** a status block printed on every `OnReady`:
```
Passway <version>                                   (Ctrl+C to quit)

Forwarding   <ready.URL> -> <target>
```
  After that, one line per request: `%-7s %-40s %3d %6s` (method, path truncated to 40 characters, status, duration rounded to ms).
- **Exit codes:** 0 on Ctrl+C, 1 on a fatal tunnel error (prints `error: <message>`), 2 on usage errors.
- `main()` wires `signal.NotifyContext(os.Interrupt, syscall.SIGTERM)` and `DefaultPath()`.

- [ ] **Step 1: Write failing tests:**
  - `TestConfigRoundTrip`: `Save` then `Load` in `t.TempDir()` round-trips. The file mode is `0600`.
  - `TestConfigMissing`: `Load` of a missing path returns a zero Config and nil.
  - `TestRunNoArgs`: exit code 2, and stderr contains `usage`.
  - `TestRunVersion`: exit code 0, and stdout is `passway dev\n`.
  - `TestRunLogin`: `login abc` returns 0, and `Load(path).Token == "abc"`.
  - `TestRunHTTPBadTarget`: `http "abc def"` returns exit code 2.
  - `TestRunHTTPHostHeaderInvalid`: `--host-header bogus` returns exit code 2.
  - `TestRunHTTPForwards`: start `server.New` behind httptest and a local app. `run` the `http` command with `--server <ts.URL>` in a goroutine. Wait for stdout to contain `Forwarding`, then send a GET through the public URL and wait for the request line `GET     /x`. Cancel the context, and `run` returns 0.
- [ ] **Step 2:** Run `go test -race ./internal/config/ ./cmd/passway/`. Expected: FAIL.
- [ ] **Step 3:** Implement. Run `go get gopkg.in/yaml.v3`. Guard writes to stdout with a mutex, because `OnRequest` calls come from several goroutines.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(cli): add passway http, login and version commands`

### Task 8: `passway-server` binary, TLS, shutdown, README

**Files:**
- Create: `cmd/passway-server/main.go`, `cmd/passway-server/main_test.go`
- Modify: `README.md` (full rewrite)

**Interfaces:**
- Consumes: `server.New`, `server.Config`, `server.StaticTokens`, `(*Server).CloseTunnels`
- Produces: `func run(ctx context.Context, args []string, stderr io.Writer, ready func(addrs []string)) error`. The `ready` callback receives the bound addresses, for tests.
- **Flags:**

  | Flag | Default | Meaning |
  |---|---|---|
  | `--domain` | (required) | Base domain |
  | `--addr` | `:8080` | Plain HTTP listener |
  | `--tls-addr` | (empty) | TLS listener |
  | `--tls-cert` | (empty) | Certificate file. Required with `--tls-addr`. |
  | `--tls-key` | (empty) | Key file. Required with `--tls-addr`. |
  | `--tokens` | (empty) | Comma-separated. Empty means open, and a warning is logged. |
  | `--public-port` | `0` | Port shown in public URLs |
- **Without TLS:** `PublicScheme` is `http`, and the public port is `--public-port`, or else the `--addr` port. An `--addr` port of 80 is omitted from URLs.
- **With TLS:** `PublicScheme` is `https`, and the port comes from `--tls-addr` the same way (443 is omitted). The plain `--addr` listener serves a 308 redirect to `https://<host><uri>`. `--addr ""` disables the plain listener.
- **Shutdown:** on context cancel, call `CloseTunnels()` and then `http.Server.Shutdown` with a 10s timeout. The function returns nil.

- [ ] **Step 1: Write failing tests:**
  - `TestRunMissingDomain`: `run` returns an error that contains `--domain`.
  - `TestRunTLSWithoutCert`: `--tls-addr :0` with no cert returns an error that contains `--tls-cert`.
  - `TestRunServesHealthz`: `--domain localhost --addr 127.0.0.1:0`. Use the address from `ready`. `GET /healthz` with `Host: localhost` returns 200. Cancelling the context makes `run` return nil within 11s.
  - `TestRunRedirectsToTLS`: write a self-signed cert for `localhost` and `*.localhost` to the temp dir (with `crypto/x509`), then start with both listeners on `127.0.0.1:0`. A plain GET (not following redirects) returns 308, with `Location` starting with `https://`.
- [ ] **Step 2:** Run `go test -race ./cmd/passway-server/`. Expected: FAIL.
- [ ] **Step 3:** Implement. Rewrite `README.md` for Passway:
  - the one-line pitch from spec §1
  - a "Status: early development" note
  - a local quickstart: `go run ./cmd/passway-server --domain localhost`, a local app on :3000, `go run ./cmd/passway http 3000`, then `curl http://<name>.localhost:8080/`. Note that curl resolves `*.localhost` to loopback, and other clients may need a `Host` header.
  - a license section: MIT for the CLI, AGPL-3.0 for the server.
- [ ] **Step 4:** Run `go test -race ./... && go vet ./...`. Expected: PASS, and vet is silent.
- [ ] **Step 5:** Do the manual demo check (this is the milestone demo for the owner). In three terminals, run the server, `python3 -m http.server 3000`, and `passway http 3000`. Then run `curl -i http://<name>.localhost:8080/`. Expected: 200 with the directory listing, and a request line in the CLI. Restart the server while the CLI keeps running. The CLI reconnects within about 5s with the **same URL**. Stop the CLI, and curl now returns the 502 offline page. (A newly started CLI process has a new session, so it only gets the same name with `--name`. That's expected.)
- [ ] **Step 6: Commit** `feat: add passway-server binary and rewrite README`
