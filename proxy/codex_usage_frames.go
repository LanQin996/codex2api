package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	codexRateLimitsFrameType       = "codex.rate_limits"
	codexResponseMetadataFrameType = "codex.response.metadata"
	responseMetadataFrameType      = "response.metadata"
	// Reset offsets are later converted to time.Duration in the shared parser.
	maxCodexFrameResetSeconds = int64((1<<63 - 1) / time.Second)
)

// codexUsageFramePersistIntervalNanos bounds how often a WS frame observation
// is written to the database. The in-memory snapshot is updated on every frame;
// persistence only matters for restart recovery, so a burst across a large pool
// must not turn every turn into a write. Operators can tune the floor with
// CODEX_WS_USAGE_FRAME_PERSIST_SECONDS (default 30; threshold crossings, 5pp
// deltas and 100% still force a write regardless of the interval).
var codexUsageFramePersistIntervalNanos = func() *atomic.Int64 {
	value := &atomic.Int64{}
	value.Store(int64(auth.UsageFramePersistInterval))
	return value
}()

// codexUsageFrameIngestEnabled is the deployment-level kill switch for WS
// quota ingestion. When disabled, usage falls back to WHAM probes and HTTP
// response headers without rebuilding the image.
var codexUsageFrameIngestEnabled = func() *atomic.Bool {
	value := &atomic.Bool{}
	value.Store(true)
	return value
}()

// SetCodexUsageFrameIngestEnabled overrides the WS frame ingestion switch.
// Call before serving traffic.
func SetCodexUsageFrameIngestEnabled(enabled bool) {
	codexUsageFrameIngestEnabled.Store(enabled)
}

// SetCodexUsageFramePersistInterval overrides the frame persistence cadence.
// Values <= 0 restore the default. Call before serving traffic.
func SetCodexUsageFramePersistInterval(interval time.Duration) {
	if interval <= 0 {
		interval = auth.UsageFramePersistInterval
	}
	codexUsageFramePersistIntervalNanos.Store(int64(interval))
}

func codexUsageFramePersistInterval() time.Duration {
	return time.Duration(codexUsageFramePersistIntervalNanos.Load())
}

// ApplyCodexUsageEventFrame ingests the preflight metadata frames that the
// upstream Codex WebSocket sends in place of response headers. On the WS
// transport there are no x-codex-* response headers per turn, so without this
// path the account usage snapshot only advances on WHAM probes. Frames are
// already on the wire (the gateway forwards them downstream), so normal ingest
// adds no upstream requests or request headers. Observation queue overflow can
// request the existing authoritative probe to recover dropped partial frames.
func ApplyCodexUsageEventFrame(store *auth.Store, account *auth.Account, eventType string, frame gjson.Result, observedAt time.Time) {
	// Relay accounts can pass through their backend's quota frames. Those
	// describe the relay's own upstream account, not this account's Codex
	// windows, so they must not drive local usage or cooldowns.
	if account == nil || account.IsRelayStyle() || !codexUsageFrameIngestEnabled.Load() {
		return
	}
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case codexRateLimitsFrameType:
		applyCodexRateLimitsFrame(store, account, frame, observedAt)
	case codexResponseMetadataFrameType, responseMetadataFrameType:
		applyCodexResponseMetadataFrame(store, account, frame, observedAt)
	}
}

// applyCodexRateLimitsFrame maps the codex.rate_limits event onto the same
// synthetic headers the HTTP transport exposes. Only the default "codex" limit
// is applied to the account's 5h/7d snapshots; metered limits (for example a
// Spark or reserve bucket) must not overwrite the main windows.
func applyCodexRateLimitsFrame(store *auth.Store, account *auth.Account, frame gjson.Result, observedAt time.Time) {
	headers := codexRateLimitsHeaders(frame, observedAt)
	if len(headers) == 0 {
		return
	}
	applyCodexUsageHeaders(store, account, headers, observedAt, codexRateLimitsFrameType)
}

func codexRateLimitsHeaders(frame gjson.Result, observedAt time.Time) http.Header {
	if name := firstCodexFrameString(frame, "metered_limit_name", "limit_id", "limit_name"); name != "" && !isDefaultCodexLimitName(name) {
		return nil
	}
	headers := make(http.Header)
	if plan, ok := codexFrameText(frame.Get("plan_type").String(), 32); ok {
		headers.Set("x-codex-plan-type", plan)
	}
	setCodexFrameWindow(headers, "x-codex-primary-", frame.Get("rate_limits.primary"), observedAt)
	setCodexFrameWindow(headers, "x-codex-secondary-", frame.Get("rate_limits.secondary"), observedAt)
	// The frame carries a compact credits snapshot. Only adopt it when both
	// booleans are present, matching the sparse-header contract: a missing flag
	// must not overwrite a richer WHAM observation with false.
	if credits := frame.Get("credits"); credits.IsObject() {
		hasCredits := credits.Get("has_credits")
		unlimited := credits.Get("unlimited")
		if hasCredits.Exists() && unlimited.Exists() {
			headers.Set("x-codex-credits-has-credits", hasCredits.String())
			headers.Set("x-codex-credits-unlimited", unlimited.String())
			if balance, ok := codexFrameText(credits.Get("balance").String(), 128); ok {
				headers.Set("x-codex-credits-balance", balance)
			}
		}
	}
	return headers
}

// codexFrameText validates free-text values copied from an upstream frame.
// These values can reach plain-text logs or persistence, so reject control
// characters that could forge a line.
func codexFrameText(value string, maxLen int) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLen {
		return "", false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", false
		}
	}
	return value, true
}

// applyCodexResponseMetadataFrame adopts the quota headers embedded in a
// codex.response.metadata frame. The same frame also carries protocol routing
// data (x-codex-turn-state) and model catalog hints (x-models-etag); only the
// explicit quota-header allowlist below is copied, so routing or model metadata
// never reaches the usage sync or its persistence path.
func applyCodexResponseMetadataFrame(store *auth.Store, account *auth.Account, frame gjson.Result, observedAt time.Time) {
	headers := codexResponseMetadataHeaders(frame, observedAt)
	if len(headers) == 0 {
		return
	}
	applyCodexUsageHeaders(store, account, headers, observedAt, codexResponseMetadataFrameType)
}

func codexResponseMetadataHeaders(frame gjson.Result, observedAt time.Time) http.Header {
	headers := make(http.Header)
	// Absolute reset values win over a relative header carrying the same
	// window, regardless of the JSON key order in the frame.
	absoluteResets := make(map[string]string, 2)
	frame.Get("headers").ForEach(func(key, value gjson.Result) bool {
		name := strings.ToLower(strings.TrimSpace(key.String()))
		if !isCodexUsageFrameHeader(name) {
			return true
		}
		// The official client accepts header values that are arrays and reads the
		// first element (json_value_as_string). Mirror that so a wrapped value
		// does not silently drop the whole window.
		if value.IsArray() {
			items := value.Array()
			if len(items) == 0 {
				return true
			}
			value = items[0]
		}
		switch value.Type {
		case gjson.String, gjson.Number, gjson.True, gjson.False:
		default:
			return true
		}
		// Convert the absolute reset form to the relative form using the frame's
		// observation time. The header parser otherwise resolves it against the
		// wall clock, which is identical in production but not in deterministic
		// tests and can drift if a frame is replayed later.
		if strings.HasSuffix(name, "-reset-at") {
			epoch := codexFrameInt64(value)
			if epoch > 0 {
				seconds := epoch - observedAt.Unix()
				if seconds > maxCodexFrameResetSeconds {
					return true
				}
				if seconds < 0 {
					seconds = 0
				}
				prefix := strings.TrimSuffix(name, "reset-at")
				absoluteResets[prefix+"reset-after-seconds"] = strconv.FormatInt(seconds, 10)
			}
			return true
		}
		if strings.HasSuffix(name, "-reset-after-seconds") {
			seconds, ok := codexFrameInt64Value(value)
			if !ok || seconds > maxCodexFrameResetSeconds {
				return true
			}
			headers.Set(name, strconv.FormatInt(max(seconds, 0), 10))
			return true
		}
		raw, ok := codexFrameText(value.String(), 256)
		if !ok {
			return true
		}
		headers.Set(name, raw)
		return true
	})
	for name, value := range absoluteResets {
		headers.Set(name, value)
	}
	return headers
}

// isCodexUsageFrameHeader lists the headers consumed by the synthetic HTTP
// response. The metadata frame also carries routing and model fields in the
// same object; an explicit allowlist keeps those out of the usage path even if
// the sync later starts persisting more header fields.
func isCodexUsageFrameHeader(name string) bool {
	switch name {
	case "x-codex-plan-type",
		"x-codex-primary-used-percent", "x-codex-primary-window-minutes",
		"x-codex-primary-reset-at", "x-codex-primary-reset-after-seconds",
		"x-codex-secondary-used-percent", "x-codex-secondary-window-minutes",
		"x-codex-secondary-reset-at", "x-codex-secondary-reset-after-seconds",
		"x-codex-credits-has-credits", "x-codex-credits-unlimited",
		"x-codex-credits-balance", "x-codex-credits-overage-reached",
		"x-codex-rate-limit-reached-type":
		return true
	}
	return false
}

func applyCodexUsageHeaders(store *auth.Store, account *auth.Account, headers http.Header, observedAt time.Time, kind string) {
	if len(headers) == 0 {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	// WS frames do not carry the per-response rate_limit_reached_type field, and
	// the shared sync treats an absent value as "not reached" (correct for HTTP
	// response headers). Preserve the last workspace hard stop instead of
	// clearing it; only WHAM or an explicit metadata header may clear it.
	resp := &http.Response{Header: headers}
	// A per-turn frame may omit one window; never let it erase a valid snapshot.
	// WHAM probes remain the authority for window presence/absence. The persist
	// decision runs inside ApplyUsageObservation so a stale/out-of-order frame
	// cannot advance the throttling state without being applied.
	store.ApplyUsageFrameObservation(account, observedAt, func() {
		if !codexUsageFrameIngestEnabled.Load() {
			return
		}
		result := syncCodexUsageState(store, account, resp, observedAt, codexUsageSyncOptions{
			persistUsageSnapshot:   true,
			clearAbsent5h:          false,
			asyncPersist:           true,
			preserveAbsentHardStop: true,
			observationLocked:      true,
			shouldPersistUsage: func(pct5h float64, has5h bool, pct7d float64, has7d bool) bool {
				return account.ShouldPersistUsageFrame(observedAt, pct5h, has5h, pct7d, has7d, codexUsageFramePersistInterval())
			},
		})
		if result.Applied {
			account.MarkUsageFrameObserved(kind, observedAt)
		}
	})
}

func setCodexFrameWindow(headers http.Header, prefix string, window gjson.Result, observedAt time.Time) {
	if !window.IsObject() {
		return
	}
	used := window.Get("used_percent")
	minutes := window.Get("window_minutes")
	if !used.Exists() || !minutes.Exists() {
		return
	}
	headers.Set(prefix+"used-percent", used.String())
	headers.Set(prefix+"window-minutes", minutes.String())
	if reset := codexFrameResetSeconds(window, observedAt); reset != "" {
		headers.Set(prefix+"reset-after-seconds", reset)
	}
}

// codexFrameResetSeconds accepts both relative and absolute reset shapes so the
// parser tolerates upstream schema drift. The official frame uses reset_at
// (absolute Unix seconds); older/observed fixtures used resets_in_seconds or
// reset_after_seconds.
func codexFrameResetSeconds(window gjson.Result, observedAt time.Time) string {
	relativeSeen := false
	for _, key := range []string{"resets_in_seconds", "reset_after_seconds"} {
		if value := window.Get(key); value.Exists() {
			if n, ok := codexFrameInt64Value(value); ok && n <= maxCodexFrameResetSeconds {
				relativeSeen = true
				// Zero/negative means "resets now", matching the absolute form
				// where a past epoch is clamped to zero. A positive relative
				// value is authoritative; a zero yields to a future absolute
				// reset below so the real window end is not discarded.
				if n > 0 {
					return strconv.FormatInt(n, 10)
				}
			}
		}
	}
	for _, key := range []string{"reset_at", "resets_at"} {
		if value := window.Get(key); value.Exists() {
			epoch, ok := codexFrameInt64Value(value)
			if !ok || epoch <= 0 {
				continue
			}
			seconds := epoch - observedAt.Unix()
			if seconds > maxCodexFrameResetSeconds {
				continue
			}
			if seconds < 0 {
				seconds = 0
			}
			return strconv.FormatInt(seconds, 10)
		}
	}
	if relativeSeen {
		return "0"
	}
	return ""
}

func codexFrameInt64Value(value gjson.Result) (int64, bool) {
	// Parse the original textual integer, not gjson's permissive Int conversion:
	// fractional/exponent numbers and overflow must not become reset deadlines.
	raw := value.String()
	if value.Type == gjson.Number {
		raw = value.Raw
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func codexFrameInt64(value gjson.Result) int64 {
	n, _ := codexFrameInt64Value(value)
	return n
}

func firstCodexFrameString(frame gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(frame.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

var codexLimitNameSeparators = strings.NewReplacer("_", "-", " ", "-")

func isDefaultCodexLimitName(name string) bool {
	return codexLimitNameSeparators.Replace(strings.ToLower(strings.TrimSpace(name))) == "codex"
}

// ApplyCodexIdleUsageFrame is the observer for connection-level metadata frames
// that arrive after a response terminal (the wsrelay read pump drops those
// instead of routing them to a request). Both frame paths share the same ordered
// runtime update and coalescing persistence. A busy observation lock queues the
// partial frames in order instead of blocking the read pump or discarding the
// final update.
func ApplyCodexIdleUsageFrame(store *auth.Store, account *auth.Account, eventType string, payload []byte) {
	if account == nil || account.IsRelayStyle() || len(payload) == 0 || !codexUsageFrameIngestEnabled.Load() {
		return
	}
	normalizedType := strings.ToLower(strings.TrimSpace(eventType))
	switch normalizedType {
	case codexRateLimitsFrameType, codexResponseMetadataFrameType, responseMetadataFrameType:
	default:
		return
	}
	// Only quota-bearing frames are parsed. The read pump also forwards
	// responsesapi.* telemetry frames here; skipping them before gjson keeps
	// the sole raw reader cheap.
	observedAt := time.Now()
	frame := gjson.ParseBytes(payload)
	var headers http.Header
	switch normalizedType {
	case codexRateLimitsFrameType:
		headers = codexRateLimitsHeaders(frame, observedAt)
	default:
		headers = codexResponseMetadataHeaders(frame, observedAt)
	}
	if len(headers) == 0 {
		return
	}
	applyCodexUsageHeaders(store, account, headers, observedAt, normalizedType)
}
