package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// CountUsers returns the number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

// CreateUser inserts a user with an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (*User, error) {
	u := &User{}
	err := s.Pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, email, password_hash, created_at",
		email, passwordHash).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	return u, err
}

// GetUserByEmail finds a user; ErrNotFound when missing.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := s.Pool.QueryRow(ctx, "SELECT id, email, password_hash, created_at FROM users WHERE lower(email) = lower($1)", email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetUser finds a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := s.Pool.QueryRow(ctx, "SELECT id, email, password_hash, created_at FROM users WHERE id = $1", id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return u, err
}

// UpdatePassword replaces the hash and invalidates every other session.
func (s *Store) UpdatePassword(ctx context.Context, userID, passwordHash, keepSessionID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1", userID, passwordHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND id <> $2", userID, keepSessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// NewToken returns a random 32-byte token as hex.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// HashToken derives the stored session id from the cookie token.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateSession stores a session for the hashed token.
func (s *Store) CreateSession(ctx context.Context, tokenHash, userID, csrf string, ttl time.Duration) (*Session, error) {
	sess := &Session{ID: tokenHash, UserID: userID, CSRFToken: csrf, ExpiresAt: time.Now().Add(ttl)}
	err := s.Pool.QueryRow(ctx,
		"INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4) RETURNING created_at",
		sess.ID, sess.UserID, sess.CSRFToken, sess.ExpiresAt).Scan(&sess.CreatedAt)
	return sess, err
}

// GetSession loads a live session; ErrNotFound when missing or expired.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	sess := &Session{}
	err := s.Pool.QueryRow(ctx,
		"SELECT id, user_id, csrf_token, expires_at, created_at FROM sessions WHERE id = $1 AND expires_at > now()", tokenHash).
		Scan(&sess.ID, &sess.UserID, &sess.CSRFToken, &sess.ExpiresAt, &sess.CreatedAt)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return sess, err
}

// TouchSession extends a session that is older than a minute.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, ttl time.Duration) error {
	_, err := s.Pool.Exec(ctx, "UPDATE sessions SET last_seen = now(), expires_at = now() + $2 WHERE id = $1 AND last_seen < now() - interval '1 minute'", tokenHash, ttl)
	return err
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1", tokenHash)
	return err
}

// DeleteExpiredSessions removes stale rows.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < now()")
	return err
}

// LoginAllowed reports whether a login attempt for key (email or IP) may proceed.
func (s *Store) LoginAllowed(ctx context.Context, key string) (bool, time.Duration, error) {
	var blockedUntil *time.Time
	err := s.Pool.QueryRow(ctx, "SELECT blocked_until FROM login_attempts WHERE key = $1", key).Scan(&blockedUntil)
	if isNoRows(err) {
		return true, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if blockedUntil != nil && blockedUntil.After(time.Now()) {
		return false, time.Until(*blockedUntil), nil
	}
	return true, 0, nil
}

// LoginFailed records a failure; after maxAttempts within the window the key is
// blocked for blockFor.
func (s *Store) LoginFailed(ctx context.Context, key string, maxAttempts int, window, blockFor time.Duration) error {
	_, err := s.Pool.Exec(ctx, `
INSERT INTO login_attempts (key, attempts, updated_at) VALUES ($1, 1, now())
ON CONFLICT (key) DO UPDATE SET
  attempts = CASE WHEN login_attempts.updated_at < now() - $3::interval THEN 1 ELSE login_attempts.attempts + 1 END,
  blocked_until = CASE WHEN (CASE WHEN login_attempts.updated_at < now() - $3::interval THEN 1 ELSE login_attempts.attempts + 1 END) >= $2 THEN now() + $4::interval ELSE NULL END,
  updated_at = now()`, key, maxAttempts, window, blockFor)
	return err
}

// LoginSucceeded clears the counter.
func (s *Store) LoginSucceeded(ctx context.Context, key string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM login_attempts WHERE key = $1", key)
	return err
}
