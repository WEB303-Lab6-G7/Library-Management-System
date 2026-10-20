package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"library/pkg/svc"
)

// A session token is  base64(json) "." base64(HMAC-SHA256).
// The signature means the browser cannot change its own user id or role.
//
// The website keeps the token per browser TAB (sessionStorage) and sends it as
// "Authorization: Bearer <token>", so one tab can be signed in as a librarian and
// another as a member at the same time. The token is also set as a cookie, which
// is used only by clients that send no Authorization header (e.g. the curl tests).

const cookieName = "library_session"

var sessionKey []byte

const sessionTTL = 8 * time.Hour

type session struct {
	UID   int64  `json:"uid"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"` // member | librarian
	Exp   int64  `json:"exp"`
}

func sign(payload []byte) []byte {
	m := hmac.New(sha256.New, sessionKey)
	m.Write(payload)
	return m.Sum(nil)
}

func setSession(w http.ResponseWriter, s *session) {
	s.Exp = time.Now().Add(sessionTTL).Unix()
	payload, _ := json.Marshal(s)
	enc := base64.RawURLEncoding
	token := enc.EncodeToString(payload) + "." + enc.EncodeToString(sign(payload))
	w.Header().Set("X-Session-Token", token) // the website stores this per tab
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

func currentSession(r *http.Request) *session {
	var token string
	if h, present := r.Header["Authorization"]; present {
		// The website always sends this header (empty when the tab is signed out),
		// so a browser tab never falls back to another tab's cookie.
		token, _ = strings.CutPrefix(strings.Join(h, ""), "Bearer ")
	} else if c, err := r.Cookie(cookieName); err == nil {
		token = c.Value
	}
	if token == "" {
		return nil
	}
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return nil
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(payloadB64)
	sig, err2 := enc.DecodeString(sigB64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, sign(payload)) {
		return nil
	}
	var s session
	if json.Unmarshal(payload, &s) != nil || s.UID == 0 || time.Now().Unix() > s.Exp {
		return nil
	}
	return &s
}

type authedHandler func(http.ResponseWriter, *http.Request, *session)

// requireAuth rejects requests without a valid session.
func requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := currentSession(r)
		if s == nil {
			svc.Error(w, http.StatusUnauthorized, "please sign in to continue")
			return
		}
		h(w, r, s)
	}
}

// requireRole additionally checks the user's role.
func requireRole(role string, h authedHandler) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request, s *session) {
		if s.Role != role {
			svc.Error(w, http.StatusForbidden, "only a %s can do that", role)
			return
		}
		h(w, r, s)
	})
}