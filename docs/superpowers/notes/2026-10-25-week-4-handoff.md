# Week 4 handoff: decisions to review together

*2026-10-25. Branch `passway-m4` (stacked on `passway-m3`) contains all 15 tasks from `docs/superpowers/plans/2026-10-24-passway-m4-launch.md`:*
- *each task was reviewed separately;*
- *then the whole branch was reviewed, and its findings were fixed.*

*`go test -race ./...` passes. Nothing is merged and no PR is opened.*

## Decisions I made on your behalf

Each line gives the decision, the reason, and what it costs if I was wrong. You can overturn any of them.

- Task 15 TestCloudUpgradeViaStripe asserts exactly one of the two clients ends with limit_reached after downgrade, not a specific one — reconnect order is racy — cost if wrong: none (weaker but correct assertion).
- Task 13 abuse RateLimiter reuse — dashboard already imports server; uses server.RateLimiter.AllowN — cost: none.
- short brand words (chase, square, steam, zoom, dhl, usps) stay substring-matched per R3 — owner asked to ban brands — cost if wrong: innocent names like "purchase-x"/"squares" refused in cloud; trimming the list is a one-line change
- TestTunnelConnCap changed from "over-cap requests queue → all 200" to "2×200 + 3×503" — the owner-approved fast-503 carry-over replaces queueing — cost if wrong: bursty clients see 503 instead of waiting
- Task 7 BillingTx.SetPlan("pro") un-waitlists AND claims a default subdomain inside the same tx (server.RandomName, ≤20 retries, only if none) — plan said "same admit path as admin admit" but admin admit only clears the flag and the dashboard claims the default at next sign-in; a payer may never re-sign-in — cost if wrong: small duplication of ensureDefaultSubdomain logic in store
- Task 7 same-second event tie — an equal-created event can't downgrade Pro (except .deleted) — checkout emits subscription.created(incomplete) in the same second it activates — cost if wrong: a genuine same-second downgrade lands one event later
- Task 7 events about a non-current subscription can't downgrade — protects re-subscribers from old-subscription events — cost: none
- Task 7 BillingTx gains Plan() and Audit() — Store.Audit would deadlock inside the single-conn tx — cost: none
- Task 7 TestOpenMigratesOnce now counts embedded files instead of hard-coding 1 — cost: none
- Task 7 "unlimited drops to free on cancel" concern moot — admin set-plan only allows free|pro in cloud — cost: none
- Task 7 only payment_status=paid grants Pro at checkout; async/trial activation still arrives via subscription.updated status active/trialing — cost: none at launch (no trials)
- promote reviewer Minor "pro event for an old subscription replaces stored subscription ID" to the fix round — it lets a later real deletion be ignored → unpaid Pro (revenue leak) — cost: small extra fix
- Task 9 domain changing hands — a fresh valid CNAME+TXT proof by a new user takes over a domain verified by someone else (AddDomain no longer ErrTaken on another's verified row; VerifyDomain evicts all other rows incl. verified, and closes the old owner's tunnels on that host) — plan said ErrTaken, but a stale verified row would lock out the real new owner forever and DNS proof is the authority — cost if wrong: old owner loses the domain only when the new party actually controls DNS (correct)
- Task 9 removing a domain closes live tunnels on that host — otherwise they keep serving until reconnect — cost: none
- host bans apply to custom domains too — Banned(acc, host) with a full hostname must match a "host" ban row for that hostname — otherwise abuse takedowns can't target custom domains — cost: none
- Task 11 go.uber.org/zap becomes a direct require — already in the graph via certmagic, needed to route certmagic logs into slog — cost: none (no new module downloaded)
- Task 11 newCertManager returns only the challenge handler; main composes it with redirectHandler — equivalent behaviour, tested — cost: none
- Task 13 abuse limit is a token bucket (burst 5, +1 per 12 min) not a strict 60-min window — ~9 reports can fit in an hour — cost if wrong: slightly more spam reports possible; a strict window is a small type later
- Task 13 empty --legal-jurisdiction renders "the place where the operator of this service is established" — literal brief default read badly — cost: none (owner sets flags)
- Task 13 public-page CSP adds connect-src 'self' — no scripts run; needed for Lighthouse robots check — cost: none
- Task 13 golang.org/x/net direct (already in go.sum, plan allowed it) — cost: none
- replace --admin-logins with --admin-github-ids (numeric, immutable GitHub user IDs; users.github_id) — same rename/reuse class as the M3 ban-by-login bug; admin can ban anyone — cost: owner looks up their ID once (api.github.com/users/<login>)
- nginx client IPs — option B: front :443 stream block sends PROXY header for all upstreams; existing apps' listen lines get proxy_protocol + real_ip from proxy_protocol so every app keeps real IPs — cost: one-line change per existing app server block, applied in the scheduled window with rollback
- proxyproto now accepts PROXY v1 as well as v2 (d13442c) — nginx proxy_protocol only emits v1 (verified on nginx 1.27); v2-only would refuse every connection behind nginx; reverses Task 10's TestProxyV1Rejected — cost: none (header still mandatory, parsing stricter-than-lenient expected; review checks it)
- Dockerfile WORKDIR /data so nonroot image can create its DB — cost: none
- promote the §4.4 :443-listener grep miss to the fix round — a missed listener breaks the prod reload — cost: one-line doc change
- final fix wave includes I1, I2, M1 (release guard), M2 (no 2nd checkout for Pro — money), M4 (doc), M5 (wal/shm restore), M6 (certmagic solver ports — would break issuance behind nginx), FLAKE3, FLAKE4 — all cheap; one dispatch — cost: larger fix diff
- M3 (old owner reconnect race at transfer) left for after launch — narrow window, ends on disconnect — cost: brief overlap

## Bugs found and fixed beyond the plan

**PROXY protocol version.** nginx's `proxy_protocol` only sends v1. A v2-only parser would have refused every connection behind nginx, so Passway now accepts both.

**Docker image.** The image couldn't create its database because `/data` was root-owned. It now uses `WORKDIR /data`.

**Handshake error message.** When the server rejected a connection, the CLI could show "session shutdown" instead of the reason (a yamux race). Fixed in `protocol.Handshake`.

**Custom domains.**
- A Pro user could repeatedly disconnect another customer's custom-domain tunnel by adding and then removing that customer's domain. Eviction is now scoped to the owner.
- A user who verifies a domain now takes it over from whoever held it, and any tunnel still running on it is closed.

**Billing.**
- A Stripe update arriving in the same second as a cancellation could bring Pro back.
- An event for an old subscription could hide a later real cancellation.
- A second browser tab could create a second subscription.

**Getting started.** The dashboard's "Getting started" told users to run `go install`, which builds a CLI that points at localhost. It now shows the install.sh and brew commands.

**Automatic HTTPS.** Certmagic's challenge solvers would have tried to bind :80 and :443 themselves. They now use the real listeners.

**Admin access.** It is keyed on numeric GitHub IDs (`--admin-github-ids`), not logins, so a re-registered login can't become admin.

**Deploy runbook.** `docs/deploy.md` now:
- tests nginx changes in place, with a guarded restore;
- refuses to apply if the live config drifted since the backup;
- catches every form of :443 listener.

**Flaky tests.** Four flaky tests were fixed at the root cause. All four were races in the tests themselves; there was no product change.

## Left for after launch (known, not blocking)

**Ownership race.** If an old owner's reconnect races a domain transfer, it can keep the domain until it disconnects.

**Abuse form limit.** The abuse form uses a token bucket, so about 9 reports fit in one hour. It is also keyed per IPv6 address rather than per /64.

**Missing tests:**
- the blocked-name paths;
- a cloud-mode automatic-HTTPS end-to-end run;
- HEAD requests to public pages, and requests with gzip disabled.

**Small fixes:**
- copy for a downgraded user's extra subdomain;
- `Price.Label` ignores `interval_count`;
- `gzipWriter.Unwrap`;
- a `ReportByID` lookup for the admin page;
- cosmetic fixes in the domains table and the docs.

**Custom-domain certificates.** Already-issued certificates keep being served after a ban or downgrade until they expire. Routing refuses those hosts and renewal is blocked.

The full list, sorted by when each needs fixing, was in the final review. It is summarised here.

## Your to-dos before launch

These are in `docs/launch/checklist.md`:
1. Domain on Cloudflare, plus an API token.
2. A GitHub OAuth app.
3. Stripe: a Pro product with a monthly price, and a webhook to `<base>/stripe/webhook` for 5 events.
4. Find your numeric GitHub ID for `--admin-github-ids`.
5. Review the legal pages, and set `--legal-operator` and `--legal-jurisdiction`.
6. Submit the domain to the Public Suffix List.
7. Rename the repo from devtunnel to passway.
8. Create the `homebrew-tap` repo and the `HOMEBREW_TAP_GITHUB_TOKEN` secret, and set the `PASSWAY_DEFAULT_SERVER` variable.
9. SSH access and the list of existing nginx hostnames. The first deploy follows `docs/deploy.md` together with you.
10. Merge PR #1, then `passway-m2`, then `passway-m3`, then `passway-m4`. Then tag the first release.
