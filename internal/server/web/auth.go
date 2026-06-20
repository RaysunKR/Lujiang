package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lujiang/lujiang/internal/server/auth"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleLogin(a *auth.WebAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if _, err := a.VerifyPassword(req.Username, req.Password); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		a.IssueCookie(w, req.Username)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"username": req.Username})
	}
}

func handleLogout(a *auth.WebAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		a.ClearCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMe(a *auth.WebAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.VerifyRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"username": user})
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// 确保 errors 包不被未使用警告（在后续 P3+ 会用）。
var _ = errors.New
