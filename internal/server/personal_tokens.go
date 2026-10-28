package server

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nicolas/hfproxy/internal/db"
)

type personalToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// Personal credentials are managed using a browser session, never another token.
func (s *Server) apiPersonalTokens(w http.ResponseWriter, r *http.Request, suffix string) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to manage your access tokens.")
		return
	}
	if p.TokenID != "" || r.Header.Get("Authorization") != "" {
		writeProblem(w, r, http.StatusForbidden, "Browser session required", "Sign in to manage your access tokens.")
		return
	}
	if suffix != "" {
		id := strings.TrimPrefix(suffix, "/")
		if id == "" || strings.Contains(id, "/") {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This access token does not exist.")
			return
		}
		if r.Method != http.MethodDelete {
			methodNotAllowedWith(w, "DELETE")
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE access_tokens SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, p.ID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to revoke this token.")
			return
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This access token does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "token.revoked", "access_token", id, nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,name,prefix,scopes,created_at,expires_at,last_used_at FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC,id DESC`, p.ID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load your tokens.")
			return
		}
		defer rows.Close()
		items := []personalToken{}
		for rows.Next() {
			var item personalToken
			if err := rows.Scan(&item.ID, &item.Name, &item.Prefix, &item.Scopes, &item.CreatedAt, &item.ExpiresAt, &item.LastUsedAt); err != nil {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load your tokens.")
				return
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load your tokens.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var input struct {
			Name          string `json:"name"`
			ExpiresInDays int    `json:"expires_in_days"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 || input.ExpiresInDays < 1 || input.ExpiresInDays > 365 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid token", "Provide a name of 1–100 characters and an expiration of 1–365 days.")
			return
		}
		tx, err := s.db.SQL.BeginTx(r.Context(), nil)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			return
		}
		defer tx.Rollback()
		// Serialize issuance per user so concurrent requests cannot bypass the limit.
		var id string
		if err := tx.QueryRowContext(r.Context(), `SELECT id FROM users WHERE id=$1 AND status='active' FOR UPDATE`, p.ID).Scan(&id); err != nil {
			if err == sql.ErrNoRows {
				writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to create a token.")
			} else {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			}
			return
		}
		var count int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL`, p.ID).Scan(&count); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			return
		}
		if count >= 100 {
			writeProblem(w, r, http.StatusConflict, "Token limit reached", "Revoke an existing token before creating another (maximum 100).")
			return
		}
		secret := "hfpx_" + randomSecret(32)
		expires := time.Now().UTC().Add(time.Duration(input.ExpiresInDays) * 24 * time.Hour)
		item := personalToken{ID: db.NewID(), Name: input.Name, Prefix: secret[:16], Scopes: []string{"read"}, ExpiresAt: &expires}
		err = tx.QueryRowContext(r.Context(), `INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, item.ID, p.ID, HashToken(secret, s.currentConfig().SessionSecret), item.Prefix, item.Name, item.Scopes, expires).Scan(&item.CreatedAt)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			return
		}
		if err := tx.Commit(); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			return
		}
		s.audit(r.Context(), p.ID, "token.created", "access_token", item.ID, map[string]any{"name": item.Name, "scopes": item.Scopes})
		writeJSON(w, http.StatusCreated, struct {
			personalToken
			Token string `json:"token"`
		}{item, secret})
	default:
		methodNotAllowedWith(w, "GET, POST")
	}
}
