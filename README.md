# Passway

**An open-source, self-hostable ngrok alternative built for webhook debugging.**
One command gives a local app a public URL.

> **Status: early development.** Milestone 1 (the core tunnel) works: you can
> run your own relay server and expose a local HTTP app through it. Expect
> rough edges and breaking changes.

## What works today

- `passway http <port>` exposes a local HTTP app at `http://<name>.<your-domain>`
- Streaming request and response bodies
- Automatic reconnect, keeping the same public URL
- Token authentication (`--tokens`) and optional TLS on the relay server
- A friendly 502 page when the tunnel is offline

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
| `--tokens` | (empty) | Comma-separated accepted client tokens. Empty means **open**, for development only. |
| `--public-port` | `0` | Port shown in public URLs, if it differs from the listener (for example behind a proxy). |

You need a wildcard DNS record (`*.tunnels.example.com`) and a wildcard
certificate for the domain. Clients authenticate with `passway login <token>`
or the `PASSWAY_TOKEN` environment variable, and pick the server with
`--server`.

## Roadmap

Planned, not built yet: a local request inspector with replay, and a hosted
cloud offering.

## License

The CLI and the rest of the repository are licensed under the [MIT License](LICENSE).
The server (`cmd/passway-server`) is licensed under the
[AGPL-3.0](cmd/passway-server/LICENSE).
