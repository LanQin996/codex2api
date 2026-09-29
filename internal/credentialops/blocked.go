package credentialops

import (
	"errors"
	"regexp"
)

var ErrAccountBlocked = errors.New("account deactivated or suspended; manual review required")

// Match explicit account-level codes only, never HTTP status or generic words
// such as disabled (which could refer to a proxy, token or organization).
var blockedCode = regexp.MustCompile(`"code"s*:s*"(?:account_deactivated|account_disabled|account_suspended|account_banned|user_deactivated)"`)

func IsAccountBlockedResponse(message string) bool { return blockedCode.MatchString(message) }

// Keep only a tiny rolling window to detect codes split across pipe writes.
// Nothing from protocol output is logged, persisted, or returned to the caller.
type blockedOutput struct {
	tail    []byte
	blocked bool
}

func (w *blockedOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		size := min(len(p), 1024)
		w.tail = append(w.tail, p[:size]...)
		if blockedCode.Match(w.tail) {
			w.blocked = true
		}
		if len(w.tail) > 256 {
			copy(w.tail, w.tail[len(w.tail)-256:])
			clear(w.tail[256:])
			w.tail = w.tail[:256]
		}
		p = p[size:]
	}
	return n, nil
}
