package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/egressip"
	"github.com/google/uuid"
)

// CodexTurnStateTicketConfig is the runtime copy of the persisted harvester
// settings. The pointer is swapped atomically so request paths never wait on
// the admin/database settings lock.
type CodexTurnStateTicketConfig struct {
	Enabled               bool
	HarvestProxyURL       string
	Models                []string
	ProbeModels           []string
	TargetLength          int
	TTLSeconds            int
	RefreshBeforeSeconds  int
	ProbeIntervalSeconds  int
	AttemptTimeoutSeconds int
	Concurrency           int
	PreserveExisting      bool
	FailClosed            bool
}

var codexTurnStateTicketConfig atomic.Pointer[CodexTurnStateTicketConfig]

func init() {
	SetCodexTurnStateTicketConfig(databaseCodexTicketConfig(nil))
}

func databaseCodexTicketConfig(s *database.CodexTurnStateSettings) *CodexTurnStateTicketConfig {
	if s == nil {
		return &CodexTurnStateTicketConfig{TargetLength: 292, TTLSeconds: 3600, RefreshBeforeSeconds: 600, ProbeIntervalSeconds: 6, AttemptTimeoutSeconds: 25, Concurrency: 1, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, ProbeModels: []string{"gpt-6-astra", "gpt-5.6-sol"}, PreserveExisting: true}
	}
	models := append([]string(nil), s.Models...)
	probeModels := append([]string(nil), s.ProbeModels...)
	return &CodexTurnStateTicketConfig{Enabled: s.Enabled, HarvestProxyURL: strings.TrimSpace(s.HarvestProxyURL), Models: models, ProbeModels: probeModels, TargetLength: s.TargetLength, TTLSeconds: s.TTLSeconds, RefreshBeforeSeconds: s.RefreshBeforeSeconds, ProbeIntervalSeconds: s.ProbeIntervalSeconds, AttemptTimeoutSeconds: s.AttemptTimeoutSeconds, Concurrency: s.Concurrency, PreserveExisting: s.PreserveExisting, FailClosed: s.FailClosed}
}

func SetCodexTurnStateTicketConfig(cfg *CodexTurnStateTicketConfig) {
	if cfg == nil {
		cfg = databaseCodexTicketConfig(nil)
	}
	copyCfg := *cfg
	copyCfg.Models = append([]string(nil), cfg.Models...)
	copyCfg.ProbeModels = append([]string(nil), cfg.ProbeModels...)
	codexTurnStateTicketConfig.Store(&copyCfg)
}

func CurrentCodexTurnStateTicketConfig() *CodexTurnStateTicketConfig {
	cfg := codexTurnStateTicketConfig.Load()
	if cfg == nil {
		return databaseCodexTicketConfig(nil)
	}
	return cfg
}

func (c *CodexTurnStateTicketConfig) ModelManaged(models ...string) bool {
	if c == nil {
		return false
	}
	for _, candidate := range models {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, managed := range c.Models {
			managed = strings.TrimSpace(managed)
			if managed == "" {
				continue
			}
			if strings.HasSuffix(managed, "*") && strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(strings.TrimSuffix(managed, "*"))) {
				return true
			}
			if strings.EqualFold(candidate, managed) {
				return true
			}
		}
	}
	return false
}

type CodexTurnStateHarvester struct {
	slotMu        sync.Mutex
	accountActive map[int64]int
	store         *auth.Store
	db            *database.DB
	cancel        context.CancelFunc
	done          chan struct{}
	wake          chan struct{}
	start         sync.Once
	stop          sync.Once
	persistMu     sync.Mutex
	tasks         chan codexTurnStateProbeKey
	persist       chan int64
	queueMu       sync.Mutex
	queued        map[codexTurnStateProbeKey]bool
	inflight      map[codexTurnStateProbeKey]bool
	states        map[codexTurnStateProbeKey]*codexTurnStateProbeStatus
	active        atomic.Int64
	pendingMu     sync.Mutex
	pending       map[uint64]*codexTurnStatePendingCapture
	nextID        atomic.Uint64
	probed        atomic.Uint64
	succeeded     atomic.Uint64
	failed        atomic.Uint64
	collected     atomic.Uint64
	injected      atomic.Uint64
	// harvestProxyWarn* 记录上一次已告警的采集代理取值：采集代理为空时探针必然
	// 失败，只在该取值首次出现时告警一次，避免每轮刷新刷屏。
	harvestProxyWarnMu  sync.Mutex
	harvestProxyWarned  string
	harvestProxyWarnSet bool
}

const (
	codexTurnStateMaxTicketsPerAccount = 64
	codexTurnStateMaxPendingCaptures   = 20000
	codexTurnStatePendingTTL           = 2 * time.Hour
	codexTurnStateWorkerCount          = 64
)

type codexTurnStateProbeKey struct {
	accountID int64
	model     string
}

type verifiedCodexTurnStateProbe struct {
	state         string
	proxyURL      string
	sid           string
	verifiedModel string
	exitIP        string
}

type codexTurnStateProbeStopError struct {
	status int
	err    error
}

func (e *codexTurnStateProbeStopError) Error() string {
	if e == nil {
		return ""
	}
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("probe stopped at status=%d", e.status)
}

func (e *codexTurnStateProbeStopError) Unwrap() error { return e.err }

func codexTurnStateStopStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusForbidden || status == http.StatusUnauthorized
}

type codexTurnStateProbeStatus struct {
	Queued         bool
	InFlight       bool
	Attempts       uint64
	Failures       int
	NextAttempt    time.Time
	LastAttempt    time.Time
	LastSuccess    time.Time
	LastDurationMs int64
	LastError      string
}

// CodexTurnStateProbeSnapshot exposes metadata needed by the admin UI without
// ever exposing the opaque ticket value or account credential.
type CodexTurnStateProbeSnapshot struct {
	Queued         bool
	InFlight       bool
	Attempts       uint64
	Failures       int
	NextAttempt    time.Time
	LastAttempt    time.Time
	LastSuccess    time.Time
	LastDurationMs int64
	LastError      string
}

type codexTurnStatePendingCapture struct {
	key           codexTurnStateProbeKey
	createdAt     time.Time
	expiresAt     time.Time
	candidate     string
	responseModel string
}

type codexTurnStatePendingContextKey struct{}

type codexTurnStateRuntimeStatus struct {
	Enabled          bool                          `json:"enabled"`
	Active           bool                          `json:"active"`
	PendingCaptures  int                           `json:"pending_captures"`
	QueuedProbes     int                           `json:"queued_probes"`
	InFlightProbes   int                           `json:"in_flight_probes"`
	ActiveRequests   int                           `json:"active_requests"`
	ConcurrencyLimit int                           `json:"per_account_concurrency_limit"`
	TotalProbed      uint64                        `json:"total_probed"`
	TotalSucceeded   uint64                        `json:"total_succeeded"`
	TotalFailed      uint64                        `json:"total_failed"`
	TotalCollected   uint64                        `json:"total_collected"`
	TotalInjected    uint64                        `json:"total_injected"`
	Truncated        bool                          `json:"truncated,omitempty"`
	Accounts         []codexTurnStateAccountStatus `json:"accounts,omitempty"`
}

type codexTurnStateAccountStatus struct {
	AccountID        int64     `json:"account_id"`
	Model            string    `json:"model"`
	State            string    `json:"state"`
	CapturedAt       time.Time `json:"captured_at,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	LastAttempt      time.Time `json:"last_attempt,omitempty"`
	LastSuccess      time.Time `json:"last_success,omitempty"`
	NextAttempt      time.Time `json:"next_attempt,omitempty"`
	Attempts         uint64    `json:"attempts"`
	Failures         int       `json:"failures"`
	LastDurationMs   int64     `json:"last_duration_ms"`
	Queued           bool      `json:"queued,omitempty"`
	InFlight         bool      `json:"in_flight,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

var activeCodexTurnStateHarvester atomic.Pointer[CodexTurnStateHarvester]

const codexTurnStateAstraMinVersion = "0.153.4"

func applyCodexTurnStateHarvestIdentity(headers http.Header, model string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.Contains(model, "gpt-6") && !strings.Contains(model, "astra") {
		return
	}
	version := strings.TrimSpace(headers.Get("version"))
	if cmp, ok := compareCodexClientVersions(version, codexTurnStateAstraMinVersion); ok && cmp >= 0 {
		return
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	if userAgent == "" {
		userAgent = defaultCodexCLIUserAgent
	}
	userAgent = replaceCodexUserAgentVersion(userAgent, codexTurnStateAstraMinVersion)
	headers.Set("version", codexTurnStateAstraMinVersion)
	headers.Set("User-Agent", userAgent)
	headers.Set("Originator", CodexOriginatorForGeneratedUserAgent(userAgent))
}

func NewCodexTurnStateHarvester(store *auth.Store, db *database.DB) *CodexTurnStateHarvester {
	return &CodexTurnStateHarvester{
		store:    store,
		db:       db,
		tasks:    make(chan codexTurnStateProbeKey, 4096),
		persist:  make(chan int64, 512),
		wake:     make(chan struct{}, 1),
		queued:   make(map[codexTurnStateProbeKey]bool),
		inflight: make(map[codexTurnStateProbeKey]bool),
		states:   make(map[codexTurnStateProbeKey]*codexTurnStateProbeStatus),
		pending:  make(map[uint64]*codexTurnStatePendingCapture),
	}
}

// WakeCodexTurnStateHarvester asks the running harvester to re-evaluate its
// settings and expired tickets immediately. The signal is deliberately
// non-blocking: settings writes must not wait for a probe worker or timer.
func WakeCodexTurnStateHarvester() {
	if h := activeCodexTurnStateHarvester.Load(); h != nil && h.wake != nil {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
}

func (h *CodexTurnStateHarvester) Start(ctx context.Context) {
	if h == nil || h.store == nil || h.db == nil {
		return
	}
	h.start.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		h.cancel = cancel
		h.done = make(chan struct{})
		activeCodexTurnStateHarvester.Store(h)
		go func() {
			defer close(h.done)
			h.loop(loopCtx)
		}()
		for i := 0; i < codexTurnStateWorkerCount; i++ {
			go h.probeWorker(loopCtx)
		}
		go h.persistenceWorker(loopCtx)
	})
}

func (h *CodexTurnStateHarvester) Stop() {
	if h == nil {
		return
	}
	h.stop.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.done != nil {
			<-h.done
		}
		if activeCodexTurnStateHarvester.Load() == h {
			activeCodexTurnStateHarvester.CompareAndSwap(h, nil)
		}
	})
}

// BindCodexTurnStateRequest stages a request-to-account/model association. The
// association is deliberately kept outside the request body and expires on its
// own, so late stream frames cannot attach state to a different account.
func BindCodexTurnStateRequest(ctx context.Context, account *auth.Account, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || account == nil || !isNativeCodexOAuth(account) {
		return ctx
	}
	cfg := CurrentCodexTurnStateTicketConfig()
	model = strings.TrimSpace(model)
	if !cfg.Enabled || model == "" || !cfg.ModelManaged(model) {
		return ctx
	}
	now := time.Now()
	if account.CodexTurnStateTicketInjection(model, cfg.TargetLength, now) == "" || h.ticketNeedsRefresh(account, model, now, cfg) {
		h.enqueueProbe(codexTurnStateProbeKey{accountID: account.ID(), model: strings.ToLower(model)}, now)
	}
	id := h.nextID.Add(1)
	h.pendingMu.Lock()
	h.prunePendingLocked(now)
	if len(h.pending) >= codexTurnStateMaxPendingCaptures {
		// Drop the oldest binding deterministically when the gateway is under
		// stream pressure. It is safer to lose a passive refresh than to let
		// unbounded request IDs accumulate.
		var oldest uint64
		var oldestAt time.Time
		for candidateID, candidate := range h.pending {
			if oldest == 0 || candidate.createdAt.Before(oldestAt) {
				oldest, oldestAt = candidateID, candidate.createdAt
			}
		}
		if oldest != 0 {
			delete(h.pending, oldest)
		}
	}
	h.pending[id] = &codexTurnStatePendingCapture{key: codexTurnStateProbeKey{accountID: account.ID(), model: strings.ToLower(model)}, createdAt: now, expiresAt: now.Add(codexTurnStatePendingTTL)}
	h.pendingMu.Unlock()
	return context.WithValue(ctx, codexTurnStatePendingContextKey{}, id)
}

func (h *CodexTurnStateHarvester) prunePendingLocked(now time.Time) {
	for id, pending := range h.pending {
		if pending == nil || (!pending.expiresAt.IsZero() && !now.Before(pending.expiresAt)) {
			delete(h.pending, id)
		}
	}
}

// StageCodexTurnStateResponse validates and stores a candidate on a pending
// attempt. It does not publish it until CommitCodexTurnStateRequest is called
// by the winning request/stream path.
func StageCodexTurnStateResponse(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	values := headers.Values(codexTurnStateHeader)
	if len(values) != 1 {
		return
	}
	// HTTP exposes the routed model as OpenAIModel/openai-model on both JSON and
	// SSE initial headers. If it is absent, model verification remains unknown
	// rather than being treated as a mismatch.
	StageCodexTurnStateValueWithModel(ctx, values[0], headers.Get("openai-model"))
}

func StageCodexTurnStateValue(ctx context.Context, value string) {
	StageCodexTurnStateValueWithModel(ctx, value, "")
}

func StageCodexTurnStateValueWithModel(ctx context.Context, value, responseModel string) {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || ctx == nil {
		return
	}
	id, ok := ctx.Value(codexTurnStatePendingContextKey{}).(uint64)
	if !ok || id == 0 {
		return
	}
	cfg := CurrentCodexTurnStateTicketConfig()
	value = observedCodexTurnState(value)
	// A degraded reply invalidates only the managed ticket actually used by this
	// attempt, even if the downstream stream later aborts.
	if blocks, valid := auth.CodexTurnStateFernetBlocks(value); valid && blocks == 11 {
		rejectManagedTicketFromContext(ctx)
		return
	}
	if !auth.ValidCodexTurnStateTicketValue(value, cfg.TargetLength) {
		return
	}
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	if pending := h.pending[id]; pending != nil && time.Now().Before(pending.expiresAt) {
		pending.candidate = value
		pending.responseModel = strings.TrimSpace(responseModel)
	}
}

func CommitCodexTurnStateRequest(ctx context.Context) {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || ctx == nil {
		return
	}
	id, ok := ctx.Value(codexTurnStatePendingContextKey{}).(uint64)
	if !ok || id == 0 {
		return
	}
	h.pendingMu.Lock()
	pending := h.pending[id]
	delete(h.pending, id)
	h.pendingMu.Unlock()
	if pending == nil || pending.candidate == "" {
		return
	}
	if account := h.store.FindByID(pending.key.accountID); account != nil {
		if pending.responseModel != "" && !responseModelMatches(pending.key.model, pending.responseModel) {
			h.store.DropAccountCodexTurnStateTicket(account.DBID, pending.key.model)
			h.enqueueProbe(pending.key, time.Now())
			return
		}
		if blocks, ok := auth.CodexTurnStateFernetBlocks(pending.candidate); !ok || (blocks != 10 && blocks != 12) {
			h.store.DropAccountCodexTurnStateTicket(account.DBID, pending.key.model)
			h.enqueueProbe(pending.key, time.Now())
			return
		}
		// 本次尝试真实的出口决策：只有确实走了票据绑定出口，回购到的票据才能
		// 继承那份绑定。
		bound := codexTurnStateBoundProxy(ctx)
		if bound == "" {
			// 这次出站没走绑定出口（账号/分组/代理池/直连）。新票据同样没绑定，
			// 拿它顶掉同模型上仍然有效的绑定票据，等于把粘性出口的保证直接丢掉；
			// 宁可保留旧绑定票据等下一次探针刷新。
			cfg := CurrentCodexTurnStateTicketConfig()
			if ticket, ok := account.CodexTurnStateTicket(pending.key.model, cfg.TargetLength, time.Now()); ok && strings.TrimSpace(ticket.ProxyURL) != "" {
				return
			}
			h.recordTicket(account, pending.key.model, pending.candidate, "response", true)
			return
		}
		h.recordTicketWithBinding(account, pending.key.model, pending.candidate, "response", true, bound, codexTurnStateProxySID(bound), pending.responseModel, "")
	}
}

func AbortCodexTurnStateRequest(ctx context.Context) {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || ctx == nil {
		return
	}
	if id, ok := ctx.Value(codexTurnStatePendingContextKey{}).(uint64); ok && id != 0 {
		h.pendingMu.Lock()
		delete(h.pending, id)
		h.pendingMu.Unlock()
	}
}

func NoteCodexTurnStateInjected() {
	if h := activeCodexTurnStateHarvester.Load(); h != nil {
		h.injected.Add(1)
	}
}

func (h *CodexTurnStateHarvester) persistenceWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case accountID := <-h.persist:
			h.persistAccountTickets(ctx, accountID)
		}
	}
}

func (h *CodexTurnStateHarvester) persistAccountTickets(ctx context.Context, accountID int64) {
	account := h.store.FindByID(accountID)
	if account == nil || h.db == nil {
		return
	}
	account.Mu().RLock()
	tickets := make(map[string]auth.CodexTurnStateTicket, len(account.CodexTurnStateTickets))
	for model, ticket := range account.CodexTurnStateTickets {
		tickets[model] = ticket
	}
	account.Mu().RUnlock()
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(persistCtx, account.DBID, map[string]any{auth.CodexTurnStateTicketsCredentialKey: tickets}); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[codex-turn-state] 账号 %d 票据持久化失败: %v", account.DBID, err)
	}
}

func (h *CodexTurnStateHarvester) enqueuePersist(accountID int64) {
	select {
	case h.persist <- accountID:
	default:
		// The account map is the source of truth during runtime. A later
		// queued write will persist the newest snapshot, so dropping a duplicate
		// notification is safe under bursty stream traffic.
	}
}

func (h *CodexTurnStateHarvester) loop(ctx context.Context) {
	for {
		if err := h.refreshSettings(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[codex-turn-state] 读取采集设置失败: %v", err)
		}
		h.refresh(ctx)
		cfg := CurrentCodexTurnStateTicketConfig()
		interval := h.nextRefreshWait(cfg, time.Now(), time.Duration(cfg.ProbeIntervalSeconds)*time.Second)
		if interval <= 0 {
			interval = 6 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-h.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// nextRefreshWait prevents a long probe interval from hiding an expiring
// ticket. The regular interval remains the upper bound, while the nearest
// managed ticket refresh deadline becomes an earlier wake-up point.
func (h *CodexTurnStateHarvester) nextRefreshWait(cfg *CodexTurnStateTicketConfig, now time.Time, interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = 6 * time.Second
	}
	if h == nil || h.store == nil || cfg == nil || !cfg.Enabled {
		return interval
	}
	wait := interval
	refreshBefore := time.Duration(max(0, cfg.RefreshBeforeSeconds)) * time.Second
	for _, account := range h.store.Accounts() {
		if account == nil || !isNativeCodexOAuth(account) {
			continue
		}
		account.Mu().RLock()
		for model, ticket := range account.CodexTurnStateTickets {
			if !cfg.ModelManaged(model) || ticket.ExpiresAt.IsZero() {
				continue
			}
			due := ticket.ExpiresAt.Add(-refreshBefore)
			if !due.After(now) {
				continue // Already queued or deferred; do not spin on a past deadline.
			}
			if candidate := due.Sub(now); candidate < wait {
				wait = candidate
			}
		}
		account.Mu().RUnlock()
	}
	if wait < 0 {
		return 0
	}
	return wait
}

func (h *CodexTurnStateHarvester) refreshSettings(ctx context.Context) error {
	s, err := h.db.GetCodexTurnStateSettings(ctx)
	if err != nil {
		return err
	}
	SetCodexTurnStateTicketConfig(databaseCodexTicketConfig(s))
	return nil
}

func isNativeCodexOAuth(account *auth.Account) bool {
	if account == nil || account.IsOpenAIResponsesAPI() || account.IsClaudeOAuth() || account.IsGrokAPI() || account.IsAntigravityAPI() || account.IsCodexAgentIdentity() {
		return false
	}
	account.Mu().RLock()
	upstream, refresh, access := strings.ToLower(strings.TrimSpace(account.UpstreamType)), strings.TrimSpace(account.RefreshToken), strings.TrimSpace(account.AccessToken)
	account.Mu().RUnlock()
	if upstream != "" && upstream != "codex" && upstream != "openai" {
		return false
	}
	return refresh != "" || access != ""
}

func (h *CodexTurnStateHarvester) refresh(ctx context.Context) {
	cfg := CurrentCodexTurnStateTicketConfig()
	if h == nil || h.store == nil || h.db == nil || !cfg.Enabled {
		return
	}
	now := time.Now()
	eligibleAccounts := 0
	dueTickets := 0
	queuedTickets := 0
	for _, account := range h.store.Accounts() {
		if !isNativeCodexOAuth(account) || !account.IsAvailableForTicketMaintenance() {
			continue
		}
		eligibleAccounts++
		// Keep learned concrete models eligible even after their ticket expires.
		models := append([]string(nil), cfg.ProbeModels...)
		account.Mu().RLock()
		for model := range account.CodexTurnStateTickets {
			models = append(models, model)
		}
		account.Mu().RUnlock()
		h.queueMu.Lock()
		for key := range h.states {
			if key.accountID == account.ID() {
				models = append(models, key.model)
			}
		}
		h.queueMu.Unlock()
		h.pruneAccountTickets(account, cfg, now)
		for _, model := range models {
			model := strings.TrimSpace(model)
			if model == "" || strings.Contains(model, "*") || !cfg.ModelManaged(model) {
				continue
			}
			if ticket := account.CodexTurnStateTicketInjection(model, cfg.TargetLength, now); ticket != "" && !h.ticketNeedsRefresh(account, model, now, cfg) {
				continue
			}
			dueTickets++
			if h.enqueueProbe(codexTurnStateProbeKey{accountID: account.ID(), model: strings.ToLower(model)}, now) {
				queuedTickets++
			}
		}
	}
	if dueTickets > 0 {
		h.warnHarvestProxyUnset(cfg, eligibleAccounts, dueTickets)
		log.Printf("[codex-turn-state] 定时刷新扫描完成: eligible_accounts=%d probe_models=%d due=%d queued=%d deferred=%d", eligibleAccounts, len(cfg.ProbeModels), dueTickets, queuedTickets, dueTickets-queuedTickets)
	}
}

// warnHarvestProxyUnset 在采集代理未配置时告警：探针一律在拨号前就被拒（不再回退
// 直连探测），票据只会过期不会刷新。这里只如实报告，绝不静默换一条出口去探。
func (h *CodexTurnStateHarvester) warnHarvestProxyUnset(cfg *CodexTurnStateTicketConfig, eligibleAccounts, dueTickets int) {
	proxyURL := ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.HarvestProxyURL)
	}
	if proxyURL != "" {
		return
	}
	h.harvestProxyWarnMu.Lock()
	changed := !h.harvestProxyWarnSet || h.harvestProxyWarned != proxyURL
	h.harvestProxyWarned, h.harvestProxyWarnSet = proxyURL, true
	h.harvestProxyWarnMu.Unlock()
	if !changed {
		return
	}
	log.Printf("[codex-turn-state] 采集代理未配置(harvest_proxy_url 为空): 无法铸造探针票据，本轮跳过 due=%d 个待刷新模型(eligible_accounts=%d)；不回退直连探测，请配置采集代理后重试", dueTickets, eligibleAccounts)
}

func (h *CodexTurnStateHarvester) pruneAccountTickets(account *auth.Account, cfg *CodexTurnStateTicketConfig, now time.Time) {
	if account == nil {
		return
	}
	changed := false
	account.Mu().Lock()
	for model, ticket := range account.CodexTurnStateTickets {
		if !cfg.ModelManaged(model) || !ticket.StoredValid(now, cfg.TargetLength) {
			delete(account.CodexTurnStateTickets, model)
			changed = true
		}
	}
	account.Mu().Unlock()
	if changed {
		h.enqueuePersist(account.ID())
	}
}

func (h *CodexTurnStateHarvester) probeEligible(key codexTurnStateProbeKey) bool {
	if h == nil || h.store == nil {
		return false
	}
	account := h.store.FindByID(key.accountID)
	if !isNativeCodexOAuth(account) || !account.IsAvailableForTicketMaintenance() || account.IsModelRateLimited(key.model) {
		return false
	}
	// Background maintenance must not spend credits to bypass exhausted usage windows.
	switch account.RuntimeStatus() {
	case "usage_exhausted", "rate_limited", "rate_limited_5h", "quota_paused":
		return false
	}
	return true
}

func (h *CodexTurnStateHarvester) enqueueProbe(key codexTurnStateProbeKey, now time.Time) bool {
	if key.accountID <= 0 || key.model == "" || !h.probeEligible(key) {
		return false
	}
	h.queueMu.Lock()
	state := h.states[key]
	if state == nil {
		state = &codexTurnStateProbeStatus{}
		h.states[key] = state
	}
	if h.queued[key] || h.inflight[key] || (!state.NextAttempt.IsZero() && now.Before(state.NextAttempt)) {
		h.queueMu.Unlock()
		return false
	}
	h.queued[key] = true
	state.Queued = true
	h.queueMu.Unlock()
	select {
	case h.tasks <- key:
		return true
	default:
		h.queueMu.Lock()
		delete(h.queued, key)
		state.Queued = false
		h.queueMu.Unlock()
		return false
	}
}

func (h *CodexTurnStateHarvester) probeWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-h.tasks:
			h.runProbeTask(ctx, key)
		}
	}
}

func (h *CodexTurnStateHarvester) runProbeTask(ctx context.Context, key codexTurnStateProbeKey) {
	h.queueMu.Lock()
	delete(h.queued, key)
	state := h.states[key]
	if state == nil {
		state = &codexTurnStateProbeStatus{}
		h.states[key] = state
	}
	state.Queued = false
	state.InFlight = true
	state.Attempts++
	h.inflight[key] = true
	h.queueMu.Unlock()
	defer func() {
		h.queueMu.Lock()
		delete(h.inflight, key)
		state.InFlight = false
		h.queueMu.Unlock()
	}()

	account := h.store.FindByID(key.accountID)
	cfg := CurrentCodexTurnStateTicketConfig()
	if account == nil || !cfg.Enabled || !h.probeEligible(key) {
		return
	}
	h.queueMu.Lock()
	state.LastAttempt = time.Now()
	h.queueMu.Unlock()
	h.probed.Add(1)
	startedAt := time.Now()
	err := h.probe(ctx, account, key.model, cfg)
	h.queueMu.Lock()
	if err == nil {
		state.Failures = 0
		state.NextAttempt = time.Time{}
		state.LastSuccess = time.Now()
		state.LastError = ""
	} else {
		state.Failures++
		retryAfter := codexTurnStateRetryInterval(cfg)
		var stopErr *codexTurnStateProbeStopError
		if errors.As(err, &stopErr) && (stopErr.status == http.StatusTooManyRequests || stopErr.status == http.StatusForbidden) {
			if retryAfter < time.Minute {
				retryAfter = time.Minute
			}
		}
		var upstreamErr *codexTicketHTTPError
		if errors.As(err, &upstreamErr) && upstreamErr.retryAfter > retryAfter {
			retryAfter = upstreamErr.retryAfter
		}
		state.NextAttempt = time.Now().Add(retryAfter)
		state.LastError = codexTurnStateProbeError(err)
		log.Printf("[codex-turn-state] account=%d model=%s failures=%d next_attempt=%s error=%s", key.accountID, key.model, state.Failures, state.NextAttempt.UTC().Format(time.RFC3339), state.LastError)
	}
	failures := state.Failures
	nextAttempt := state.NextAttempt
	state.LastDurationMs = time.Since(startedAt).Milliseconds()
	attempts := state.Attempts
	lastDurationMs := state.LastDurationMs
	h.queueMu.Unlock()
	if err == nil {
		h.succeeded.Add(1)
		log.Printf("[codex-turn-state] 票据刷新成功: account=%d model=%s attempt=%d elapsed_ms=%d expires_in=%ds", account.DBID, key.model, attempts, lastDurationMs, cfg.TTLSeconds)
	} else {
		h.failed.Add(1)
		log.Printf("[codex-turn-state] 票据刷新失败: account=%d model=%s attempt=%d elapsed_ms=%d failures=%d next_attempt=%s error=%v", account.DBID, key.model, attempts, lastDurationMs, failures, nextAttempt.UTC().Format(time.RFC3339), err)
	}
}

func (h *CodexTurnStateHarvester) ticketNeedsRefresh(account *auth.Account, model string, now time.Time, cfg *CodexTurnStateTicketConfig) bool {
	account.Mu().RLock()
	ticket, ok := account.CodexTurnStateTickets[strings.ToLower(strings.TrimSpace(model))]
	account.Mu().RUnlock()
	return !ok || ticket.NeedsVerification || ticket.ExpiresAt.Before(now.Add(time.Duration(cfg.RefreshBeforeSeconds)*time.Second))
}

func (h *CodexTurnStateHarvester) probe(ctx context.Context, account *auth.Account, model string, cfg *CodexTurnStateTicketConfig) error {
	if account == nil || ctx.Err() != nil {
		return ctx.Err()
	}
	account.Mu().RLock()
	token, expires := strings.TrimSpace(account.AccessToken), account.ExpiresAt
	account.Mu().RUnlock()
	if token == "" || (!expires.IsZero() && time.Until(expires) < 2*time.Minute) {
		if err := h.store.RefreshSingle(ctx, account.DBID); err != nil {
			return &codexTurnStateStageError{stage: "token_refresh", err: err}
		}
		account.Mu().RLock()
		token = strings.TrimSpace(account.AccessToken)
		account.Mu().RUnlock()
	}
	if token == "" {
		return errors.New("missing access token")
	}
	if handled, err := h.verifyLoadedTicket(ctx, account, token, model, cfg); handled {
		return err
	}
	winner, err := h.raceVerifiedProbes(ctx, min(64, max(1, cfg.Concurrency)), func(attemptCtx context.Context) (verifiedCodexTurnStateProbe, error) {
		var lastErr error
		for retry := 0; retry < 8; retry++ {
			proxyURL, sid, err := codexTurnStateStickyProxy(cfg.HarvestProxyURL)
			if err != nil {
				return verifiedCodexTurnStateProbe{}, err
			}
			sessionID := uuid.NewString()
			state, status, responseModel, err := h.fireTicketProbe(attemptCtx, account, token, model, cfg, proxyURL, sessionID, "")
			if err == nil && status == http.StatusOK && auth.ValidCodexTurnStateTicketValue(state, cfg.TargetLength) && responseModelMatches(model, responseModel) {
				// Replay the candidate on the same session and egress. An accepted
				// ticket may rotate; retain a valid replacement instead of demanding equality.
				verifyState, verifyStatus, verifyModel, verifyErr := h.fireTicketProbe(attemptCtx, account, token, model, cfg, proxyURL, sessionID, state)
				accepted, ok := acceptedCodexTicket(state, verifyState, verifyStatus, model, verifyModel, cfg.TargetLength)
				if verifyErr == nil && ok {
					return verifiedCodexTurnStateProbe{state: accepted, proxyURL: proxyURL, sid: sid, verifiedModel: firstNonEmptyString(verifyModel, responseModel)}, nil
				}
				if verifyErr != nil {
					lastErr = verifyErr
				} else {
					lastErr = fmt.Errorf("ticket replay rejected: status=%d candidate_length=%d returned_length=%d model=%s", verifyStatus, len(state), len(verifyState), verifyModel)
				}
				if codexTurnStateStopStatus(verifyStatus) {
					return verifiedCodexTurnStateProbe{}, &codexTurnStateProbeStopError{status: verifyStatus, err: lastErr}
				}
				continue
			}
			if err == nil {
				lastErr = fmt.Errorf("probe returned status=%d length=%d model=%s", status, len(state), responseModel)
			} else {
				lastErr = err
			}
			if codexTurnStateStopStatus(status) {
				return verifiedCodexTurnStateProbe{}, &codexTurnStateProbeStopError{status: status, err: lastErr}
			}
			// A degraded sid cannot become healthy by replaying it. Move to a
			// fresh sticky sid immediately; only true transport failures get a
			// short backoff before the next candidate.
			if isTransientCodexTurnStateProbeError(lastErr) {
				timer := time.NewTimer(250 * time.Millisecond)
				select {
				case <-attemptCtx.Done():
					timer.Stop()
					return verifiedCodexTurnStateProbe{}, attemptCtx.Err()
				case <-timer.C:
				}
			}
		}
		return verifiedCodexTurnStateProbe{}, lastErr
	}, account.ID())
	if err != nil {
		return err
	}
	h.publishProbeWinner(ctx, account, model, winner)
	return nil
}

// publishProbeWinner 是探针的收尾：出口 IP 只在这里、只对验证通过的 winner 出口查一次
// （64 个并发 attempt 共用同一个 winner，不会各自去打回显端点），查询失败一律忽略，绝不
// 改变探针成败。proxyURL 含凭据，只用于拨号，绝不写进日志、用量日志或 admin JSON。
func (h *CodexTurnStateHarvester) publishProbeWinner(ctx context.Context, account *auth.Account, model string, winner verifiedCodexTurnStateProbe) {
	winner.exitIP = codexTurnStateExitIP(ctx, winner.proxyURL)
	h.recordTicketWithBinding(account, model, winner.state, "probe", true, winner.proxyURL, winner.sid, winner.verifiedModel, winner.exitIP)
}

// codexTurnStateExitIP 在铸造票据的那条出口上查一次出口 IP：只有它允许离开本进程，
// 代理 URL（含账号密码）不出现在返回值、日志或错误里。任何失败都只返回空串——票据本身
// 已经铸造并验证成功，出口 IP 只是给运营者核对"是不是同一个 IP"的展示信息。
func codexTurnStateExitIP(ctx context.Context, proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	ip, err := egressip.Lookup(ctx, proxyURL)
	if err != nil || ip == "" {
		return ""
	}
	return ip
}

func responseModelMatches(requested, actual string) bool {
	actual = strings.TrimSpace(actual)
	if actual == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(requested), actual)
}

func codexTurnStateStickyProxy(template string) (proxyURL, sid string, err error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", "", errors.New("codex turn-state harvest proxy is empty")
	}
	sid = strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if strings.Contains(template, "sid-") {
		proxyURL = replaceStickyProxySID(template, sid)
	} else {
		proxyURL = template
	}
	return proxyURL, sid, nil
}

func replaceStickyProxySID(raw, sid string) string {
	lower := strings.ToLower(raw)
	start := strings.Index(lower, "sid-")
	if start < 0 {
		return raw
	}
	valueStart := start + len("sid-")
	valueEnd := valueStart + len(codexTurnStateProxySID(raw))
	return raw[:valueStart] + sid + raw[valueEnd:]
}

// codexTurnStateProxySID 取回粘性代理 URL 里 sid- 片段的当前取值，没有该片段返回空。
// 响应回购的票据要记住铸造它的那条粘性出口，sid 是这条出口的粘性键。
func codexTurnStateProxySID(raw string) string {
	lower := strings.ToLower(raw)
	start := strings.Index(lower, "sid-")
	if start < 0 {
		return ""
	}
	valueStart := start + len("sid-")
	valueEnd := valueStart
	for valueEnd < len(raw) && raw[valueEnd] != '-' && raw[valueEnd] != '@' && raw[valueEnd] != ':' {
		valueEnd++
	}
	return raw[valueStart:valueEnd]
}

func (h *CodexTurnStateHarvester) recordTicket(account *auth.Account, model, state, source string, enqueuePersistence bool) {
	h.recordTicketWithBinding(account, model, state, source, enqueuePersistence, "", "", "", "")
}

func (h *CodexTurnStateHarvester) recordTicketWithBinding(account *auth.Account, model, state, source string, enqueuePersistence bool, proxyURL, proxySID, verifiedModel, exitIP string) {
	if h == nil || account == nil {
		return
	}
	cfg := CurrentCodexTurnStateTicketConfig()
	now := time.Now().UTC()
	state = strings.TrimSpace(state)
	if state == "" || !auth.ValidCodexTurnStateTicketValue(state, cfg.TargetLength) {
		return
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return
	}
	blocks, _ := auth.CodexTurnStateFernetBlocks(state)
	ticket := auth.CodexTurnStateTicket{
		State: state, Length: len(state), CapturedAt: now,
		ExpiresAt: now.Add(time.Duration(cfg.TTLSeconds) * time.Second), Source: source,
		ProxyURL: strings.TrimSpace(proxyURL), ProxySID: strings.TrimSpace(proxySID),
		VerifiedModel: strings.TrimSpace(verifiedModel), ExitIP: strings.TrimSpace(exitIP), FernetBlocks: blocks,
	}
	account.Mu().Lock()
	if account.CodexTurnStateTickets == nil {
		account.CodexTurnStateTickets = make(map[string]auth.CodexTurnStateTicket)
	}
	// An echoed opaque value is not a newly minted ticket. Extending its TTL
	// here indefinitely postpones proactive refresh on busy accounts.
	if existing, ok := account.CodexTurnStateTickets[model]; ok && existing.State == state && source != "probe" {
		account.Mu().Unlock()
		return
	}
	account.CodexTurnStateTickets[model] = ticket
	for len(account.CodexTurnStateTickets) > codexTurnStateMaxTicketsPerAccount {
		oldestModel := ""
		var oldest time.Time
		for candidateModel, candidate := range account.CodexTurnStateTickets {
			if candidateModel == model {
				continue
			}
			if oldestModel == "" || candidate.CapturedAt.Before(oldest) {
				oldestModel, oldest = candidateModel, candidate.CapturedAt
			}
		}
		if oldestModel == "" {
			break
		}
		delete(account.CodexTurnStateTickets, oldestModel)
	}
	account.Mu().Unlock()
	h.store.ApplyAccountCodexTurnStateTicket(account.DBID, model, ticket)
	h.collected.Add(1)
	if enqueuePersistence {
		h.enqueuePersist(account.DBID)
	}
}

// CodexTurnStateRuntimeStatus is intentionally metadata-only: it never returns
// the opaque ticket value itself.
func CodexTurnStateRuntimeStatus() any {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil {
		cfg := CurrentCodexTurnStateTicketConfig()
		return codexTurnStateRuntimeStatus{Enabled: cfg.Enabled}
	}
	cfg := CurrentCodexTurnStateTicketConfig()
	status := codexTurnStateRuntimeStatus{
		Enabled: cfg.Enabled,
		Active:  true,
		PendingCaptures: func() int {
			h.pendingMu.Lock()
			defer h.pendingMu.Unlock()
			h.prunePendingLocked(time.Now())
			return len(h.pending)
		}(),
		TotalProbed: h.probed.Load(), TotalSucceeded: h.succeeded.Load(), TotalFailed: h.failed.Load(), TotalCollected: h.collected.Load(), TotalInjected: h.injected.Load(),
	}
	h.queueMu.Lock()
	status.QueuedProbes = len(h.queued)
	status.InFlightProbes = len(h.inflight)
	status.ActiveRequests = int(h.active.Load())
	status.ConcurrencyLimit = max(1, cfg.Concurrency)
	h.queueMu.Unlock()
	if h.store != nil {
		now := time.Now()
		for _, account := range h.store.Accounts() {
			if len(status.Accounts) >= 5000 {
				status.Truncated = true
				break
			}
			if account == nil || !isNativeCodexOAuth(account) {
				continue
			}
			account.Mu().RLock()
			models := make(map[string]struct{}, len(cfg.ProbeModels)+len(account.CodexTurnStateTickets))
			for _, model := range cfg.ProbeModels {
				if model = strings.ToLower(strings.TrimSpace(model)); model != "" {
					models[model] = struct{}{}
				}
			}
			for model := range account.CodexTurnStateTickets {
				models[model] = struct{}{}
			}
			for model := range models {
				if len(status.Accounts) >= 5000 {
					status.Truncated = true
					break
				}
				ticket, hasTicket := account.CodexTurnStateTickets[model]
				key := codexTurnStateProbeKey{accountID: account.ID(), model: model}
				state := "missing"
				if hasTicket && ticket.Valid(now, cfg.TargetLength) {
					state = "ready"
				} else if hasTicket && ticket.NeedsVerification && ticket.StoredValid(now, cfg.TargetLength) {
					state = "unverified"
				} else if hasTicket {
					state = "expired"
				}
				item := codexTurnStateAccountStatus{AccountID: account.ID(), Model: model, State: state}
				if hasTicket {
					item.CapturedAt, item.ExpiresAt = ticket.CapturedAt, ticket.ExpiresAt
				}
				if state == "ready" {
					item.RemainingSeconds = max(0, int64(time.Until(ticket.ExpiresAt).Seconds()))
				}
				h.queueMu.Lock()
				if probeState := h.states[key]; probeState != nil {
					item.LastAttempt, item.LastSuccess, item.NextAttempt = probeState.LastAttempt, probeState.LastSuccess, probeState.NextAttempt
					item.Attempts, item.Failures, item.LastDurationMs = probeState.Attempts, probeState.Failures, probeState.LastDurationMs
					item.Queued, item.InFlight, item.LastError = probeState.Queued, probeState.InFlight, probeState.LastError
					if probeState.InFlight {
						item.State = "refreshing"
					} else if probeState.Queued && item.State != "ready" {
						item.State = "queued"
					}
				}
				h.queueMu.Unlock()
				status.Accounts = append(status.Accounts, item)
			}
			account.Mu().RUnlock()
		}
	}
	return status
}

// CodexTurnStateProbeStatuses returns a copy of probe metadata for one account.
// It is safe for the account response builder to call concurrently with probes.
func CodexTurnStateProbeStatuses(accountID int64) map[string]CodexTurnStateProbeSnapshot {
	result := make(map[string]CodexTurnStateProbeSnapshot)
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || accountID <= 0 {
		return result
	}
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	for key, state := range h.states {
		if key.accountID != accountID || state == nil {
			continue
		}
		result[key.model] = CodexTurnStateProbeSnapshot{
			Queued: state.Queued, InFlight: state.InFlight, Attempts: state.Attempts,
			Failures: state.Failures, NextAttempt: state.NextAttempt, LastAttempt: state.LastAttempt,
			LastSuccess: state.LastSuccess, LastDurationMs: state.LastDurationMs, LastError: state.LastError,
		}
	}
	return result
}

func TriggerCodexTurnStateProbe(accountID int64, model string) bool {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil {
		return false
	}
	cfg := CurrentCodexTurnStateTicketConfig()
	model = strings.TrimSpace(model)
	if !cfg.Enabled || accountID <= 0 || model == "" || !cfg.ModelManaged(model) || !h.probeEligible(codexTurnStateProbeKey{accountID: accountID, model: strings.ToLower(model)}) {
		return false
	}
	h.queueMu.Lock()
	if state := h.states[codexTurnStateProbeKey{accountID: accountID, model: strings.ToLower(model)}]; state != nil {
		state.NextAttempt = time.Time{}
	}
	h.queueMu.Unlock()
	h.enqueueProbe(codexTurnStateProbeKey{accountID: accountID, model: strings.ToLower(model)}, time.Now())
	return true
}

func TriggerCodexTurnStateProbes(accountID int64, model string) int {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || h.store == nil {
		return 0
	}
	model = strings.TrimSpace(model)
	count := 0
	if accountID > 0 {
		if model != "" {
			if TriggerCodexTurnStateProbe(accountID, model) {
				return 1
			}
			return 0
		}
		for _, candidate := range CurrentCodexTurnStateTicketConfig().ProbeModels {
			if TriggerCodexTurnStateProbe(accountID, candidate) {
				count++
			}
		}
		return count
	}
	for _, account := range h.store.Accounts() {
		if account == nil || !isNativeCodexOAuth(account) {
			continue
		}
		for _, candidate := range CurrentCodexTurnStateTicketConfig().ProbeModels {
			if TriggerCodexTurnStateProbe(account.ID(), candidate) {
				count++
			}
		}
	}
	return count
}

func (h *CodexTurnStateHarvester) fireProbe(ctx context.Context, account *auth.Account, token, model string, cfg *CodexTurnStateTicketConfig) (string, int, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}})
	attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.AttemptTimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, CodexBaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", fmt.Sprintf("codex-ticket-%d", time.Now().UnixNano()))
	applyCodexRequestHeaders(req, account, token, req.Header.Get("session_id"), "", nil, http.Header{})
	applyCodexTurnStateHarvestIdentity(req.Header, model)
	resp, err := NewUTLSHttpClient(cfg.HarvestProxyURL).Do(req)
	if err != nil {
		return "", 0, &codexTurnStateStageError{stage: "proxy/upstream_request", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", resp.StatusCode, fmt.Errorf("upstream probe status=%d request_id=%s body=%q", resp.StatusCode, strings.TrimSpace(resp.Header.Get("x-request-id")), strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(resp.Header.Get(codexTurnStateHeader)), resp.StatusCode, nil
}

func (h *CodexTurnStateHarvester) fireProbeWithProxy(ctx context.Context, account *auth.Account, token, model string, cfg *CodexTurnStateTicketConfig, proxyURL string) (string, int, string, error) {
	return h.fireTicketProbe(ctx, account, token, model, cfg, proxyURL, uuid.NewString(), "")
}

func (h *CodexTurnStateHarvester) fireTicketProbe(ctx context.Context, account *auth.Account, token, model string, cfg *CodexTurnStateTicketConfig, proxyURL, sessionID, candidate string) (string, int, string, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}})
	// Candidate scanning only needs the response headers: the turn-state header
	// is minted before the SSE body starts. A degraded candidate that stalls
	// must not occupy a worker for the full stream timeout, so cap the header
	// phase separately and always close the body before moving to the next sid.
	headerTimeout := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
	if headerTimeout <= 0 || headerTimeout > 12*time.Second {
		headerTimeout = 12 * time.Second
	}
	if candidate != "" {
		headerTimeout = time.Duration(max(25, cfg.AttemptTimeoutSeconds)) * time.Second
	}
	attemptCtx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, CodexBaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", 0, "", err
	}
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", sessionID)
	applyCodexRequestHeaders(req, account, token, req.Header.Get("session_id"), "", nil, http.Header{})
	applyCodexTurnStateHarvestIdentity(req.Header, model)
	applyTicketReplayHeader(req.Header, candidate)
	phase := "acquire"
	if candidate != "" {
		phase = "replay"
	}
	started := time.Now()
	client := NewUTLSHttpClient(proxyURL)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, "", &codexTurnStateStageError{stage: "proxy/upstream_request", err: errors.New(redactTicketDiagnostic(err.Error(), token, proxyURL, candidate))}
	}
	log.Printf("[codex-turn-state-probe] account=%d phase=%s model=%s sid=%s status=%d length=%d elapsed_ms=%d request_id=%q cf_ray=%q ua=%q version=%q", account.ID(), phase, model, codexTurnStateProxySID(proxyURL), resp.StatusCode, len(strings.TrimSpace(resp.Header.Get(codexTurnStateHeader))), time.Since(started).Milliseconds(), resp.Header.Get("x-request-id"), resp.Header.Get("cf-ray"), req.Header.Get("User-Agent"), req.Header.Get("Version"))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		// Redact before shortening the summary, so truncation cannot expose a token prefix.
		cleanBody := redactTicketDiagnostic(string(body), token, proxyURL, candidate)
		detail := ticketErrorSummary([]byte(cleanBody), resp.Header.Get("Content-Type"))
		return "", resp.StatusCode, "", &codexTicketHTTPError{status: resp.StatusCode, retryAfter: ticketRetryAfter(resp.Header.Get("Retry-After"), time.Now()), detail: fmt.Sprintf("phase=%s request_id=%q cf_ray=%q retry_after=%q detail=%q", phase, resp.Header.Get("x-request-id"), resp.Header.Get("cf-ray"), resp.Header.Get("Retry-After"), detail)}
	}
	state := strings.TrimSpace(resp.Header.Get(codexTurnStateHeader))
	responseModel := strings.TrimSpace(resp.Header.Get("openai-model"))
	if candidate != "" {
		defer resp.Body.Close()
		if state != "" && !auth.ValidCodexTurnStateTicketValue(state, cfg.TargetLength) {
			return state, resp.StatusCode, responseModel, errTicketRejected
		}
		streamModel, streamState, err := readTicketReplayStream(resp.Body)
		if err != nil {
			return state, resp.StatusCode, responseModel, fmt.Errorf("ticket replay stream: %w", err)
		}
		if streamModel != "" {
			responseModel = streamModel
		}
		if streamState != "" {
			state = streamState
		}
		log.Printf("[codex-turn-state-probe] account=%d phase=replay_completed model=%s returned_model=%s sid=%s returned_length=%d", account.ID(), model, responseModel, codexTurnStateProxySID(proxyURL), len(state))
	}
	// Rejecting a degraded 312 must not read the SSE stream. Closing the body
	// tears down the connection immediately, so the worker can try a new sid.
	_ = resp.Body.Close()
	return state, resp.StatusCode, responseModel, nil
}

// CodexTurnStateProbeDiagnostics returns a metadata-only snapshot for one account.
func CodexTurnStateProbeDiagnostics(accountID int64) map[string]struct {
	Queued      bool
	InFlight    bool
	LastAttempt time.Time
	LastSuccess time.Time
	NextAttempt time.Time
	LastError   string
} {
	result := make(map[string]struct {
		Queued      bool
		InFlight    bool
		LastAttempt time.Time
		LastSuccess time.Time
		NextAttempt time.Time
		LastError   string
	})
	if h := activeCodexTurnStateHarvester.Load(); h != nil {
		h.queueMu.Lock()
		defer h.queueMu.Unlock()
		for key, state := range h.states {
			if key.accountID == accountID {
				result[key.model] = struct {
					Queued      bool
					InFlight    bool
					LastAttempt time.Time
					LastSuccess time.Time
					NextAttempt time.Time
					LastError   string
				}{state.Queued, state.InFlight, state.LastAttempt, state.LastSuccess, state.NextAttempt, state.LastError}
			}
		}
	}
	return result
}

// Attach the failing operation to the original diagnostic.
type codexTurnStateStageError struct {
	stage string
	err   error
}

func (e *codexTurnStateStageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *codexTurnStateStageError) Unwrap() error { return e.err }

// Return the original diagnostic for this private deployment. It may contain
// credentials; operators must not share these messages without reviewing them.
func codexTurnStateProbeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Retry delay starts when the previous attempt finishes; concurrency queues and
// the periodic scan may defer actual execution beyond this earliest time.
func codexTurnStateRetryInterval(cfg *CodexTurnStateTicketConfig) time.Duration {
	if cfg == nil || cfg.ProbeIntervalSeconds <= 0 {
		return 6 * time.Second
	}
	return time.Duration(cfg.ProbeIntervalSeconds) * time.Second
}

// raceProbes fans out one account/model batch, sharing the per-account request budget.
// Only the winner is published by the caller, after all losing attempts exit.
func (h *CodexTurnStateHarvester) raceProbes(ctx context.Context, parallelism int, attempt func(context.Context) (string, error), accountIDs ...int64) (string, error) {
	accountID := int64(0)
	if len(accountIDs) > 0 {
		accountID = accountIDs[0]
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		state string
		err   error
	}
	count := min(64, max(1, parallelism))
	results := make(chan result, count)
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := h.acquireProbeSlot(batchCtx, accountID); err != nil {
				results <- result{err: err}
				return
			}
			defer h.releaseProbeSlot(accountID)
			if err := batchCtx.Err(); err != nil {
				results <- result{err: err}
				return
			}
			state, err := attempt(batchCtx)
			results <- result{state: state, err: err}
		}()
	}
	var winner string
	var lastErr error
	for i := 0; i < count; i++ {
		r := <-results
		if r.err == nil && r.state != "" && winner == "" {
			winner = r.state
			cancel()
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			lastErr = r.err
		}
	}
	workers.Wait()
	if winner != "" {
		return winner, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("probe batch returned no ticket")
	}
	return "", lastErr
}

func (h *CodexTurnStateHarvester) raceVerifiedProbes(ctx context.Context, parallelism int, attempt func(context.Context) (verifiedCodexTurnStateProbe, error), accountIDs ...int64) (verifiedCodexTurnStateProbe, error) {
	accountID := int64(0)
	if len(accountIDs) > 0 {
		accountID = accountIDs[0]
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		value verifiedCodexTurnStateProbe
		err   error
	}
	count := min(64, max(1, parallelism))
	results := make(chan result, count)
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := h.acquireProbeSlot(batchCtx, accountID); err != nil {
				results <- result{err: err}
				return
			}
			defer h.releaseProbeSlot(accountID)
			if err := batchCtx.Err(); err != nil {
				results <- result{err: err}
				return
			}
			value, err := attempt(batchCtx)
			results <- result{value: value, err: err}
		}()
	}
	var winner verifiedCodexTurnStateProbe
	var lastErr error
	var stopErr error
	for i := 0; i < count; i++ {
		r := <-results
		if r.err == nil && r.value.state != "" && winner.state == "" {
			winner = r.value
			cancel()
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			lastErr = r.err
			var candidateStop *codexTurnStateProbeStopError
			if stopErr == nil && errors.As(r.err, &candidateStop) {
				stopErr = candidateStop
				cancel()
			}
		}
	}
	workers.Wait()
	if winner.state != "" {
		return winner, nil
	}
	if stopErr != nil {
		return verifiedCodexTurnStateProbe{}, stopErr
	}
	if ctx.Err() != nil {
		return verifiedCodexTurnStateProbe{}, ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("probe batch returned no verified ticket")
	}
	return verifiedCodexTurnStateProbe{}, lastErr
}

func (h *CodexTurnStateHarvester) acquireProbeSlot(ctx context.Context, accountID int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg := CurrentCodexTurnStateTicketConfig()
		if !cfg.Enabled {
			return errors.New("ticket harvesting disabled")
		}
		h.slotMu.Lock()
		if h.accountActive == nil {
			h.accountActive = make(map[int64]int)
		}
		if h.accountActive[accountID] < min(64, max(1, cfg.Concurrency)) {
			h.accountActive[accountID]++
			h.active.Add(1)
			h.slotMu.Unlock()
			return nil
		}
		h.slotMu.Unlock()
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (h *CodexTurnStateHarvester) releaseProbeSlot(accountID int64) {
	h.slotMu.Lock()
	defer h.slotMu.Unlock()
	h.accountActive[accountID]--
	if h.accountActive[accountID] <= 0 {
		delete(h.accountActive, accountID)
	}
	h.active.Add(-1)
}

func isTransientCodexTurnStateProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "tls") || strings.Contains(text, "eof") || strings.Contains(text, "connection reset") || strings.Contains(text, "connection refused")
}
