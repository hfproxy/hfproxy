package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/nicolas/hfproxy/internal/db"
	"golang.org/x/crypto/argon2"
	"golang.org/x/oauth2"
)

const loginPage = `<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connexion · hfproxy</title><link rel="stylesheet" href="/assets/app.css"><style>body{background:#f4f5f1}.login{max-width:430px;margin:12vh auto;background:white;padding:36px;border:1px solid #e5e8e0;border-radius:20px;box-shadow:0 20px 50px #1e33200c}.brand{font-weight:800;font-size:24px;letter-spacing:-.04em}.mark{display:inline-block;vertical-align:middle;width:30px;height:30px;border-radius:9px;margin-right:9px}.muted{color:#667064;font-size:14px;line-height:1.6}label{display:block;margin:18px 0 6px;font-size:13px;font-weight:650}input{width:100%;padding:12px;border:1px solid #dce1d7;border-radius:9px;font:inherit}button,.button{display:block;width:100%;margin-top:18px;border:0;border-radius:9px;padding:12px;background:#172019;color:white;font-weight:700;text-align:center;text-decoration:none;cursor:pointer}.error{padding:12px;background:#fff0ed;border-radius:9px;color:#a93d2b;font-size:14px}.divider{border-top:1px solid #e8ebe5;margin:24px 0}</style></head><body><main class="login"><div class="brand"><img class="mark" src="/hfproxy-mark.svg" alt="">hfproxy</div><p class="muted">Connectez-vous pour accéder au proxy et à son cache de modèles.</p>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{if .Local}}<form method="post" action="/login"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><label for="username">Identifiant</label><input id="username" name="username" autocomplete="username" required><label for="password">Mot de passe</label><input id="password" type="password" name="password" autocomplete="current-password" required><button type="submit">Se connecter</button></form>{{end}}{{if .OIDC}}{{if .Local}}<div class="divider"></div>{{end}}<a class="button" href="/auth/login">Continuer avec SSO</a>{{end}}{{if not .Local}}{{if not .OIDC}}<p class="muted">Aucun fournisseur d’authentification n’est configuré.</p>{{end}}{{end}}</main></body></html>`

type loginData struct {
	Error string
	CSRF  string
	Local bool
	OIDC  bool
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if p, err := s.authenticate(r); err == nil && p != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data := loginData{Local: s.currentConfig().AuthMode == "local" || s.currentConfig().LDAPURL != "", OIDC: s.currentOIDCConfig() != nil}
	if r.Method == http.MethodGet {
		cookie, err := r.Cookie("hfproxy_login_csrf")
		if err != nil || cookie.Value == "" {
			cookie = &http.Cookie{Name: "hfproxy_login_csrf", Value: randomSecret(24), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.currentConfig().PublicURL, "https://"), MaxAge: 600}
			http.SetCookie(w, cookie)
		}
		data.CSRF = cookie.Value
		s.renderLogin(w, data, http.StatusOK)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, data, http.StatusBadRequest)
		return
	}
	if retryAfter, ok := s.allowLoginAttempt(r, r.FormValue("username")); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeProblem(w, r, http.StatusTooManyRequests, "Too many login attempts", "Retry later before attempting to sign in again.")
		return
	}
	csrfCookie, err := r.Cookie("hfproxy_login_csrf")
	if err != nil || !constantEqual(csrfCookie.Value, r.FormValue("csrf_token")) {
		data.Error = "La requête a expiré. Rechargez la page puis réessayez."
		s.renderLogin(w, data, http.StatusForbidden)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	var userID string
	var authErr error
	provider := "local"
	if s.currentConfig().AuthMode == "local" {
		s.loginMu.Lock()
		if s.loginHashSlots == nil {
			s.loginHashSlots = make(chan struct{}, 4)
		}
		slots := s.loginHashSlots
		s.loginMu.Unlock()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}
		var storedHash, status string
		authErr = s.db.SQL.QueryRowContext(r.Context(), `SELECT id,password_hash,status FROM users WHERE username=$1`, username).Scan(&userID, &storedHash, &status)
		if !verifyPassword(localLoginHash(storedHash, status, authErr), password) {
			authErr = sql.ErrNoRows
		}
	} else {
		authErr = sql.ErrNoRows
	}
	if authErr != nil && s.currentConfig().LDAPURL != "" {
		userID, err = s.authenticateLDAP(r.Context(), username, password)
		provider = "ldap"
	} else {
		err = authErr
	}
	if err != nil || userID == "" {
		s.audit(r.Context(), "", "auth.login_failed", "user", username, map[string]any{"provider": provider})
		data.Error = "Identifiant ou mot de passe incorrect."
		s.renderLogin(w, data, http.StatusUnauthorized)
		return
	}
	if _, err := s.createSession(r.Context(), userID, w, r); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Session error", "Unable to create a session.")
		return
	}
	s.audit(r.Context(), userID, "auth.login", "user", userID, map[string]any{"provider": provider})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func localLoginHash(storedHash, status string, lookupErr error) string {
	if lookupErr != nil || status != "active" {
		return dummyPasswordHash
	}
	return storedHash
}

func (s *Server) renderLogin(w http.ResponseWriter, data loginData, status int) {
	t, err := template.New("login").Parse(loginPage)
	if err != nil {
		http.Error(w, "login page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = t.Execute(w, data)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.checkCSRF(r) {
		writeProblem(w, r, http.StatusForbidden, "CSRF validation failed", "Reload the page and retry the action.")
		return
	}
	cookie, err := r.Cookie("hfproxy_session")
	var actorID string
	if err == nil && cookie.Value != "" {
		digest := HashToken(cookie.Value, s.currentConfig().SessionSecret)
		_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT user_id FROM sessions WHERE token_hash=$1`, digest).Scan(&actorID)
		_, _ = s.db.SQL.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, digest)
	}
	s.audit(r.Context(), actorID, "auth.logout", "user", actorID, nil)
	clearCookie(w, "hfproxy_session")
	clearCookie(w, "hfproxy_csrf")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type oidcState struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Expires  time.Time `json:"expires"`
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.currentOIDCConfig() == nil {
		http.NotFound(w, r)
		return
	}
	state := oidcState{State: randomSecret(24), Nonce: randomSecret(24), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(10 * time.Minute)}
	encoded, _ := json.Marshal(state)
	http.SetCookie(w, &http.Cookie{Name: "hfproxy_oidc", Value: base64.RawURLEncoding.EncodeToString(encoded), Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.currentConfig().OIDCRedirectURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	url := s.currentOIDCConfig().AuthCodeURL(state.State, oauth2.S256ChallengeOption(state.Verifier), oauth2.SetAuthURLParam("nonce", state.Nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.currentOIDCConfig() == nil || s.currentOIDCVerifier() == nil {
		http.NotFound(w, r)
		return
	}
	defer http.SetCookie(w, &http.Cookie{Name: "hfproxy_oidc", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.currentConfig().OIDCRedirectURL, "https://"), SameSite: http.SameSiteLaxMode})
	cookie, err := r.Cookie("hfproxy_oidc")
	if err != nil {
		http.Redirect(w, r, "/login?error=invalid_state", http.StatusSeeOther)
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	var state oidcState
	if err != nil || json.Unmarshal(data, &state) != nil || time.Now().After(state.Expires) || state.State == "" || state.State != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/login?error=invalid_state", http.StatusSeeOther)
		return
	}
	token, err := s.currentOIDCConfig().Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil {
		s.logger.Warn("OIDC code exchange failed")
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	idToken, err := s.currentOIDCVerifier().Verify(r.Context(), rawIDToken)
	if err != nil {
		s.logger.Warn("OIDC ID token validation failed")
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	var claims struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Nonce         string `json:"nonce"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" || !constantEqual(claims.Nonce, state.Nonce) {
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	userID, err := s.oidcUser(r.Context(), idToken.Issuer, claims)
	if err != nil {
		s.audit(r.Context(), "", "auth.login_failed", "identity", idToken.Subject, map[string]any{"provider": "oidc"})
		http.Redirect(w, r, "/login?error=provisioning_disabled", http.StatusSeeOther)
		return
	}
	if _, err := s.createSession(r.Context(), userID, w, r); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Session error", "Unable to create a session.")
		return
	}
	s.audit(r.Context(), userID, "auth.login", "user", userID, map[string]any{"provider": "oidc"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) oidcUser(ctx context.Context, issuer string, claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
}) (string, error) {
	var userID, status string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT u.id,u.status FROM external_identities x JOIN users u ON u.id=x.user_id WHERE x.issuer=$1 AND x.subject=$2`, issuer, claims.Subject).Scan(&userID, &status)
	if err == nil {
		if status != "active" {
			return "", fmt.Errorf("account disabled")
		}
		return userID, nil
	}
	if err != sql.ErrNoRows || !s.currentConfig().OIDCAutoProvision || !claims.EmailVerified {
		return "", fmt.Errorf("identity is not provisioned")
	}
	email, err := mail.ParseAddress(claims.Email)
	if err != nil || !claims.EmailVerified {
		return "", fmt.Errorf("verified email is required for provisioning")
	}
	username := usernameFromEmail(email.Address)
	userID = db.NewID()
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,email,display_name,role) VALUES ($1,$2,$3,$4,'reader')`, userID, username, email.Address, claims.Name)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES ($1,$2,'oidc',$3,$4)`, db.NewID(), userID, issuer, claims.Subject)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.audit(ctx, userID, "user.provision", "user", userID, map[string]any{"provider": "oidc"})
	return userID, nil
}

func (s *Server) authenticateLDAP(ctx context.Context, username, password string) (string, error) {
	if s.currentConfig().LDAPURL == "" || username == "" || password == "" {
		return "", fmt.Errorf("LDAP authentication is not available")
	}
	conn, err := ldap.DialURL(s.currentConfig().LDAPURL)
	if err != nil {
		return "", fmt.Errorf("LDAP connection failed")
	}
	defer conn.Close()
	conn.SetTimeout(5 * time.Second)
	parsedURL, err := url.Parse(s.currentConfig().LDAPURL)
	if err != nil {
		return "", fmt.Errorf("LDAP configuration is invalid")
	}
	if parsedURL.Scheme == "ldap" && s.currentConfig().LDAPStartTLS {
		if err := conn.StartTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsedURL.Hostname()}); err != nil {
			return "", fmt.Errorf("LDAP StartTLS failed")
		}
	}
	if s.currentConfig().LDAPBindDN != "" {
		if err := conn.Bind(s.currentConfig().LDAPBindDN, s.currentConfig().LDAPBindPassword); err != nil {
			return "", fmt.Errorf("LDAP service bind failed")
		}
	}
	filter := strings.ReplaceAll(s.currentConfig().LDAPUserFilter, "{username}", ldap.EscapeFilter(username))
	request := ldap.NewSearchRequest(s.currentConfig().LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 5, false, filter, []string{"mail", "cn", "displayName"}, nil)
	result, err := conn.Search(request)
	if err != nil || len(result.Entries) != 1 {
		return "", fmt.Errorf("LDAP user lookup failed")
	}
	entry := result.Entries[0]
	if err := conn.Bind(entry.DN, password); err != nil {
		return "", fmt.Errorf("LDAP credentials rejected")
	}
	issuer := s.currentConfig().LDAPURL
	var userID, status string
	err = s.db.SQL.QueryRowContext(ctx, "SELECT u.id,u.status FROM external_identities x JOIN users u ON u.id=x.user_id WHERE x.issuer=$1 AND x.subject=$2", issuer, entry.DN).Scan(&userID, &status)
	if err == nil {
		if status != "active" {
			return "", fmt.Errorf("account disabled")
		}
		return userID, nil
	}
	if err != sql.ErrNoRows || !s.currentConfig().LDAPAutoProvision {
		return "", fmt.Errorf("LDAP identity is not provisioned")
	}
	localUsername := usernameFromLogin(username)
	email := entry.GetAttributeValue("mail")
	displayName := entry.GetAttributeValue("displayName")
	if displayName == "" {
		displayName = entry.GetAttributeValue("cn")
	}
	if len(email) > 254 {
		email = ""
	}
	if len(displayName) > 200 {
		displayName = username
	}
	userID = db.NewID()
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,username,email,display_name,role) VALUES($1,$2,$3,$4,'reader')", userID, localUsername, email, displayName); err != nil {
		return "", fmt.Errorf("provision LDAP user failed")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,'ldap',$3,$4)", db.NewID(), userID, issuer, entry.DN); err != nil {
		return "", fmt.Errorf("record LDAP identity failed")
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.audit(ctx, userID, "user.provision", "user", userID, map[string]any{"provider": "ldap"})
	return userID, nil
}

func usernameFromLogin(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		}
	}
	base := strings.Trim(b.String(), "._-")
	if len(base) > 40 {
		base = base[:40]
	}
	if base == "" {
		base = "user"
	}
	return base + "-" + randomSecret(4)
}

func (s *Server) renderDashboardLogout(w http.ResponseWriter, csrf string) template.HTML {
	return template.HTML(`<form method="post" action="/logout"><input type="hidden" name="csrf_token" value="` + template.HTMLEscapeString(csrf) + `"><button class="quiet-button">Se déconnecter</button></form>`)
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func HashToken(token, pepper string) []byte {
	h := hmac.New(sha256.New, []byte(pepper))
	_, _ = h.Write([]byte(token))
	return h.Sum(nil)
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", fmt.Errorf("password must contain 12 to 1024 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const memory = 64 * 1024
	const iterations = 3
	const parallelism = 1
	hash := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

var dummyPasswordHash = func() string {
	hash, err := HashPassword("hfproxy-dummy-password")
	if err != nil {
		panic(err)
	}
	return hash
}()

func (s *Server) allowLoginAttempt(r *http.Request, username string) (int, bool) {
	host := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsed
	}
	keys := []string{"ip:" + host, "user:" + strings.ToLower(strings.TrimSpace(username))}
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginBuckets == nil {
		s.loginBuckets = make(map[string]loginBucket)
	}
	for _, key := range keys {
		bucket := s.loginBuckets[key]
		if bucket.window.IsZero() || now.Sub(bucket.window) >= time.Minute {
			bucket = loginBucket{window: now}
		}
		if bucket.count >= 10 {
			remaining := int(time.Until(bucket.window.Add(time.Minute)).Seconds()) + 1
			return remaining, false
		}
		bucket.count++
		s.loginBuckets[key] = bucket
	}
	return 0, true
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil || memory < 8*1024 || memory > 256*1024 || iterations == 0 || iterations > 10 || parallelism == 0 || parallelism > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomID() string { return randomSecret(12) }

func constantEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func constantEqualBytes(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

func usernameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.ToLower(local)
	var b strings.Builder
	for _, r := range local {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	value := strings.Trim(b.String(), "._-")
	if len(value) > 40 {
		value = value[:40]
	}
	if value == "" {
		value = "user"
	}
	return value + "-" + randomSecret(4)
}
