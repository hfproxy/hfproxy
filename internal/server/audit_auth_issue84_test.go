package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func TestIssue84CSRFlessRequestsDoNotLockOutUsername(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	password := "correct-horse-battery-staple"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,password_hash,role,status) VALUES($1,'issue84-admin',$2,'admin','active')`, db.NewID(), hash); err != nil {
		t.Fatal(err)
	}
	postLogin := func(pass string, csrf bool) *httptest.ResponseRecorder {
		form := url.Values{"username": {"issue84-admin"}, "password": {pass}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if csrf {
			form.Set("csrf_token", "issue84-valid-csrf")
			req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: "hfproxy_login_csrf", Value: "issue84-valid-csrf"})
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 10; i++ {
		if rec := postLogin("invalid-password", false); rec.Code != http.StatusForbidden {
			t.Fatalf("CSRF-less request %d returned %d, want %d", i, rec.Code, http.StatusForbidden)
		}
	}
	if rec := postLogin(password, true); rec.Code != http.StatusSeeOther {
		t.Fatalf("valid login after CSRF-less requests returned %d, want %d", rec.Code, http.StatusSeeOther)
	}
}
