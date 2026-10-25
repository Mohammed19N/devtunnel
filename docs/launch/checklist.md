# Launch checklist (owner-gated)

These steps need the owner's accounts or decisions. The code and docs are
ready for them. Work roughly top to bottom: later steps depend on earlier
ones.

- [ ] **Repo rename.** Rename `Mohammed19N/devtunnel` to `passway` on GitHub.
      `install.sh`, GoReleaser, the README, the site and these drafts all use
      `github.com/mohammed19n/passway`. Make it public before posting.
- [ ] **Domain.** Buy it and put its DNS on Cloudflare. Then:
  - Create A/AAAA records for `<domain>`, `*.<domain>` and `edge.<domain>`.
    `edge` must be an A/AAAA record, not a CNAME (see docs/deploy.md).
  - Create a Cloudflare API token with Zone.DNS:Edit on that zone.
  - Replace `<domain>` in `README.md` and `docs/launch/*.md`.
- [ ] **GitHub OAuth app.** Callback `https://<domain>/auth/github/callback`.
      The client ID and secret go in `/etc/passway/env`.
- [ ] **Stripe.**
  - Create the Pro product and its monthly price (this sets the final price).
  - Turn on the Customer Portal.
  - Add a webhook endpoint at `https://<domain>/stripe/webhook` with the 5
    events listed in docs/deploy.md.
  - The price ID, secret key and webhook secret go in `/etc/passway/env`.
  - Do one end-to-end run in test mode before switching to live keys.
- [ ] **First VPS deploy.** Follow docs/deploy.md with Claude over SSH. It
      needs SSH access, the list of the existing apps' hostnames, and the
      owner's approval of the nginx diff.
- [ ] **Legal review.** Read `/terms`, `/privacy` and `/aup` on the deployed
      site. Set `PASSWAY_LEGAL_OPERATOR` and `PASSWAY_LEGAL_JURISDICTION`.
      Approve or edit the text, and make sure the abuse address receives
      mail.
- [ ] **Public Suffix List.** Submit `<domain>` at https://publicsuffix.org/
      so browsers isolate cookies between tunnels. Review takes weeks, so
      submit early. Launch doesn't need to wait for the merge.
- [ ] **Homebrew tap.** Create the public repo `mohammed19n/homebrew-tap`.
      Add a `HOMEBREW_TAP_GITHUB_TOKEN` repository secret: a fine-grained
      token with contents read/write on the tap repo only.
- [ ] **`PASSWAY_DEFAULT_SERVER`.** Set it as a repository variable
      (Settings → Secrets and variables → Actions → Variables) to
      `https://<domain>`. Release builds of the CLI use it as their default
      `--server`.
- [ ] **First tag.**
  - `git tag v0.1.0 && git push origin v0.1.0` triggers the release
    workflow: binaries, checksums, the Homebrew cask and the Docker image on
    GHCR.
  - Afterwards, make the GHCR package public.
  - Check that `curl -fsSL https://<domain>/install.sh | sh` and
    `brew install mohammed19n/tap/passway` both install a working CLI.
- [ ] **Launch posts.** Edit and post the drafts in this folder: Show HN,
      r/golang, r/selfhosted, Product Hunt. Be around for the first few
      hours to answer comments.
