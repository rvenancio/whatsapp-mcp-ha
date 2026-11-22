package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

// MCPPath is the MCP endpoint path.
const MCPPath = "/mcp"

// Role is what an authenticated token may do.
type Role int

const (
	RoleNone Role = iota
	RoleRead      // auth_token: read tools only
	RoleSend      // send_token: read and send tools
	// RoleSendInactive is a send token while enable_send is off: refused with 403, but not counted as a
	// failed login (callers behind one tunnel share a failure key).
	RoleSendInactive
)

// Authenticator checks bearer tokens in constant time and slows down repeated failures.
// It knows the read-only token and, when sending is active, the send token.
type Authenticator struct {
	want       atomic.Pointer[[32]byte]
	wantSend   atomic.Pointer[[32]byte] // nil when sending is not active
	inactive   atomic.Pointer[[32]byte] // send token while enable_send is off; nil if none
	log        *slog.Logger
	mode       int  // 0 api_key, 1 none. Zero keeps bearer-token behaviour.
	sendOnAuth bool // none: a successful check may call the send tools

	mu         sync.Mutex
	failures   map[string]*failEntry
	now        func() time.Time
	logLimit   *rate.Limiter // failed-auth log lines, all clients together
	suppressed int
}

type failEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

func NewAuthenticator(token string, log *slog.Logger) *Authenticator {
	a := &Authenticator{log: log, failures: map[string]*failEntry{}, now: time.Now,
		logLimit: rate.NewLimiter(rate.Every(10*time.Second), 5)}
	a.SetToken(token)
	return a
}

const (
	authModeAPIKey = iota
	authModeNone
)

// UseNone turns authentication off. sendTools is true when enable_send is on, so every caller
// gets the send tools. Not recommended.
func (a *Authenticator) UseNone(sendTools bool) {
	a.mode = authModeNone
	a.sendOnAuth = sendTools
}

// SetToken replaces the accepted read-only token (token rotation from the add-on page).
func (a *Authenticator) SetToken(token string) {
	h := sha256.Sum256([]byte(token))
	a.want.Store(&h)
}

// SetSendToken sets the send token; an empty token disables it.
func (a *Authenticator) SetSendToken(token string) {
	if token == "" {
		a.wantSend.Store(nil)
		return
	}
	h := sha256.Sum256([]byte(token))
	a.wantSend.Store(&h)
}

// SetInactiveSendToken records a send token that exists while sending is off. Empty clears it.
func (a *Authenticator) SetInactiveSendToken(token string) {
	if token == "" {
		a.inactive.Store(nil)
		return
	}
	h := sha256.Sum256([]byte(token))
	a.inactive.Store(&h)
}

// Role returns what the Authorization header ("Bearer <token>") allows. Both tokens are always
// compared, in constant time, so the timing does not tell which one matched.
func (a *Authenticator) Role(header string) Role {
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		tok = ""
	}
	tok = strings.TrimSpace(tok)
	got := sha256.Sum256([]byte(tok))
	want := a.want.Load()
	read := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	match := func(p *[32]byte) bool {
		if p == nil {
			var zero [32]byte
			subtle.ConstantTimeCompare(got[:], zero[:])
			return false
		}
		return subtle.ConstantTimeCompare(got[:], p[:]) == 1
	}
	send := match(a.wantSend.Load())
	inactive := match(a.inactive.Load())
	switch {
	case tok == "":
		return RoleNone
	case send:
		return RoleSend
	case read:
		return RoleRead
	case inactive:
		return RoleSendInactive
	}
	return RoleNone
}

// RoleFrom is Role for an HTTP request. api_key reads Authorization. none accepts everyone.
func (a *Authenticator) RoleFrom(h http.Header) Role {
	switch a.mode {
	case authModeNone:
		if a.sendOnAuth {
			return RoleSend
		}
		return RoleRead
	default:
		if h == nil {
			return a.Role("")
		}
		return a.Role(h.Get("Authorization"))
	}
}

// Check returns true if the Authorization header carries a valid token.
func (a *Authenticator) Check(header string) bool {
	r := a.Role(header)
	return r == RoleRead || r == RoleSend
}

// clientKey identifies the caller for failure limiting. Behind Cloudflare every request comes from
// the tunnel connector, so the CF-Connecting-IP header is part of the key. It can be forged, which
// only lets a caller evade its own limit; the token length is what prevents guessing.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" && len(cf) <= 64 {
		return host + "|" + cf
	}
	return host
}

const maxFailEntries = 10000

func (a *Authenticator) limited(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.failures[key]
	return e != nil && e.lim.TokensAt(a.now()) < 1
}

func (a *Authenticator) recordFailure(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	e := a.failures[key]
	if e == nil {
		if len(a.failures) >= maxFailEntries {
			// Evict the least recently seen entry (never a full reset, which would clear every limit).
			oldestKey, oldest := "", now
			for k, v := range a.failures {
				if v.seen.Before(oldest) || oldestKey == "" {
					oldestKey, oldest = k, v.seen
				}
			}
			delete(a.failures, oldestKey)
		}
		// 10 failed attempts, then one more every 6 seconds.
		e = &failEntry{lim: rate.NewLimiter(rate.Every(6*time.Second), 10)}
		a.failures[key] = e
	}
	e.seen = now
	e.lim.AllowN(now, 1)
}

// logFailure logs failed attempts with a global rate limit, so a flood cannot fill the log.
func (a *Authenticator) logFailure() {
	a.mu.Lock()
	allow := a.logLimit.AllowN(a.now(), 1)
	suppressed := a.suppressed
	if allow {
		a.suppressed = 0
	} else {
		a.suppressed++
	}
	a.mu.Unlock()
	if allow {
		// No client address: behind a tunnel it is the visitor's IP, which is personal data.
		// The JWT and bearer token are not logged either.
		msg := "rejected MCP request without a valid token"
		a.log.Warn(msg, "suppressed_since_last", suppressed)
	}
}

// Middleware rejects requests without the bearer token. Nothing about the token is logged.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r)
		if a.limited(key) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		role := a.RoleFrom(r.Header)
		if role == RoleSendInactive {
			http.Error(w, "the send token is not accepted while enable_send is off", http.StatusForbidden)
			return
		}
		if role != RoleRead && role != RoleSend {
			a.recordFailure(key)
			a.logFailure()
			w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewMCPHandler builds the exposed handler: GET /healthz (no details) and the authenticated /mcp.
// Requests with the send token get sendServer (read and send tools); every other authenticated request
// gets readServer. sendServer may be nil when sending is not active. Every other path returns 404.
func NewMCPHandler(readServer, sendServer *mcp.Server, auth *Authenticator) http.Handler {
	pick := func(r *http.Request) *mcp.Server {
		if sendServer != nil && auth.RoleFrom(r.Header) == RoleSend {
			return sendServer
		}
		return readServer
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(pick, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: 1 << 20,
	})
	protected := auth.Middleware(http.NewCrossOriginProtection().Handler(mcpHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle(MCPPath, protected)
	mux.HandleFunc("/", http.NotFound)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
