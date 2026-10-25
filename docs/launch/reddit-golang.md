<!-- Draft for r/golang, for the owner to edit. Check the subreddit's rules on
     self-promotion before posting. Replace <domain>. -->

# r/golang

## Title

I wrote an open-source ngrok alternative in Go, focused on inspecting and replaying webhooks

## Post

I've been building Passway, a tunnel that exposes a local HTTP server at a
public URL, with a local inspector for replaying requests. The code is at
https://github.com/mohammed19n/passway. Here are some Go details people here
might find interesting.

**Transport.** The CLI dials `wss://<server>/_tunnel` with
`github.com/coder/websocket` and wraps the connection as a `net.Conn`. It runs
a `hashicorp/yamux` session over that. Each public request becomes a yamux
stream that the client serves with a normal `http.Handler`, so streaming
bodies, WebSockets (hijack) and cancellation mostly come from net/http for
free. The control messages are a small versioned JSON protocol.

**Inspector.** The capture is an `http.Handler` middleware that tees request
and response bodies into a capped in-memory ring. Bodies are cut at 1 MB in
the store but never in the proxied traffic. The UI is one `embed`ed HTML file
with no build step, and live updates come over Server-Sent Events. Replay
sends the stored bytes back through the same handler chain.

**Server.**
- `modernc.org/sqlite` for accounts, so there's no cgo and the binaries are
  static.
- `caddyserver/certmagic` for a wildcard certificate over Cloudflare DNS-01,
  plus on-demand certificates for verified custom domains.
- A small PROXY protocol (v1/v2) listener wrapper, so it can sit behind nginx
  `stream` passthrough on a shared VPS.
- Stripe billing without the SDK: a few form-encoded calls, and webhook
  signature checks with `crypto/hmac`.

**Tests.** There's an end-to-end suite that starts the server, the client
and a local app in-process. It covers forwarding, reconnects, the
inspector, and the cloud flows (dev sign-in, a fake Stripe, a fake DNS
resolver for custom domains). Everything runs under `-race` in CI.

Licences: the CLI is MIT and the server AGPL-3.0. Self-hosting is the
default mode. There's a hosted version too, at <domain>.

I'd welcome code review. The packages are `internal/client`,
`internal/inspector`, `internal/protocol` and `internal/server`.
