package dashboard

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/store"
)

const maxGitHubBody = 1 << 20

// ghProfile is the part of GitHub's GET /user response we use.
type ghProfile struct {
	ID        int64     `json:"id"`
	Login     string    `json:"login"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url"`
	CreatedAt time.Time `json:"created_at"`
}

// oauthError is an error response from GitHub's token endpoint.
type oauthError struct{ Code string }

func (e *oauthError) Error() string { return "github oauth: " + e.Code }

func (d *Dashboard) callbackURL() string { return d.cfg.BaseURL + "/auth/github/callback" }

// handleLogin starts the OAuth flow. No scope is requested: the public
// profile is enough.
func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	d.setCookie(w, d.stateCookie, state, int(stateTTL/time.Second))
	q := url.Values{
		"client_id":    {d.cfg.GitHubClientID},
		"redirect_uri": {d.callbackURL()},
		"state":        {state},
		"allow_signup": {"true"},
	}
	http.Redirect(w, r, d.cfg.GitHubAuthURL+"?"+q.Encode(), http.StatusFound)
}

func (d *Dashboard) handleCallback(w http.ResponseWriter, r *http.Request) {
	want, ok := readCookie(r, d.stateCookie)
	d.setCookie(w, d.stateCookie, "", -1)
	q := r.URL.Query()
	if got := q.Get("state"); !ok || got == "" || !hmac.Equal([]byte(got), []byte(want)) {
		d.fail(w, http.StatusBadRequest, "This sign-in link expired or was started in another browser. Please sign in again.")
		return
	}
	if q.Get("error") != "" {
		d.fail(w, http.StatusBadRequest, "GitHub sign-in was cancelled.")
		return
	}
	code := q.Get("code")
	if code == "" {
		d.fail(w, http.StatusBadRequest, "GitHub didn't send a sign-in code. Please sign in again.")
		return
	}
	ctx := r.Context()
	token, err := d.exchangeCode(ctx, code)
	if err != nil {
		d.log.Warn("dashboard: github token exchange failed", "err", err)
		var oe *oauthError
		if errors.As(err, &oe) {
			d.fail(w, http.StatusBadRequest, "GitHub sign-in failed. Please sign in again.")
		} else {
			d.fail(w, http.StatusBadGateway, "Couldn't reach GitHub. Please try again.")
		}
		return
	}
	p, err := d.fetchUser(ctx, token)
	if err != nil {
		d.log.Warn("dashboard: github user lookup failed", "err", err)
		d.fail(w, http.StatusBadGateway, "Couldn't read your GitHub profile. Please try again.")
		return
	}
	d.signIn(w, r, p, "github")
}

// exchangeCode trades an authorization code for an access token. Errors
// never contain the code, the token or the client secret.
func (d *Dashboard) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {d.cfg.GitHubClientID},
		"client_secret": {d.cfg.GitHubClientSecret},
		"code":          {code},
		"redirect_uri":  {d.callbackURL()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.GitHubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("token response (status %d): %w", resp.StatusCode, err)
	}
	if body.Error != "" {
		return "", &oauthError{Code: body.Error}
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("token response: status %d without a token", resp.StatusCode)
	}
	return body.AccessToken, nil
}

// fetchUser returns the profile of the token's owner.
func (d *Dashboard) fetchUser(ctx context.Context, token string) (ghProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.GitHubAPIURL+"/user", nil)
	if err != nil {
		return ghProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.client.Do(req)
	if err != nil {
		return ghProfile{}, fmt.Errorf("user request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghProfile{}, fmt.Errorf("user request: status %d", resp.StatusCode)
	}
	var p ghProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubBody)).Decode(&p); err != nil {
		return ghProfile{}, fmt.Errorf("user response: %w", err)
	}
	if p.ID <= 0 || p.Login == "" || p.CreatedAt.IsZero() {
		return ghProfile{}, errors.New("user response: missing id, login or created_at")
	}
	return p, nil
}

// signIn applies the signup rules to p, upserts the user, starts a session
// and redirects to /dashboard (or /waitlist).
func (d *Dashboard) signIn(w http.ResponseWriter, r *http.Request, p ghProfile, via string) {
	ctx := r.Context()
	if min := d.cfg.MinGitHubAge; min > 0 && d.cfg.Now().Sub(p.CreatedAt) < min {
		d.fail(w, http.StatusForbidden, ageMessage(min))
		return
	}
	n, err := d.cfg.Store.CountUsers(ctx, server.PlanFree.Name)
	if err != nil {
		d.internalError(w, "count users", err)
		return
	}
	// Waitlisted only applies to a new row; an existing user keeps their state.
	u, created, err := d.cfg.Store.UpsertGitHubUser(ctx, store.User{
		GitHubID: p.ID, Login: p.Login, Name: p.Name, AvatarURL: p.AvatarURL,
		Waitlisted: n >= d.cfg.FreeSignupCap,
	})
	if err != nil {
		d.internalError(w, "upsert user", err)
		return
	}
	// Checked on the upserted row, so a ban keyed on the user id still holds
	// after a GitHub rename.
	banned, err := d.cfg.Store.IsUserBanned(ctx, u)
	if err != nil {
		d.internalError(w, "ban lookup", err)
		return
	}
	if banned {
		d.fail(w, http.StatusForbidden, "account suspended")
		return
	}
	if created {
		if err := d.cfg.Store.Audit(ctx, u.ID, "signup", fmt.Sprintf("via=%s waitlisted=%t", via, u.Waitlisted)); err != nil {
			d.internalError(w, "audit", err)
			return
		}
	} else if u.Waitlisted && n < d.cfg.FreeSignupCap {
		// There is room again (an admin raised the cap, or Free users left).
		if err := d.cfg.Store.SetWaitlisted(ctx, u.ID, false); err != nil {
			d.internalError(w, "admit from waitlist", err)
			return
		}
		u.Waitlisted = false
		if err := d.cfg.Store.Audit(ctx, u.ID, "waitlist.admit", "via=sign-in"); err != nil {
			d.internalError(w, "audit", err)
			return
		}
	}
	// Checked on every sign-in, not just at signup, so a failed claim or a
	// user admitted from the waitlist still ends up with a default name.
	if !u.Waitlisted {
		if err := d.ensureDefaultSubdomain(ctx, u.ID); err != nil {
			d.internalError(w, "claim default subdomain", err)
			return
		}
	}
	if err := d.startSession(ctx, w, u.ID); err != nil {
		d.internalError(w, "create session", err)
		return
	}
	dest := "/dashboard"
	if u.Waitlisted {
		dest = "/waitlist"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ageMessage is the copy for an account younger than min.
func ageMessage(min time.Duration) string {
	days := int((min + 24*time.Hour - 1) / (24 * time.Hour))
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return fmt.Sprintf("GitHub accounts must be at least %d %s old to sign up", days, unit)
}

// ensureDefaultSubdomain claims a random default subdomain for a user that
// holds none.
func (d *Dashboard) ensureDefaultSubdomain(ctx context.Context, userID int64) error {
	subs, err := d.cfg.Store.Subdomains(ctx, userID)
	if err != nil || len(subs) > 0 {
		return err
	}
	for range 20 {
		err = d.cfg.Store.ClaimSubdomain(ctx, userID, server.RandomName(), true, 0)
		if !errors.Is(err, store.ErrTaken) {
			return err
		}
	}
	return err
}

var devLoginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// handleDevLogin signs in as a fake, year-old GitHub account (DevLogin only).
func (d *Dashboard) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	login := r.URL.Query().Get("login")
	if !devLoginRE.MatchString(login) {
		d.fail(w, http.StatusBadRequest, "Pass a GitHub-style login: /auth/dev?login=alice")
		return
	}
	d.signIn(w, r, ghProfile{ID: devGitHubID(login), Login: login, Name: login,
		CreatedAt: d.cfg.Now().Add(-365 * 24 * time.Hour)}, "dev")
}

// devGitHubID is always negative, so it can't collide with a real GitHub id.
func devGitHubID(login string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(login))
	return -int64(h.Sum64()>>1) - 1
}
