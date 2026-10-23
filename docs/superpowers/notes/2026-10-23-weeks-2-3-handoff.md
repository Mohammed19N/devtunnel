# Weeks 2–3 handoff: decisions to review together

*2026-10-23. You were away; I kept going as asked. Each decision below records what I chose, why, and what it costs if I'm wrong. You can overturn any of them.*

## Where things stand

| Branch | Head | Status |
|---|---|---|
| `passway-m1` (PR #1) | — | Week 1. Not merged. |
| `passway-m2` | `022fef9` | Week 2 (inspector + abuse basics). Every task reviewed, plus a whole-branch review and fixes. Not merged. |
| `passway-m3` | `7a4640a` | Week 3 (accounts, dashboard, plan limits, self-hosted mode), stacked on `passway-m2`. Every task reviewed, plus a whole-branch review and fixes. Not merged. |

All three are pushed. Nothing was merged and no PRs were opened, since those are your calls. `go test -race ./...` passes, and the code builds for Windows and macOS.

### Try the week-3 demo locally (no GitHub app needed)

```sh
go run ./cmd/passway-server --domain localhost --db ./passway.db --dev-login
# open http://localhost:8080/auth/dev?login=you → dashboard → CLI tokens → create
go run ./cmd/passway login <token>
go run ./cmd/passway http 3000          # your default subdomain, Plan line shows Free
go run ./cmd/passway-server admin --db ./passway.db set-plan you pro
# dashboard → Reserved subdomains → claim "acme"
go run ./cmd/passway http 3000 --name acme
```

## Your to-dos

1. **Buy the domain.** Spec §11 says this is needed by week 3, which is now. The GitHub OAuth callback, Stripe webhooks and the first VPS deploy all wait on it.
2. **Create a GitHub OAuth app** with the callback `https://<domain>/auth/github/callback`. Give me the client ID, and put the secret in `PASSWAY_GITHUB_CLIENT_SECRET`. For local testing you can also use a second app with the callback `http://localhost:8080/auth/github/callback`.
3. **Put the base domain on the Public Suffix List** before launch. This is the real fix for one tunnel subdomain planting cookies for another (see W2-11 and W3-10).
4. **Set the final prices and limits.** The Free and Pro numbers are still the spec's placeholders, in `internal/server/plans.go`.
5. **Merge order:** PR #1, then `passway-m2`, then `passway-m3`. Or review them as one stack.

## Week 2 decisions (inspector + abuse basics)

- **W2-1:** Replay passes its "replay of #N" link through a request-context pointer. *Cost if wrong:* a small refactor.
- **W2-2:** The inspector UI sends DELETE with a JSON content type, which keeps the local API's CSRF guard simple. *Cost:* none.
- **W2-3:** The dial-timeout test is designed so the server's connection open really blocks. *Cost:* the test could flake.
- **W2-4:** The subdomain blocklist matches substrings, as spec §6.8 asks ("can't contain"). *Cost:* a few legitimate names get refused (anything containing `bank` or `secure`), and the list is easy to trim.
- **W2-5:** The page footer links to passway.run, even on self-hosted servers. *Cost:* self-hosters may want a flag to remove it.
- **W2-6:** The inspector API requires a JSON content type on DELETE too. *Cost:* none.
- **W2-7:** The inspector listens on any loopback address (`127.0.0.0/8`, `::1`, `localhost`). *Cost:* none.
- **W2-8:** An HTTP 502, 503 or 429 on the first connect is retried, not reported as "not a Passway server", because a reverse proxy in front of the server may be restarting. *Cost:* a URL that keeps returning 5xx is retried forever. It is still logged.
- **W2-9:** The demo rehearsal was run by script, with screenshots. *Cost:* none.
- **W2-10:** The final fix round included the cheap "should fix" items as well as the serious ones. *Cost:* a bigger diff to review.
- **W2-11:** The browser-warning cookie is signed with a server secret, tied to the tunnel's address. The secret is now persisted when `--db` is set. *Cost:* without `--db`, visitors see the warning again after each restart.
- **W2-12:** A "browser navigation" is detected by the `Sec-Fetch-*` headers for any method, so an auto-submitted form POST also gets the warning. *Cost:* a webhook sender that sent `Sec-Fetch-Mode: navigate` would get the warning page.
- **W2-13:** Webhook-provider brands (`stripe`, `github`) are not blocked as subdomain words, because names like `stripe-hooks` are exactly what users pick. *Cost:* phishing names using those brands are allowed.
- **W2-14:** The demo screenshot is committed as `docs/img/inspector.png`. *Cost:* it gets replaced by the GIF in week 4.
- **W2-15:** These were left for later: a memory budget for stored requests, decoding gzip bodies for display, look-alike spellings in the blocklist, a fast 503 when the connection cap is full, bidi-character spoofing, and the logo mismatch. *Cost:* they are carried into weeks 3–4.
- **W2-16:** The warning-cookie leftover was fixed in week 3 Task 1.

## Week 3 decisions (accounts, dashboard, limits, self-hosted)

- **W3-1:** I started week 3 without you reviewing the plan, as you asked. *Cost:* rework if you disagree with any choice below.
- **W3-2:** **Cloud mode is the default.** Self-hosters pass `--self-hosted`, which spec §6.9 names. *Cost:* one extra flag for self-hosters, and the error message tells them.
- **W3-3:** **No htmx.** The dashboard uses plain forms that redirect after each submit. *Cost:* htmx can be added later for polish.
- **W3-4:** **`--dev-login`** works only on localhost domains, so the demo and tests run without a GitHub app. *Cost:* a small extra surface, guarded by the localhost check.
- **W3-5:** **New GitHub accounts must be at least 7 days old** (`--min-github-age 168h`). *Cost:* some genuinely new users are refused. The flag changes it.
- **W3-6:** **Bans reach live tunnels within 15 s.** The server polls the DB, because the admin CLI is a separate process. *Cost:* up to 15 s delay.
- **W3-7:** **Takedown trail:** `admin whois <subdomain>`, plus the account and login in the "tunnel up" logs, instead of an `X-Passway-Tunnel` header. The spec allows "header or log". *Cost:* none.
- **W3-8:** **No per-visitor warning token**, because it would not stop planting. The Public Suffix List is the real fix. *Cost:* cookie tossing between tunnels stays possible until the domain is on the list.
- **W3-9:** The bans table is the only source of truth. `users.banned_at` is unused. *Cost:* a dead column.
- **W3-10:** SQLite write transactions take the lock up front (`_txlock=immediate`), because the admin CLI writes the same file. *Cost:* negligible contention.
- **W3-11:** Auth errors are split three ways:
  - an invalid token is fatal;
  - a ban or waitlist message is shown as-is;
  - a database hiccup is retried.

  *Cost:* none.
- **W3-12:** The handshake checks the account ban first. A banned subdomain requested with `--name` gets "account suspended", and default name resolution skips banned names. *Cost:* none.
- **W3-13:** Bandwidth is metered in memory and flushed to the DB every 60 s. It is also re-checked every 256 KiB during a transfer, so one long download or WebSocket is cut at the cap. *Cost:* a small per-write overhead.
- **W3-14:** Unlimited accounts in cloud mode follow `--browser-warning`. *Cost:* none.
- **W3-15:** On https only `__Host-` cookies are read, with no fallback to unprefixed names. A duplicated cookie is distrusted. *Cost:* a tunnel can force a dashboard visitor to sign in again, but can't hijack the session.
- **W3-16:** `MinGitHubAge` 0 disables the age check in the library; the 7-day default lives in the CLI flag. *Cost:* only Go code that builds the config directly can turn the check off by accident.
- **W3-17:** **User bans are tied to the account ID, not the GitHub login**, so renaming can't escape a ban and a ban can't pass to whoever takes the old login. Rows written by login before this fix are still honoured. *Cost:* none.
- **W3-18:** A self-hosted server with `--db` accepts its `--tokens` list and admin-created tokens. A cloud user's token is refused there, and an admin token is refused on the cloud. *Cost:* none.
- **W3-19:** The `--github-client-secret` flag wins over the environment variable when both are set. The docs recommend the environment variable. *Cost:* none.
- **W3-20:** `PollBansNow()` is exported for tests and admin tooling. *Cost:* a tiny API addition.
- **W3-21:** **Waitlist:** `admin admit <login>` exists. Waitlisted users are also admitted automatically at sign-in once there is room under `--free-signup-cap`. *Cost:* none.
- **W3-22:** Revoking a token disconnects tunnels using it. A revoke from the dashboard acts at once; one from the admin CLI acts within 15 s. *Cost:* none.
- **W3-23:** Restarting the CLI gets your own subdomain back immediately. Name holds are tied to the account in cloud mode. Self-hosted mode is unchanged. *Cost:* none.

### Two of my week-3 decisions that the final review corrected

- I first described the bandwidth risk as "about a minute of overshoot". It was really unlimited within one transfer, and the fix is W3-13.
- I first recorded bans by login as well as account ID, which let a ban pass to a stranger after a rename. The fix is W3-17.

## Carried into week 4 (known, not blocking)

- A waitlisted user still gets a user row, which counts toward the signup cap.
- The signup cap is soft: concurrent sign-ups at the cap can overshoot slightly.
- The dev-login GET can be triggered cross-site (localhost only).
- Old sessions are not revoked when the user signs in again.
- From week 2:
  - the stored-request memory budget;
  - gzip display;
  - look-alike spellings in the blocklist;
  - the connection-cap fast 503;
  - bidi spoofing in the inspector;
  - the logo mismatch;
  - making the footer link and blocklist configurable for self-hosters.
- `admin --db /typo token create` with no name leaves a stray empty database file.
- Spec items scheduled for week 4: Stripe, custom domains, the install script, static and legal pages, GoReleaser and Homebrew, PROXY protocol v2 with the first VPS deploy.
