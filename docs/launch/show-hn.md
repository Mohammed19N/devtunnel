<!-- Draft for the owner to edit before posting. Replace <domain>. Post as
     "Show HN" with the URL https://github.com/mohammed19n/passway, then add
     the first comment straight away. -->

# Show HN

## Title (at most 80 characters)

Show HN: Passway – an open-source, self-hostable tunnel for debugging webhooks

<!-- 78 characters. Alternative (74): Show HN: Passway – a self-hostable ngrok alternative for webhook debugging -->

## URL

https://github.com/mohammed19n/passway

## First comment

Hi HN, I built Passway. You run `passway http 3000`, it gives your local app
a public HTTPS URL, and a local inspector at localhost:4040 shows every request
that comes through. You can replay any request, edit it first, or copy it as
cURL.

Why: most of my tunnel use is webhooks. A Stripe or GitHub event hits my
laptop and my handler throws. Then I want the exact request again, signature
header and all, without going back to the provider's dashboard to trigger
another event. Tools that do this exist, but I wanted one that is open source
and that I could run on my own server. I also wanted a free tier whose limits
are written down.

What it is:

- Two Go binaries. The CLI is MIT licensed, the server AGPL-3.0.
- The CLI opens one outbound WebSocket over 443 to the server and multiplexes
  requests over it with yamux. Bodies stream, and WebSockets and large uploads
  work.
- The inspector is a single embedded HTML page with no build step. Replay
  resends the captured bytes, so `Stripe-Signature` still verifies if your
  handler's tolerance allows it.
- Self-hosted is the default mode: one binary or Docker image, wildcard DNS,
  and either your own certificate or automatic ones through Cloudflare DNS-01.
  No accounts and no limits.
- There's also a hosted version at <domain>. It has GitHub sign-in, a stable
  subdomain on Free, and Pro (Stripe) with more tunnels and custom domains.
  The limits for each plan are in the README.

Abuse is the hard part of running a public tunnel. Free tunnels show browser
visitors a one-time warning page naming the account owner. Webhook and API
requests are never interrupted. Subdomains with brand or phishing words are
refused, and sign-up needs a GitHub account at least two days old.

It's new, and I'm one person, so expect rough edges. I'd especially like
feedback on the inspector and on self-hosting. If something breaks, please
open an issue.
