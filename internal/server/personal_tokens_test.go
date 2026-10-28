package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPersonalTokensRequireSession(t *testing.T) {
	s := newAuditAuthServerNoDB(auditAuthConfig())
	for _, path := range []string{"/api/v1/me/tokens", "/api/v1/me/tokens/other"} {
		rec := auditAuthDo(s, http.MethodGet, path, "", nil, nil)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unauthenticated %s: status=%d, headers=%v", path, rec.Code, rec.Header())
		}
	}
}

func TestPersonalTokenLifecycleAndIsolation(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	userID, existingToken := auditAPICreateUser(t, store, "reader", "reader")
	otherID, _ := auditAPICreateUser(t, store, "other", "reader")
	cookies := auditAuthSession(t, s, userID)
	otherCookies := auditAuthSession(t, s, otherID)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		return auditAuthDo(s, method, path, body, cookies, nil)
	}
	for _, body := range []string{
		`{"name":" ","expires_in_days":30}`,
		`{"name":"test","expires_in_days":0}`,
		`{"name":"test","expires_in_days":366}`,
		`{"name":"test","expires_in_days":30,"scopes":["admin"]}`,
		`{"name":"test","expires_in_days":30,"user_id":"other"}`,
	} {
		if rec := do("POST", "/api/v1/me/tokens", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid input status=%d: %s", rec.Code, rec.Body)
		}
	}
	// A session cookie alone must not bypass CSRF validation.
	req := httptest.NewRequest("POST", "/api/v1/me/tokens", strings.NewReader(`{"name":"test","expires_in_days":30}`))
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", rec.Code)
	}
	if rec := auditAPIDo(s, "GET", "/api/v1/me/tokens", existingToken, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("bearer token could manage credentials: %d", rec.Code)
	}
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET scopes=ARRAY['write'] WHERE token_hash=$1`, HashToken(existingToken, auditAPISessionSecret)); err != nil {
		t.Fatal(err)
	}
	if rec := auditAPIDo(s, "POST", "/api/v1/me/tokens", existingToken, `{"name":"delegated","expires_in_days":30}`); rec.Code != http.StatusForbidden {
		t.Fatalf("write token could issue another credential: %d", rec.Code)
	}
	rec = do("POST", "/api/v1/me/tokens", `{"name":" Workstation ","expires_in_days":30}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d: %s", rec.Code, rec.Body)
	}
	var created struct {
		personalToken
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "hfpx_") || len(created.Token) < 40 || created.Name != "Workstation" || created.ExpiresAt == nil || len(created.Scopes) != 1 || created.Scopes[0] != "read" {
		t.Fatal("created token metadata is invalid")
	}
	var stored []byte
	if err := store.SQL.QueryRow(`SELECT token_hash FROM access_tokens WHERE id=$1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, HashToken(created.Token, auditAPISessionSecret)) {
		t.Fatal("token is not stored as the expected keyed hash")
	}
	list := do("GET", "/api/v1/me/tokens", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), created.Token) || strings.Contains(list.Body.String(), "token_hash") || !strings.Contains(list.Body.String(), created.ID) {
		t.Fatalf("list status=%d; secret must not be listed", list.Code)
	}
	otherList := auditAuthDo(s, "GET", "/api/v1/me/tokens", "", otherCookies, nil)
	if otherList.Code != http.StatusOK || strings.Contains(otherList.Body.String(), created.ID) {
		t.Fatal("token leaked to another user")
	}
	if rec := auditAuthDo(s, "DELETE", "/api/v1/me/tokens/"+created.ID, "", otherCookies, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user could revoke token: %d", rec.Code)
	}
	if rec := auditAPIDo(s, "GET", "/api/v1/me", created.Token, ""); rec.Code != http.StatusOK {
		t.Fatalf("new token cannot authenticate: %d", rec.Code)
	}
	if rec := auditAPIDo(s, "POST", "/api/v1/users", created.Token, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token could mutate API: %d", rec.Code)
	}
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET expires_at=now()-interval '1 second' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if rec := auditAPIDo(s, "GET", "/api/v1/me", created.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token still authenticates: %d", rec.Code)
	}
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET expires_at=now()+interval '30 days' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if rec := do("DELETE", "/api/v1/me/tokens/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d: %s", rec.Code, rec.Body)
	}
	if rec := auditAPIDo(s, "GET", "/api/v1/me", created.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still authenticates: %d", rec.Code)
	}
	if list := do("GET", "/api/v1/me/tokens", ""); strings.Contains(list.Body.String(), created.ID) {
		t.Fatal("revoked token is still listed")
	}
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name) SELECT 'limit-'||n,$1,decode(md5(n::text),'hex'),'prefix','limit' FROM generate_series(1,99) n`, userID); err != nil {
		t.Fatal(err)
	}
	if rec := do("POST", "/api/v1/me/tokens", `{"name":"excess","expires_in_days":30}`); rec.Code != http.StatusConflict {
		t.Fatalf("token count limit not enforced: %d", rec.Code)
	}
}
