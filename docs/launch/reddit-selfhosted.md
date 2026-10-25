<!-- Draft for r/selfhosted, for the owner to edit. Check the subreddit's
     self-promotion rules and flair. Replace <domain>. -->

# r/selfhosted

## Title

Passway: a self-hostable tunnel (ngrok-style) with a request inspector for webhooks. One binary or Docker image.

## Post

I've released Passway, an open-source tunnel server and CLI:
https://github.com/mohammed19n/passway

You run `passway http 3000` on your laptop and get `https://<name>.your-domain`
forwarding to it. A local inspector on port 4040 shows every request and lets
you replay it. I built it mainly for webhook work: Stripe, GitHub and
similar.

**What self-hosting looks like:**

- One static binary (`passway-server`) or the Docker image
  `ghcr.io/mohammed19n/passway-server`. Data is a single SQLite file.
- DNS: a wildcard A record (`*.tunnels.example.com`) plus the base name.
- TLS: bring your own wildcard certificate, or set a Cloudflare API token and
  it obtains and renews one over DNS-01.
- Auth: a fixed `--tokens` list, or tokens you create and revoke with
  `passway-server admin token create <name>`.
- Self-hosted is the default mode. There are no accounts, plan limits,
  phone-home or branding.
- If nginx already owns 80/443, the repo includes nginx `stream` snippets
  that pass TLS through by SNI. Your existing sites stay on nginx and keep
  their real client IPs (via the PROXY protocol). See `deploy/nginx` and
  `docs/self-hosting.md`.

```sh
docker run -d --name passway --restart unless-stopped \
  -p 80:8080 -p 443:8443 -v passway:/data \
  -e PASSWAY_CLOUDFLARE_API_TOKEN=... \
  ghcr.io/mohammed19n/passway-server \
  --domain tunnels.example.com --addr :8080 --tls-addr :8443 --public-port 443 \
  --tls-mode acme --acme-email you@example.com --db /data/passway.db
```

**Licence:** the server is AGPL-3.0 and the CLI MIT. I also run a hosted
version at <domain>, which pays for the work, but the self-hosted server is
the same code and isn't limited.

**Not there yet:** raw TCP/UDP tunnels, request history stored on the server,
and teams. Feedback on the self-hosting docs is very welcome. Tell me where
you got stuck.
