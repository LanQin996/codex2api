package admin

import (
	"sync"
	"time"
)

const (
	adminAuthFailureWindow = 10 * time.Minute
	adminAuthBlockDuration = 10 * time.Minute
	adminAuthMaxFailures   = 5
	adminAuthMaxEntries    = 4096
)

// adminAuthThrottle 记录按 IP 的管理端鉴权失败次数。
// 只在失败时计数；达到阈值后在密钥比较前阻断该 IP 的所有认证尝试。
type adminAuthThrottle struct {
	mu      sync.Mutex
	entries map[string]*adminAuthThrottleEntry
}

type adminAuthThrottleEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

func newAdminAuthThrottle() *adminAuthThrottle {
	return &adminAuthThrottle{entries: make(map[string]*adminAuthThrottleEntry)}
}

// retryAfter 返回该 IP 剩余的阻断时间；未阻断时返回 0。
func (t *adminAuthThrottle) retryAfter(ip string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[ip]
	if entry == nil {
		t.evictLocked(now)
		return t.capacityRetryAfterLocked(now)
	}
	if !now.Before(entry.blockedUntil) {
		return 0
	}
	return entry.blockedUntil.Sub(now)
}

// recordFailure 记录一次失败；达到阈值时开始阻断并返回阻断时长。
func (t *adminAuthThrottle) recordFailure(ip string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[ip]
	if entry == nil {
		t.evictLocked(now)
		if retry := t.capacityRetryAfterLocked(now); retry > 0 {
			return retry
		}
		entry = &adminAuthThrottleEntry{windowStart: now}
		t.entries[ip] = entry
	}
	if now.Before(entry.blockedUntil) {
		return entry.blockedUntil.Sub(now)
	}
	if !now.Before(entry.windowStart.Add(adminAuthFailureWindow)) {
		entry.failures = 0
		entry.windowStart = now
	}
	entry.failures++
	if entry.failures < adminAuthMaxFailures {
		return 0
	}
	entry.failures = 0
	entry.windowStart = now
	entry.blockedUntil = now.Add(adminAuthBlockDuration)
	return adminAuthBlockDuration
}

func (t *adminAuthThrottle) reset(ip string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.entries, ip)
	t.mu.Unlock()
}

// evictLocked only removes expired entries; live failures and blocks survive saturation.
func (t *adminAuthThrottle) evictLocked(now time.Time) {
	if len(t.entries) < adminAuthMaxEntries {
		return
	}
	for ip, entry := range t.entries {
		if !now.Before(entry.blockedUntil) && !now.Before(entry.windowStart.Add(adminAuthFailureWindow)) {
			delete(t.entries, ip)
		}
	}
}

// Untracked sources wait for the first slot to expire before key comparison.
func (t *adminAuthThrottle) capacityRetryAfterLocked(now time.Time) time.Duration {
	if len(t.entries) < adminAuthMaxEntries {
		return 0
	}
	var earliest time.Time
	for _, entry := range t.entries {
		expires := entry.windowStart.Add(adminAuthFailureWindow)
		if entry.blockedUntil.After(expires) {
			expires = entry.blockedUntil
		}
		if earliest.IsZero() || expires.Before(earliest) {
			earliest = expires
		}
	}
	return earliest.Sub(now)
}
