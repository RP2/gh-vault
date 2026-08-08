package web

import (
	"net"
	"sync"
	"time"
)

const (
	rateLimitAttempts    = 5
	rateLimitWindow      = time.Minute
	rateLimitLockout     = 15 * time.Minute
	rateLimitCleanupAge  = 30 * time.Minute
	rateLimitCleanupFreq = 10 * time.Minute
)

type ipAttempt struct {
	attempts    []time.Time
	lockedUntil time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*ipAttempt
}

func newRateLimiter() (*rateLimiter, chan struct{}) {
	stop := make(chan struct{})
	return &rateLimiter{
		attempts: make(map[string]*ipAttempt),
	}, stop
}

func (rl *rateLimiter) allow(ip string) bool {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	attempt, ok := rl.attempts[ip]
	if !ok {
		attempt = &ipAttempt{}
		rl.attempts[ip] = attempt
	}

	if now.Before(attempt.lockedUntil) {
		return false
	}

	// Drop attempts that fell outside the sliding window.
	cutoff := now.Add(-rateLimitWindow)
	idx := 0
	for idx < len(attempt.attempts) && attempt.attempts[idx].Before(cutoff) {
		idx++
	}
	attempt.attempts = attempt.attempts[idx:]

	if len(attempt.attempts) >= rateLimitAttempts {
		attempt.lockedUntil = now.Add(rateLimitLockout)
		return false
	}

	attempt.attempts = append(attempt.attempts, now)
	return true
}

func (rl *rateLimiter) reset(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	attempt, ok := rl.attempts[ip]
	if !ok {
		return
	}
	attempt.attempts = attempt.attempts[:0]
	attempt.lockedUntil = time.Time{}
}

func (rl *rateLimiter) cleanup() {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	for ip, attempt := range rl.attempts {
		// Prune old timestamps
		cutoff := now.Add(-rateLimitWindow)
		idx := 0
		for idx < len(attempt.attempts) && attempt.attempts[idx].Before(cutoff) {
			idx++
		}
		attempt.attempts = attempt.attempts[idx:]

		if now.After(attempt.lockedUntil) && len(attempt.attempts) == 0 {
			delete(rl.attempts, ip)
		}
	}
}

func (rl *rateLimiter) cleanupLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(rateLimitCleanupFreq)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-stop:
			return
		}
	}
}

func clientIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host
	}
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String()
	}
	return "unknown"
}
