package server

import (
	"database/sql"
	"testing"
)

func TestIssue77DisabledAndMissingUsersUseDummyHash(t *testing.T) {
	if got := localLoginHash("stored-argon2-hash", "disabled", nil); got != dummyPasswordHash {
		t.Fatal("disabled account did not select the dummy password hash")
	}
	if got := localLoginHash("", "", sql.ErrNoRows); got != dummyPasswordHash {
		t.Fatal("missing account did not select the dummy password hash")
	}
	if got := localLoginHash("stored-argon2-hash", "active", nil); got != "stored-argon2-hash" {
		t.Fatal("active account did not select its stored hash")
	}
}
