package proxy

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/api"
)

const (
	defaultWSMaxConnectionsPerKey = 64
	defaultWSMaxConnectionsPerIP  = 16
	defaultWSMaxConnections       = 4096
)

// wsConnectionLimiter bounds downstream WebSocket connections per API key,
// per client IP for anonymous mode, and globally. Connections are cheap to
// open but expensive to keep: each one holds a goroutine, file descriptor and
// read buffer for as long as the client stays silent.
type wsConnectionLimiter struct {
	mu        sync.Mutex
	maxTotal  int
	maxPerKey int
	maxPerIP  int
	total     int
	perKey    map[int64]int
	perIP     map[string]int
}

func newWSConnectionLimiter(maxTotal, maxPerKey int) *wsConnectionLimiter {
	if maxTotal <= 0 {
		maxTotal = defaultWSMaxConnections
	}
	if maxPerKey <= 0 {
		maxPerKey = defaultWSMaxConnectionsPerKey
	}
	return &wsConnectionLimiter{
		maxTotal:  maxTotal,
		maxPerKey: maxPerKey,
		maxPerIP:  defaultWSMaxConnectionsPerIP,
		perKey:    make(map[int64]int),
		perIP:     make(map[string]int),
	}
}

var globalWSConnectionLimiter = newWSConnectionLimiter(defaultWSMaxConnections, defaultWSMaxConnectionsPerKey)

// acquire registers one connection. The returned release is idempotent.
func (l *wsConnectionLimiter) acquire(keyID int64, clientIP string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxTotal {
		return nil, false
	}
	useKey := keyID > 0
	useIP := !useKey && clientIP != ""
	if useKey && l.perKey[keyID] >= l.maxPerKey {
		return nil, false
	}
	if useIP && l.perIP[clientIP] >= l.maxPerIP {
		return nil, false
	}
	l.total++
	if useKey {
		l.perKey[keyID]++
	}
	if useIP {
		l.perIP[clientIP]++
	}
	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.total--
		if useKey {
			l.perKey[keyID]--
			if l.perKey[keyID] <= 0 {
				delete(l.perKey, keyID)
			}
		}
		if useIP {
			l.perIP[clientIP]--
			if l.perIP[clientIP] <= 0 {
				delete(l.perIP, clientIP)
			}
		}
	}, true
}

// acquireDownstreamWebSocket reserves a slot before the HTTP connection is
// hijacked. On rejection the caller must not upgrade.
func (h *Handler) acquireDownstreamWebSocket(c *gin.Context) (func(), bool) {
	release, ok := globalWSConnectionLimiter.acquire(requestAPIKeyID(c), c.ClientIP())
	if !ok {
		c.Header("Retry-After", "5")
		api.SendErrorWithStatus(c, api.NewAPIError(
			api.ErrCodeServiceUnavailable,
			"too many concurrent WebSocket connections",
			api.ErrorTypeServer,
		), http.StatusServiceUnavailable)
		return nil, false
	}
	return release, true
}
