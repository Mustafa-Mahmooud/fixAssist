package web

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/github"
)

// "Sign in with GitHub", through the same GitHub App that reads code: its
// user authorization (OAuth) returns the person's GitHub id and verified
// email; the account is found by GitHub id, else linked by that email, else
// created. The state is signed and bound to the browser with a short-lived
// cookie, so a sign-in started elsewhere can't be completed here (login CSRF).

const oauthCookie = "fix_oauth"

func (a *App) githubCallbackURL() string {
	return strings.TrimRight(a.BaseURL, "/") + "/auth/github/callback"
}

func (a *App) githubSignInStart(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if a.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: nonce, Path: "/auth/github", MaxAge: 600,
		HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, a.GitHub.SignInURL(a.githubCallbackURL(), a.signState("login", nonce)), http.StatusFound)
}

func (a *App) githubSignInCallback(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if a.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(status int, msg string) {
		a.render(w, status, "login", page{Title: "Sign in", Error: msg, Data: loginData{EmailOff: a.EmailOff, GitHub: true}})
	}
	nonce, _ := r.Cookie(oauthCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/github", MaxAge: -1,
		HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if q.Get("error") != "" {
		fail(http.StatusOK, "GitHub sign-in was cancelled. You can try again, or use an email link.")
		return
	}
	parts, ok := a.checkState(q.Get("state"), "login")
	if !ok || len(parts) != 1 || nonce == nil || !hmac.Equal([]byte(parts[0]), []byte(nonce.Value)) {
		fail(http.StatusBadRequest, "That sign-in expired or was started in another browser. Please try again.")
		return
	}
	user, err := a.GitHub.SignIn(r.Context(), q.Get("code"), a.githubCallbackURL())
	if errors.Is(err, github.ErrNoVerifiedEmail) {
		fail(http.StatusOK, "Your GitHub account has no verified email address. Verify one on GitHub, or sign in with an email link.")
		return
	}
	if err != nil {
		a.Log.Warn("github sign-in", "err", err)
		fail(http.StatusBadGateway, "GitHub didn't confirm the sign-in. Please try again.")
		return
	}
	userID, err := a.Auth.SignInGitHub(r.Context(), user.ID, user.Email)
	if errors.Is(err, auth.ErrGitHubConflict) {
		fail(http.StatusConflict, "The account for "+user.Email+" is linked to a different GitHub account. Sign in with an email link instead.")
		return
	}
	if err != nil {
		a.Log.Error("github sign-in account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.startSession(w, r, userID); err != nil {
		a.Log.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.Log.Info("signed in with github", "user_id", userID, "github_login", user.Login)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// startSession signs the browser in (email link or GitHub).
func (a *App) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	cookie, err := a.Auth.CreateSession(r.Context(), userID)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: cookie, Path: "/", HttpOnly: true, Secure: a.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionTTL.Seconds()),
	})
	return nil
}
