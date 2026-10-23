package wsrelay

import (
	"sync/atomic"

	"github.com/codex2api/auth"
)

// IdleMetadataObserver receives connection-level metadata frames that the read
// pump would otherwise drop (they arrive outside any request lease, typically
// shortly after a terminal frame). The observer must not block: it runs on the
// sole raw reader goroutine.
type IdleMetadataObserver func(account *auth.Account, eventType string, payload []byte)

var idleMetadataObserver atomic.Pointer[IdleMetadataObserver]

// SetIdleMetadataObserver installs the connection-level metadata observer.
// Passing nil removes it. The proxy package registers a usage-only observer so
// post-turn rate-limit frames can update the account snapshot without being
// routed to a request.
func SetIdleMetadataObserver(observer IdleMetadataObserver) {
	if observer == nil {
		idleMetadataObserver.Store(nil)
		return
	}
	idleMetadataObserver.Store(&observer)
}

func notifyIdleMetadataObserver(account *auth.Account, eventType string, payload []byte) {
	observer := idleMetadataObserver.Load()
	if observer == nil {
		return
	}
	(*observer)(account, eventType, payload)
}
