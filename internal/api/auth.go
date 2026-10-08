package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"lakesync/internal/core"
	"lakesync/internal/store"
)

const (
	sessionCookie = "lakesync_session"
	sessionTTL    = 7 * 24 * time.Hour
	maxLoginTries = 5
	loginWindow   = 15 * time.Minute
	loginBlock    = 15 * time.Minute
)

type ctxKey int

const (
	ctxSession ctxKey = iota
	ctxUser
)

func sessionFrom(ctx context.Context) *store.Session {
	s, _ := ctx.Value(ctxSession).(*store.Session)
	return s
}

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

// HashPassword hashes with argon2id (64 MB, 3 iterations, 2 lanes).
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword checks a password against an argon2id hash.
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// EnsureAdmin creates the admin on first boot from the environment. It fails
// when no user exists and the environment is incomplete.
func EnsureAdmin(ctx context.Context, st *store.Store, email, password string) error {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if email == "" || password == "" {
		return errors.New("no user exists yet: set ADMIN_EMAIL and ADMIN_PASSWORD to create the admin account")
	}
	if len(password) < 8 {
		return errors.New("ADMIN_PASSWORD must be at least 8 characters")
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, strings.ToLower(strings.TrimSpace(email)), h); err != nil {
		return err
	}
	return nil
}

// auth loads the session from the cookie and rejects anonymous requests.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			s.writeError(w, http.StatusUnauthorized, "unauthorized", "Please log in.", "")
			return
		}
		h := store.HashToken(c.Value)
		sess, err := s.st.GetSession(r.Context(), h)
		if err != nil {
			s.clearSessionCookie(w)
			s.writeError(w, http.StatusUnauthorized, "unauthorized", "Your session has expired. Please log in again.", "")
			return
		}
		user, err := s.st.GetUser(r.Context(), sess.UserID)
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "unauthorized", "Please log in.", "")
			return
		}
		_ = s.st.TouchSession(r.Context(), h, sessionTTL)
		ctx := context.WithValue(r.Context(), ctxSession, sess)
		ctx = context.WithValue(ctx, ctxUser, user)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: !s.cfg.DevMode,
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(sessionTTL),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: !s.cfg.DevMode, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := s.decode(r, &body); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || body.Password == "" {
		s.badRequest(w, "Email and password are required.", "")
		return
	}
	ip := clientIP(r)
	for _, key := range []string{"email:" + email, "ip:" + ip} {
		ok, wait, err := s.st.LoginAllowed(r.Context(), key)
		if err != nil {
			s.internal(w, err)
			return
		}
		if !ok {
			s.writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed attempts.", fmt.Sprintf("Try again in %d minutes.", int(wait.Minutes())+1))
			return
		}
	}
	user, err := s.st.GetUserByEmail(r.Context(), email)
	fail := func() {
		_ = s.st.LoginFailed(r.Context(), "email:"+email, maxLoginTries, loginWindow, loginBlock)
		_ = s.st.LoginFailed(r.Context(), "ip:"+ip, maxLoginTries*4, loginWindow, loginBlock)
		s.writeError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong email or password.", "")
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// burn time like a real check to avoid user enumeration by timing
			VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", body.Password)
			fail()
			return
		}
		s.internal(w, err)
		return
	}
	if !VerifyPassword(user.PasswordHash, body.Password) {
		fail()
		return
	}
	_ = s.st.LoginSucceeded(r.Context(), "email:"+email)
	token := store.NewToken()
	sess, err := s.st.CreateSession(r.Context(), store.HashToken(token), user.ID, store.NewToken(), sessionTTL)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.setSessionCookie(w, token)
	s.writeJSON(w, 200, s.meResponse(r.Context(), user, sess))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r.Context()); sess != nil {
		_ = s.st.DeleteSession(r.Context(), sess.ID)
	}
	s.clearSessionCookie(w)
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, 200, s.meResponse(r.Context(), userFrom(r.Context()), sessionFrom(r.Context())))
}

func (s *Server) meResponse(ctx context.Context, u *store.User, sess *store.Session) map[string]any {
	return map[string]any{
		"user":       map[string]any{"id": u.ID, "email": u.Email, "created_at": u.CreatedAt},
		"csrf_token": sess.CSRFToken,
		"app": map[string]any{
			"name":     core.AppName,
			"version":  s.cfg.Version,
			"commit":   s.cfg.Commit,
			"dev_mode": s.cfg.DevMode,
		},
		"egress_ip":               s.egress.get(),
		"allow_private_endpoints": s.cfg.AllowPrivateEndpoints,
		"worker_in_process":       s.worker != nil,
	}
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := s.decode(r, &body); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	u := userFrom(r.Context())
	if !VerifyPassword(u.PasswordHash, body.Current) {
		s.writeError(w, http.StatusForbidden, "invalid_credentials", "The current password is wrong.", "")
		return
	}
	if len(body.New) < 8 {
		s.badRequest(w, "The new password must be at least 8 characters.", "")
		return
	}
	h, err := HashPassword(body.New)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.st.UpdatePassword(r.Context(), u.ID, h, sessionFrom(r.Context()).ID); err != nil {
		s.internal(w, err)
		return
	}
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}
