<!-- Draft for Product Hunt, for the owner to edit. Replace <domain>. Media:
     docs/img/demo.gif (gallery), docs/img/inspector.png (thumbnail or
     gallery). -->

# Product Hunt

## Name

Passway

## Tagline (at most 60 characters)

Open-source tunnels for debugging webhooks on localhost

<!-- 55 characters -->

## Links

- Website: https://<domain>
- GitHub: https://github.com/mohammed19n/passway

## Description (at most 260 characters)

Passway gives your local app a public HTTPS URL with one command. Its local
inspector shows every request, such as a Stripe or GitHub webhook, and lets you
replay or edit it. Open source, self-hostable, with a free hosted plan.

<!-- 227 characters -->

## Topics

Developer Tools, Open Source, GitHub

## Maker's first comment

Hi everyone, I'm the maker of Passway.

I made it because most of my time with tunnels goes into debugging webhooks.
An event arrives, my code fails, and I want to send that exact request again
after a fix, without triggering a new event at the provider.

With Passway:

- `passway http 3000` prints a public URL for your local app.
- http://localhost:4040 lists every request live, with headers and a
  pretty-printed body.
- Replay resends a request unchanged (signature headers included). Edit &
  Replay lets you change it first, and you can copy any request as cURL.

The whole project is open source: the CLI under MIT, the server under
AGPL-3.0. You can run your own relay with one binary or Docker image. The
hosted version has:

- a Free plan: 1 tunnel, a stable subdomain, 1 GB a month;
- a Pro plan: more tunnels and bandwidth, plus custom domains.

I'd love to hear what you'd want from the inspector next.
