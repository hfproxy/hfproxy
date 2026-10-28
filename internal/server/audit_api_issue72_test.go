package server

import (
	"net/http"
	"sync"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func TestIssue72ConcurrentAdminDemotionsKeepAnActiveAdmin(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	adminA := db.NewID()
	adminB := db.NewID()
	for id, username := range map[string]string{adminA: "issue72-a", adminB: "issue72-b"} {
		if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'admin')`, id, username); err != nil {
			t.Fatal(err)
		}
	}
	principalA := &Principal{ID: adminA, Username: "issue72-a", Role: "admin", Authenticated: true}
	principalB := &Principal{ID: adminB, Username: "issue72-b", Role: "admin", Authenticated: true}

	for round := 0; round < 10; round++ {
		if _, err := store.SQL.Exec(`UPDATE users SET role='admin',status='active' WHERE id IN ($1,$2)`, adminA, adminB); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, request := range []struct {
			principal *Principal
			target    string
		}{{principalA, adminB}, {principalB, adminA}} {
			wg.Add(1)
			go func(i int, principal *Principal, target string) {
				defer wg.Done()
				<-start
				rec := auditAPIDoAs(s, principal, http.MethodPatch, "/api/v1/users/"+target, `{"role":"reader"}`)
				codes[i] = rec.Code
			}(i, request.principal, request.target)
		}
		close(start)
		wg.Wait()

		var admins int
		if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins == 0 {
			t.Fatalf("round %d: concurrent demotions returned %v; no active administrator remains", round, codes)
		}
		if codes[0]+codes[1] != http.StatusOK+http.StatusConflict {
			t.Fatalf("round %d: expected one success and one conflict, got %v", round, codes)
		}
	}
}
