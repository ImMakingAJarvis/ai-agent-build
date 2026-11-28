// Package api is loopd's HTTP layer (M4.1). This card (ELR-93, M4.1a)
// adds the token loader and the bearer-token guard; the routes land in
// the later M4.1 cards.
package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// LoadToken reads the bearer token from the file at path. The file must
// exist, must not be empty (after trimming one trailing newline, which a
// token written with `echo` gains), and must not be readable by anyone
// other than its owner: a mode with any group/other bit set (mode&0o077)
// is an error, because a token in a group-readable file is a leak.
func LoadToken(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token %s: %w", path, err)
	}
	if st.Mode()&0o077 != 0 {
		return "", fmt.Errorf("token %s: mode %o is group/other accessible", path, st.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token %s: %w", path, err)
	}
	tok := strings.TrimSuffix(string(b), "\n")
	if tok == "" {
		return "", fmt.Errorf("token %s: file is empty", path)
	}
	return tok, nil
}

// Guard wraps next with the bearer-token check. /healthz and /metrics
// pass without a token (probes and Prometheus must work unauthenticated);
// every other path and method — GET included, nothing is served without
// the token — requires the header `Authorization: Bearer <token>` and
// answers 401 {"error":"unauthorized"} otherwise. The comparison is
// constant-time.
func Guard(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(token)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}
