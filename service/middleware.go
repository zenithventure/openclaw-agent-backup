package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const agentContextKey contextKey = "agent"

// AgentFromContext extracts the authenticated agent from the request context.
func AgentFromContext(ctx context.Context) *Agent {
	a, _ := ctx.Value(agentContextKey).(*Agent)
	return a
}

// Auth validates the bearer token and injects the agent into the context.
func Auth(store DataStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			http.Error(w, `{"error":"invalid Authorization format, expected Bearer token"}`, http.StatusUnauthorized)
			return
		}

		agent, err := store.LookupAgentByToken(token)
		if err != nil {
			log.Printf("ERROR: token lookup failed: %v", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if agent == nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), agentContextKey, agent)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RateLimit applies per-IP rate limiting using the store.
func RateLimit(maxPerMinute int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		key := "ratelimit:" + ip

		// We need a store reference — use a simple in-memory approach instead
		// since the store is not passed here. For production, use the store.
		// For now, we'll use a package-level rate limiter.
		allowed, err := globalRateLimiter.Allow(key, maxPerMinute)
		if err != nil {
			log.Printf("ERROR: rate limit check failed: %v", err)
			// Fail open
			next.ServeHTTP(w, r)
			return
		}

		if !allowed {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":"rate limit exceeded, try again later"}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// LogRequests logs each request with method, path, status, and duration.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	// Check X-Forwarded-For / X-Real-IP for reverse proxies
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// ---------------------------------------------------------------------------
// In-memory rate limiter (avoids passing store into middleware)
// ---------------------------------------------------------------------------

type inMemoryRateLimiter struct {
	windows map[string]*rateLimitWindow
}

type rateLimitWindow struct {
	count    int
	windowStart time.Time
}

var globalRateLimiter = &inMemoryRateLimiter{
	windows: make(map[string]*rateLimitWindow),
}

func (rl *inMemoryRateLimiter) Allow(key string, maxPerMinute int) (bool, error) {
	now := time.Now()

	w, exists := rl.windows[key]
	if !exists || now.Sub(w.windowStart) > time.Minute {
		rl.windows[key] = &rateLimitWindow{count: 1, windowStart: now}
		return true, nil
	}

	if w.count >= maxPerMinute {
		return false, nil
	}

	w.count++
	return true, nil
}
