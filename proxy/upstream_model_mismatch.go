package proxy

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 上游响应体回显的 model 与实际发给上游的 model 不一致时，给「账号 + 模型」打一个
// 纯观测标记（account_model_mismatches），管理端账号列表可见。标记不参与调度、
// 冷却或计费，只用来发现上游（多为中转站）悄悄换模型。

// upstreamModelMismatchPersistInterval 同一「账号 + 模型」两次落库的最小间隔；
// 间隔内的命中只在内存累加，避免持续换模型的上游让每个请求都多一次写库。
const upstreamModelMismatchPersistInterval = 30 * time.Second

// upstreamModelSnapshotSuffix 匹配官方快照后缀：-2025-08-07 / -20250929 / -0613 / @20250929。
var upstreamModelSnapshotSuffix = regexp.MustCompile(`^[-@_](\d{4}-\d{2}-\d{2}|\d{8}|\d{4})$`)

// upstreamModelFromPayload 从上游响应载荷里取回显的 model：Responses SSE 事件在
// response.model，非流式 body / chat chunk 在顶层 model，Anthropic message_start 在
// message.model。
func upstreamModelFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return upstreamModelFromResult(gjson.ParseBytes(payload))
}

func upstreamModelFromResult(root gjson.Result) string {
	for _, path := range []string{"response.model", "model", "message.model"} {
		if value := root.Get(path); value.Type == gjson.String {
			if model := strings.TrimSpace(value.String()); model != "" {
				return model
			}
		}
	}
	return ""
}

// contextGrokNativeUpstreamModel 暂存原生透传路径（forwardGrokNativeResponse*）本次
// attempt 观测到的上游 model，供紧随其后的用量日志读取。
const contextGrokNativeUpstreamModel = "codex2api.grok_native_upstream_model"

func grokNativeUpstreamModel(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return c.GetString(contextGrokNativeUpstreamModel)
}

// keepUpstreamModel 供流式解析逐事件调用：后到的非空值覆盖先到的（终态事件最权威）。
func keepUpstreamModel(current string, root gjson.Result) string {
	if model := upstreamModelFromResult(root); model != "" {
		return model
	}
	return current
}

func normalizeUpstreamModelForCompare(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	// 聚合网关常带 provider 前缀（openai/gpt-5、models/gemini-2.5-pro）。
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	// Claude Code 的上下文窗口标注（claude-sonnet-4-5[1m]）不是模型名的一部分。
	if idx := strings.Index(model, "["); idx > 0 {
		model = model[:idx]
	}
	model = strings.TrimSuffix(model, compactOpenAIModelSuffix)
	model = strings.TrimSuffix(model, "-latest")
	return model
}

// upstreamModelMatches 判断上游回显的 model 是否就是我们发过去的那个。别名解析成
// 带日期的官方快照（gpt-5 → gpt-5-2025-08-07）属于正常行为，不算不一致。
func upstreamModelMatches(sentModel, upstreamModel string) bool {
	sent := normalizeUpstreamModelForCompare(sentModel)
	upstream := normalizeUpstreamModelForCompare(upstreamModel)
	if sent == "" || upstream == "" || sent == upstream {
		return true
	}
	short, long := sent, upstream
	if len(short) > len(long) {
		short, long = long, short
	}
	return strings.HasPrefix(long, short) && upstreamModelSnapshotSuffix.MatchString(long[len(short):])
}

type upstreamModelMismatchKey struct {
	accountID int64
	model     string
}

type upstreamModelMismatchState struct {
	upstreamModel string
	persistedAt   time.Time
	pendingHits   int64
}

type upstreamModelMismatchTracker struct {
	mu     sync.Mutex
	states map[upstreamModelMismatchKey]*upstreamModelMismatchState
}

// note 累加一次命中；需要落库时返回应写入的命中数（含此前攒下的），否则返回 0。
func (t *upstreamModelMismatchTracker) note(accountID int64, model, upstreamModel string, now time.Time) int64 {
	key := upstreamModelMismatchKey{accountID: accountID, model: model}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.states == nil {
		t.states = make(map[upstreamModelMismatchKey]*upstreamModelMismatchState)
	}
	state := t.states[key]
	if state == nil {
		state = &upstreamModelMismatchState{}
		t.states[key] = state
	}
	state.pendingHits++
	if state.upstreamModel == upstreamModel && now.Sub(state.persistedAt) < upstreamModelMismatchPersistInterval {
		return 0
	}
	hits := state.pendingHits
	state.pendingHits = 0
	state.upstreamModel = upstreamModel
	state.persistedAt = now
	return hits
}

// noteUpstreamModelMismatch 在用量日志的公共出口比对模型。只看成功响应：失败响应的
// model 回显不可靠，也不代表上游真的用别的模型算了这次请求。
func (h *Handler) noteUpstreamModelMismatch(input *database.UsageLogInput) {
	if h == nil || h.db == nil || input == nil || input.AccountID <= 0 || input.StatusCode != 200 {
		return
	}
	upstreamModel := strings.TrimSpace(input.UpstreamModel)
	if upstreamModel == "" {
		return
	}
	// EffectiveModel 只在发生映射时非空；未映射时发给上游的就是请求模型本身。
	sentModel := strings.TrimSpace(input.EffectiveModel)
	if sentModel == "" {
		sentModel = strings.TrimSpace(input.Model)
	}
	if upstreamModelMatches(sentModel, upstreamModel) {
		return
	}
	modelKey := strings.ToLower(sentModel)
	now := time.Now()
	hits := h.upstreamModelMismatches.note(input.AccountID, modelKey, upstreamModel, now)
	if hits <= 0 {
		return
	}
	log.Printf("[账号 %d] 上游返回模型与请求不一致: 请求=%s 上游=%s", input.AccountID, sentModel, upstreamModel)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.db.RecordModelMismatch(ctx, input.AccountID, modelKey, upstreamModel, hits, now); err != nil {
		log.Printf("[账号 %d] 持久化模型不一致标记失败 model=%s: %v", input.AccountID, modelKey, err)
	}
}
