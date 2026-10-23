# Passway

**An open-source, self-hostable ngrok alternative built for webhook debugging.**
One command gives a local app a public URL.

> **Status: early development.** Milestone 1 (the core tunnel) and milestone 2
> (request inspector, replay and basic abuse protection) work: you can run your
> own relay server, expose a local HTTP app through it and inspect the traffic.
> Expect rough edges and breaking changes.

## What works today

- `passway http <port>` exposes a local HTTP app at `http://<name>.<your-domain>`
- Streaming request and response bodies
- Automatic reconnect, keeping the same public URL
- Token authentication (`--tokens`) and optional TLS on the relay server
- A friendly 502 page when the tunnel is offline
- A local request inspector with replay (see below)
- Per-tunnel rate limiting, a connection cap and an optional browser warning page

## Quickstart (local)

Requires the Go version listed in `go.mod`.

```sh
git clone https://github.com/Mohammed19N/devtunnel
cd devtunnel
```

In one terminal, start the relay server:

```sh
go run ./cmd/passway-server --domain localhost
```

In another, start something to expose on port 3000, for example:

```sh
python3 -m http.server 3000
```

In a third, open the tunnel:

```sh
go run ./cmd/passway http 3000
```

The CLI prints a public URL such as `http://brave-marmot-93.localhost:8080`.
Request it:

```sh
curl http://<name>.localhost:8080/
```

`curl` resolves `*.localhost` to loopback by itself. Other clients may not, so
send the tunnel's hostname as a `Host` header instead:

```sh
curl -H 'Host: <name>.localhost:8080' http://127.0.0.1:8080/
```

## Inspect & replay webhooks

`passway http <port>` also starts a request inspector at
**http://127.0.0.1:4040** (if that port is taken it tries 4041 to 4049; the
CLI prints the address it chose). An explicit `--inspect-addr` is used as given, with no fallback. Every request that goes through the tunnel
shows up there live, with headers and bodies for both the request and the
response.

![The Passway inspector](docs/img/inspector.png)

<!-- TODO(week 4): replace the screenshot with a short GIF of capture and replay. -->

- **Replay** sends a captured request to your local app again, unchanged
  (including headers such as `Stripe-Signature`). The replay appears as a new
  entry in the list.
- **Edit & Replay** lets you change the method, path, headers or body first.
  Edited bodies are sent as UTF-8 text.
- **Copy as cURL** builds a `curl` command for the captured request against
  your public tunnel URL.

The inspector keeps the last 500 requests in memory and stores at most 1 MB of
each body (larger bodies are marked truncated, but are always forwarded in
full). It listens on loopback only.

| Flag | Default | Meaning |
|---|---|---|
| `--inspect` | `true` | Run the inspector. Use `--inspect=false` to turn it off. |
| `--inspect-addr` | `127.0.0.1:4040` | Inspector listen address. Must be a loopback address. |

## Running a server

```sh
passway-server --domain tunnels.example.com \
  --tls-addr :443 --tls-cert cert.pem --tls-key key.pem \
  --addr :80 --tokens secret1,secret2
```

| Flag | Default | Meaning |
|---|---|---|
| `--domain` | (required) | Base domain. Tunnels live at `<name>.<domain>`. |
| `--addr` | `:8080` | Plain HTTP listener. With TLS enabled it redirects to HTTPS. Empty disables it. |
| `--tls-addr` | (empty) | TLS listener, e.g. `:443`. |
| `--tls-cert`, `--tls-key` | (empty) | Certificate and key files. Required with `--tls-addr`. |
| `--tokens` | (empty) | Comma-separated accepted client tokens. Required unless the domain is `localhost` (or `*.localhost`) or `--open` is given. |
| `--open` | `false` | Allow running with no `--tokens` on a non-localhost domain. That makes an open relay anyone can use; the server refuses to start otherwise. |
| `--public-port` | `0` | Port shown in public URLs, if it differs from the listener (for example behind a proxy). |
| `--rate-limit` | `0` | Maximum requests per minute for each tunnel. Over the limit, visitors get `429` with a `Retry-After` header. `0` means unlimited. |
| `--max-conns-per-tunnel` | `64` | Maximum concurrent proxied connections per tunnel. |
| `--browser-warning` | `false` | Show a one-time warning page to browsers (GET or HEAD requests that accept `text/html`) before they reach a tunnel. Webhook calls and other API traffic are not interrupted. Clients can skip it with a `Passway-Skip-Warning` header. |

The server refuses requested subdomain names (`--name`) that contain blocked
words, such as brand names and terms commonly used in phishing (`paypal`,
`login`, `bank` and similar).

You need a wildcard DNS record (`*.tunnels.example.com`) and a wildcard
certificate for the domain. Clients authenticate with `passway login <token>`
or the `PASSWAY_TOKEN` environment variable, and pick the server with
`--server`.

## Roadmap

Planned, not built yet: accounts and plans, and a hosted cloud offering.

## License

The CLI and the rest of the repository are licensed under the [MIT License](LICENSE).
The server (`cmd/passway-server`) is licensed under the
[AGPL-3.0](cmd/passway-server/LICENSE).
