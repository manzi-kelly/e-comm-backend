package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type attemptState struct {
	failures  int
	windowEnd time.Time
	blockedTo time.Time
}

// LoginLimiter provides in-memory throttling for administrator login attempts.
// It is process-local and resets on restart; multi-instance deployments require
// a shared limiter, which is out of scope for this phase.
type LoginLimiter struct {
	mu          sync.Mutex
	entries     map[string]*attemptState
	maxFailures int
	window      time.Duration
	now         func() time.Time
}

func NewLoginLimiter(maxFailures int, window time.Duration) *LoginLimiter {
	if maxFailures <= 0 {
		maxFailures = 10
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &LoginLimiter{
		entries:     make(map[string]*attemptState),
		maxFailures: maxFailures,
		window:      window,
		now:         time.Now,
	}
}

// ClientIP returns the direct peer IP address. Forwarding headers are ignored to
// prevent attackers from bypassing throttling by spoofing them.
func ClientIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.getLocked(key, now)
	if now.Before(state.blockedTo) {
		// Use the limiter's clock, not wall time: tests inject a deterministic
		// clock and production uses time.Now, so this is correct in both cases.
		return false, state.blockedTo.Sub(now)
	}
	return true, 0
}

func (l *LoginLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.getLocked(key, now)
	state.failures++
	if state.failures >= l.maxFailures {
		state.blockedTo = now.Add(l.window)
		state.failures = 0
		state.windowEnd = now.Add(l.window)
	}
}

func (l *LoginLimiter) RecordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *LoginLimiter) getLocked(key string, now time.Time) *attemptState {
	state, ok := l.entries[key]
	if !ok || (!state.windowEnd.IsZero() && now.After(state.windowEnd) && now.After(state.blockedTo)) {
		state = &attemptState{windowEnd: now.Add(l.window)}
		l.entries[key] = state
	}
	// Opportunistic cleanup of stale entries to bound memory growth.
	if len(l.entries) > 4096 {
		for k, v := range l.entries {
			if now.After(v.windowEnd) && now.After(v.blockedTo) {
				delete(l.entries, k)
			}
		}
	}
	return state
}

// SetNow overrides the limiter clock; it is used by activation tests to
// exercise throttle windows deterministically.
func (l *LoginLimiter) SetNow(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// For tests in the auth package.
func (l *LoginLimiter) setNow(now func() time.Time) { l.SetNow(now) }
