package gateway

// The "unauthorised" state: the platform answers 401 or 403 to this
// gateway's key (revoked, replaced, or a plan key retired by an enrolled
// gateway). The gateway keeps reading and spooling, holds the spool so no
// batch is ever dropped because of it, says so in the log once an hour, and
// leaves the state by itself on the first accepted call.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrUnauthorized: the platform refused the gateway's key (HTTP 401 or 403).
var ErrUnauthorized = errors.New("the platform refuses this gateway's key")

// AuthState tracks whether the platform currently refuses the key.
type AuthState struct {
	// Logf writes the state's log lines (log.Printf in the program).
	Logf func(format string, args ...any)

	mu      sync.Mutex
	since   time.Time // zero: authorised
	lastLog time.Time
}

// Observe records the outcome of an authenticated call: an ErrUnauthorized
// enters the state, nil (an accepted call) leaves it. Other errors (the
// network) say nothing about the key and are ignored.
func (a *AuthState) Observe(err error, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case errors.Is(err, ErrUnauthorized):
		if a.since.IsZero() {
			a.since, a.lastLog = now, now
			a.logf("UNAUTHORISED: %v. Readings keep spooling and nothing is dropped; enroll again with a new code: cannabits-gateway enroll <code>", err)
		}
	case err == nil:
		if !a.since.IsZero() {
			a.logf("authorised again after %s; the spool drains", now.Sub(a.since).Round(time.Second))
			a.since = time.Time{}
		}
	}
}

// Unauthorised reports whether the platform currently refuses the key.
func (a *AuthState) Unauthorised() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.since.IsZero()
}

// Tick repeats the state's log line once an hour while it lasts.
func (a *AuthState) Tick(now time.Time, pending int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.since.IsZero() || now.Sub(a.lastLog) < time.Hour {
		return
	}
	a.lastLog = now
	a.logf("UNAUTHORISED since %s: %d batch(es) kept in the spool, none dropped; enroll again with a new code: cannabits-gateway enroll <code>", a.since.UTC().Format(time.RFC3339), pending)
}

func (a *AuthState) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// ReadKeyFile returns the key stored in path, "" when there is none.
func ReadKeyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), err
}

// WriteKeyFile stores a key durably: a 0600 temporary file in the same
// directory, synced, then renamed over the old one. A crash leaves either
// the old key or the new one, never half of one.
func WriteKeyFile(path, key string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gateway.key.tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
