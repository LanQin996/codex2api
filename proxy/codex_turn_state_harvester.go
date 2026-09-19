package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
		return &CodexTurnStateTicketConfig{TargetLength: 292, TTLSeconds: 3600, RefreshBeforeSeconds: 600, ProbeIntervalSeconds: 6, AttemptTimeoutSeconds: 25, Concurrency: 8, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, ProbeModels: []string{"gpt-6-astra", "gpt-5.6-sol"}, PreserveExisting: true}
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
	store     *auth.Store
	db        *database.DB
	cancel    context.CancelFunc
	done      chan struct{}
	start     sync.Once
	stop      sync.Once
	persistMu sync.Mutex
	tasks     chan codexTurnStateProbeKey
	persist   chan int64
	queueMu   sync.Mutex
	queued    map[codexTurnStateProbeKey]bool
	inflight  map[codexTurnStateProbeKey]bool
	states    map[codexTurnStateProbeKey]*codexTurnStateProbeStatus
	active    atomic.Int64
	pendingMu sync.Mutex
	pending   map[uint64]*codexTurnStatePendingCapture
	nextID    atomic.Uint64
	probed    atomic.Uint64
	succeeded atomic.Uint64
	failed    atomic.Uint64
	collected atomic.Uint64
	injected  atomic.Uint64
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

type codexTurnStateProbeStatus struct {
	Queued      bool
	InFlight    bool
	Failures    int
	NextAttempt time.Time
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
}

type codexTurnStatePendingCapture struct {
	key       codexTurnStateProbeKey
	createdAt time.Time
	expiresAt time.Time
	candidate string
}

type codexTurnStatePendingContextKey struct{}

type codexTurnStateRuntimeStatus struct {
	Enabled         bool                          `json:"enabled"`
	Active          bool                          `json:"active"`
	PendingCaptures int                           `json:"pending_captures"`
	QueuedProbes    int                           `json:"queued_probes"`
	InFlightProbes  int                           `json:"in_flight_probes"`
	TotalProbed     uint64                        `json:"total_probed"`
	TotalSucceeded  uint64                        `json:"total_succeeded"`
	TotalFailed     uint64                        `json:"total_failed"`
	TotalCollected  uint64                        `json:"total_collected"`
	TotalInjected   uint64                        `json:"total_injected"`
	Truncated       bool                          `json:"truncated,omitempty"`
	Accounts        []codexTurnStateAccountStatus `json:"accounts,omitempty"`
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
		queued:   make(map[codexTurnStateProbeKey]bool),
		inflight: make(map[codexTurnStateProbeKey]bool),
		states:   make(map[codexTurnStateProbeKey]*codexTurnStateProbeStatus),
		pending:  make(map[uint64]*codexTurnStatePendingCapture),
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
	StageCodexTurnStateValue(ctx, values[0])
}

func StageCodexTurnStateValue(ctx context.Context, value string) {
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
	if !auth.ValidCodexTurnStateTicketValue(value, cfg.TargetLength) {
		return
	}
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	if pending := h.pending[id]; pending != nil && time.Now().Before(pending.expiresAt) {
		pending.candidate = value
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
		h.recordTicket(account, pending.key.model, pending.candidate, "response", true)
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
		interval := time.Duration(cfg.ProbeIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 6 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
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
	for _, account := range h.store.Accounts() {
		if !isNativeCodexOAuth(account) || !account.IsAvailable() {
			continue
		}
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
			h.enqueueProbe(codexTurnStateProbeKey{accountID: account.ID(), model: strings.ToLower(model)}, now)
		}
	}
}

func (h *CodexTurnStateHarvester) pruneAccountTickets(account *auth.Account, cfg *CodexTurnStateTicketConfig, now time.Time) {
	if account == nil {
		return
	}
	changed := false
	account.Mu().Lock()
	for model, ticket := range account.CodexTurnStateTickets {
		if !cfg.ModelManaged(model) || !ticket.Valid(now, cfg.TargetLength) {
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
	if !isNativeCodexOAuth(account) || !account.IsAvailable() || account.IsModelRateLimited(key.model) {
		return false
	}
	// Background maintenance must not spend credits to bypass exhausted usage windows.
	switch account.RuntimeStatus() {
	case "usage_exhausted", "rate_limited", "rate_limited_5h", "quota_paused":
		return false
	}
	return true
}

func (h *CodexTurnStateHarvester) enqueueProbe(key codexTurnStateProbeKey, now time.Time) {
	if key.accountID <= 0 || key.model == "" || !h.probeEligible(key) {
		return
	}
	h.queueMu.Lock()
	state := h.states[key]
	if state == nil {
		state = &codexTurnStateProbeStatus{}
		h.states[key] = state
	}
	if h.queued[key] || h.inflight[key] || (!state.NextAttempt.IsZero() && now.Before(state.NextAttempt)) {
		h.queueMu.Unlock()
		return
	}
	h.queued[key] = true
	state.Queued = true
	h.queueMu.Unlock()
	select {
	case h.tasks <- key:
	default:
		h.queueMu.Lock()
		delete(h.queued, key)
		state.Queued = false
		h.queueMu.Unlock()
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
	h.inflight[key] = true
	state.LastAttempt = time.Now()
	h.queueMu.Unlock()
	defer func() {
		h.queueMu.Lock()
		delete(h.inflight, key)
		state.InFlight = false
		h.queueMu.Unlock()
	}()

	for {
		cfg := CurrentCodexTurnStateTicketConfig()
		limit := max(1, cfg.Concurrency)
		if h.active.Add(1) <= int64(limit) {
			break
		}
		h.active.Add(-1)
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	defer h.active.Add(-1)
	account := h.store.FindByID(key.accountID)
	cfg := CurrentCodexTurnStateTicketConfig()
	if account == nil || !cfg.Enabled || !h.probeEligible(key) {
		return
	}
	h.probed.Add(1)
	err := h.probe(ctx, account, key.model, cfg)
	h.queueMu.Lock()
	if err == nil {
		state.Failures = 0
		state.NextAttempt = time.Time{}
		state.LastSuccess = time.Now()
		state.LastError = ""
	} else {
		state.Failures++
		backoff := time.Duration(1<<min(state.Failures, 6)) * time.Second
		if backoff < time.Duration(cfg.ProbeIntervalSeconds)*time.Second {
			backoff = time.Duration(cfg.ProbeIntervalSeconds) * time.Second
		}
		if backoff > time.Hour {
			backoff = time.Hour
		}
		jitter := time.Duration(rand.Int63n(int64(backoff/4) + 1))
		state.NextAttempt = time.Now().Add(backoff + jitter)
		state.LastError = codexTurnStateProbeError(err)
		log.Printf("[codex-turn-state] account=%d model=%s failures=%d next_attempt=%s error=%s", key.accountID, key.model, state.Failures, state.NextAttempt.UTC().Format(time.RFC3339), state.LastError)
	}
	h.queueMu.Unlock()
	if err == nil {
		h.succeeded.Add(1)
	} else {
		h.failed.Add(1)
	}
}

func (h *CodexTurnStateHarvester) ticketNeedsRefresh(account *auth.Account, model string, now time.Time, cfg *CodexTurnStateTicketConfig) bool {
	account.Mu().RLock()
	ticket, ok := account.CodexTurnStateTickets[strings.ToLower(strings.TrimSpace(model))]
	account.Mu().RUnlock()
	return !ok || ticket.ExpiresAt.Before(now.Add(time.Duration(cfg.RefreshBeforeSeconds)*time.Second))
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
	state, status, err := h.fireProbe(ctx, account, token, model, cfg)
	if err != nil || status != http.StatusOK || !auth.ValidCodexTurnStateTicketValue(state, cfg.TargetLength) {
		if err != nil {
			return err
		}
		return fmt.Errorf("probe returned status=%d length=%d", status, len(state))
	}
	h.recordTicket(account, model, state, "probe", true)
	return nil
}

func (h *CodexTurnStateHarvester) recordTicket(account *auth.Account, model, state, source string, enqueuePersistence bool) {
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
	ticket := auth.CodexTurnStateTicket{State: state, Length: len(state), CapturedAt: now, ExpiresAt: now.Add(time.Duration(cfg.TTLSeconds) * time.Second), Source: source}
	account.Mu().Lock()
	if account.CodexTurnStateTickets == nil {
		account.CodexTurnStateTickets = make(map[string]auth.CodexTurnStateTicket)
	}
	// An echoed opaque value is not a newly minted ticket. Extending its TTL
	// here indefinitely postpones proactive refresh on busy accounts.
	if existing, ok := account.CodexTurnStateTickets[model]; ok && existing.State == state {
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
					item.LastAttempt, item.LastSuccess, item.LastError = probeState.LastAttempt, probeState.LastSuccess, probeState.LastError
					if probeState.InFlight {
						item.State = "refreshing"
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
	return strings.TrimSpace(resp.Header.Get(codexTurnStateHeader)), resp.StatusCode, nil
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

// Do not expose transport URLs, proxy credentials or token-refresh response bodies.
type codexTurnStateStageError struct {
	stage string
	err   error
}

func (e *codexTurnStateStageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *codexTurnStateStageError) Unwrap() error { return e.err }

func codexTurnStateProbeError(err error) string {
	stage := "probe"
	var staged *codexTurnStateStageError
	if errors.As(err, &staged) {
		stage = staged.stage
	}
	reason := "unclassified failure (raw message withheld)"
	message := strings.ToLower(err.Error())
	var dns *net.DNSError
	var op *net.OpError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case errors.As(err, &dns):
		reason = "DNS lookup failed"
	case errors.As(err, &netErr) && netErr.Timeout():
		reason = "network timeout"
	case strings.Contains(message, "407") || strings.Contains(message, "authentication failed") || strings.Contains(message, "username/password authentication"):
		reason = "proxy authentication failed; check protocol, username and password"
	case strings.Contains(message, "connection refused"):
		reason = "connection refused; check proxy host and port"
	case strings.Contains(message, "socks"):
		reason = "SOCKS handshake/connect failed; check proxy protocol and credentials"
	case strings.Contains(message, "tls") || strings.Contains(message, "certificate"):
		reason = "TLS handshake/certificate failed"
	case strings.Contains(message, "eof"):
		reason = "connection closed by proxy/upstream (EOF)"
	case strings.Contains(message, "invalid_grant"):
		reason = "refresh token rejected (invalid_grant)"
	case errors.As(err, &op):
		reason = "network operation failed: " + op.Op
	case strings.HasPrefix(message, "probe returned status=") || message == "missing access token":
		reason = err.Error()
	}
	return stage + ": " + reason
}
