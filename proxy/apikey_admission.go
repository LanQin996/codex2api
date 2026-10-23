package proxy

import (
	"sync"
	"time"
)

const (
	apiKeyAdmissionRPMSlots = 60
	apiKeyAdmissionMaxKeys  = 8192
	apiKeyAdmissionIdleTTL  = 10 * time.Minute
)

// apiKeyAdmissionLimiter 在进程内按实际请求递增 RPM/RPD 计数。
//
// 数据库聚合 + 60s 缓存适合展示和跨进程兜底，但不能约束同一缓存窗口内的并发
// 突发：所有请求都读到同一个旧值。这里在准入点用原子计数补上这一段，缓存只负责
// 历史用量。多实例部署时每个副本各有一份计数，跨副本仍需共享计数（见 issue）。
type apiKeyAdmissionLimiter struct {
	mu      sync.Mutex
	entries map[int64]*apiKeyAdmissionEntry
}

type apiKeyAdmissionEntry struct {
	rpmBuckets [apiKeyAdmissionRPMSlots]int64
	rpmSecond  int64
	rpdBuckets []apiKeyAdmissionDayBucket
	rpdCount   int64
	lastSeen   time.Time
}

type apiKeyAdmissionDayBucket struct {
	lastNanos int64
	count     int64
}

func newAPIKeyAdmissionLimiter() *apiKeyAdmissionLimiter {
	return &apiKeyAdmissionLimiter{entries: make(map[int64]*apiKeyAdmissionEntry)}
}

func (h *Handler) admissionLimiter() *apiKeyAdmissionLimiter {
	if h == nil {
		return nil
	}
	h.admissionOnce.Do(func() {
		h.admission = newAPIKeyAdmissionLimiter()
	})
	return h.admission
}

// allow 在 RPM/RPD 限额内为 requests 个上游请求预留计数。
// limit <= 0 表示该维度不限制。拒绝时返回 "rpm" 或 "rpd"。
func (l *apiKeyAdmissionLimiter) allow(keyID int64, rpm, rpd, requests int, now time.Time) (bool, string) {
	if l == nil || keyID <= 0 || requests <= 0 {
		return true, ""
	}
	if rpm <= 0 && rpd <= 0 {
		return true, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := l.entries[keyID]
	if entry == nil {
		// 先清理再插入，否则满表时新键会被自己的淘汰逻辑删掉。
		l.evictLocked(now)
		if len(l.entries) >= apiKeyAdmissionMaxKeys {
			return false, "capacity"
		}
		entry = &apiKeyAdmissionEntry{}
		l.entries[keyID] = entry
	}
	entry.lastSeen = now

	if rpm > 0 && entry.rpmUsed(now)+int64(requests) > int64(rpm) {
		return false, "rpm"
	}
	if rpd > 0 {
		entry.expireRPD(now)
		if entry.rpdCount+int64(requests) > int64(rpd) {
			return false, "rpd"
		}
	}

	// 两个维度都通过后再一起记账，避免 RPD 拒绝时仍消耗 RPM 配额。
	if rpm > 0 {
		entry.rpmBuckets[now.Unix()%apiKeyAdmissionRPMSlots] += int64(requests)
	}
	if rpd > 0 {
		entry.recordRPD(now, int64(requests))
	}
	return true, ""
}

// Minute-coalesced admissions keep the last 24h without one allocation per
// request. A bucket expires 24h after its last admission, so rounding can only
// retain counts for less than one extra minute, never reset recent usage early.
func (e *apiKeyAdmissionEntry) expireRPD(now time.Time) {
	cutoff := now.Add(-apiKeyRPDWindow).UnixNano()
	expired := 0
	for expired < len(e.rpdBuckets) && e.rpdBuckets[expired].lastNanos <= cutoff {
		e.rpdCount -= e.rpdBuckets[expired].count
		expired++
	}
	if expired > 0 {
		copy(e.rpdBuckets, e.rpdBuckets[expired:])
		e.rpdBuckets = e.rpdBuckets[:len(e.rpdBuckets)-expired]
	}
}

func (e *apiKeyAdmissionEntry) recordRPD(now time.Time, count int64) {
	nanos := now.UnixNano()
	if n := len(e.rpdBuckets); n > 0 && nanos/int64(time.Minute) <= e.rpdBuckets[n-1].lastNanos/int64(time.Minute) {
		bucket := &e.rpdBuckets[n-1]
		bucket.lastNanos = max(bucket.lastNanos, nanos)
		bucket.count += count
	} else {
		e.rpdBuckets = append(e.rpdBuckets, apiKeyAdmissionDayBucket{lastNanos: nanos, count: count})
	}
	e.rpdCount += count
}

// rpmUsed 推进秒级桶并返回最近 60 秒的计数，不写入本次请求。
func (e *apiKeyAdmissionEntry) rpmUsed(now time.Time) int64 {
	second := now.Unix()
	switch {
	case e.rpmSecond == 0 || second < e.rpmSecond || second-e.rpmSecond >= apiKeyAdmissionRPMSlots:
		e.rpmBuckets = [apiKeyAdmissionRPMSlots]int64{}
		e.rpmSecond = second
	default:
		for s := e.rpmSecond + 1; s <= second; s++ {
			e.rpmBuckets[s%apiKeyAdmissionRPMSlots] = 0
		}
		e.rpmSecond = second
	}
	var used int64
	for _, count := range e.rpmBuckets {
		used += count
	}
	return used
}

func (l *apiKeyAdmissionLimiter) evictLocked(now time.Time) {
	if len(l.entries) < apiKeyAdmissionMaxKeys {
		return
	}
	for key, entry := range l.entries {
		entry.expireRPD(now)
		if now.Sub(entry.lastSeen) > apiKeyAdmissionIdleTTL && entry.rpdCount == 0 {
			delete(l.entries, key)
		}
	}
	// Never evict live quota history to admit another key: doing so would
	// reset its counters. New keys fail closed until an idle entry expires.
}
