package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newQualityBatchID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type qualityBatchRequest struct {
	AccountIDs []int64 `json:"account_ids"`
	Channel    string  `json:"channel"`
	RequestID  string  `json:"request_id"`
	qualityTestRequest
}

func (h *Handler) prepareQualityJob(ctx context.Context, id int64, req qualityTestRequest) (database.QualityTestJob, error) {
	job := database.QualityTestJob{AccountID: id, Model: req.Model, ReasoningEffort: req.ReasoningEffort, Prompt: req.Prompt}
	account := h.store.FindByID(id)
	if account == nil {
		return job, errors.New("账号不在运行时池中")
	}
	if err := h.validateQualityTestForAccount(ctx, account, req); err != nil {
		return job, err
	}
	job.Channel = "codex"
	switch {
	case account.IsClaudeOAuth():
		job.Channel = "claude"
	case account.IsGrokAPI():
		job.Channel = "grok"
	case account.IsAntigravityAPI():
		job.Channel = "antigravity"
	}
	account.Mu().RLock()
	job.AccountName, job.PlanType = account.Email, account.PlanType
	account.Mu().RUnlock()
	if job.AccountName == "" {
		job.AccountName = fmt.Sprintf("ID %d", id)
	}
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		return job, err
	}
	if row != nil && row.Name != "" {
		job.AccountName = row.Name
	}
	job.PresetKind, job.PresetRef, job.PresetName = h.resolveQualityTestPreset(ctx, req)
	return job, nil
}
func readQualityBatchRequest(c *gin.Context) (qualityBatchRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req qualityBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.AccountIDs) == 0 || len(req.AccountIDs) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择 1–1000 个账号"})
		return req, false
	}
	seen := map[int64]bool{}
	ids := []int64{}
	for _, id := range req.AccountIDs {
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
			return req, false
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	req.AccountIDs = ids
	return req, true
}
func (h *Handler) QualityTestBatchOptions(c *gin.Context) {
	req, ok := readQualityBatchRequest(c)
	if !ok {
		return
	}
	var result qualityTestOptions
	first := true
	for _, id := range req.AccountIDs {
		account := h.store.FindByID(id)
		if account == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("账号 #%d 不可用", id)})
			return
		}
		opts := h.qualityTestOptionsForAccount(c.Request.Context(), account)
		if first {
			result = opts
			first = false
		} else {
			result.Models = qualityStringIntersection(result.Models, opts.Models)
			result.ReasoningEfforts = qualityStringIntersection(result.ReasoningEfforts, opts.ReasoningEfforts)
		}
	}
	c.JSON(http.StatusOK, result)
}
func qualityStringIntersection(a, b []string) []string {
	result := []string{}
	for _, v := range a {
		for _, other := range b {
			if v == other {
				result = append(result, v)
				break
			}
		}
	}
	return result
}
func (h *Handler) CreateQualityTestBatch(c *gin.Context) {
	if h.db == nil || h.qualityTestContext == nil || h.qualityTestContext.Err() != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "检测任务服务不可用"})
		return
	}
	req, ok := readQualityBatchRequest(c)
	if !ok {
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.ReasoningEffort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if len(req.RequestID) < 16 || len(req.RequestID) > 100 || req.Model == "" || len(req.Model) > 200 || strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > qualityTestPromptLimit || req.Channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的批量检测配置"})
		return
	}
	encoded, _ := json.Marshal(req)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	// Replaying a committed batch never depends on current account/preset state.
	if existing, err := h.db.GetQualityTestBatch(c.Request.Context(), req.RequestID); err == nil {
		replay, err := h.db.EnqueueQualityTestBatch(c.Request.Context(), req.RequestID, fingerprint, nil, nil)
		if err != nil {
			qualityBatchError(c, err)
			return
		}
		replay.Counts = existing.Counts
		c.JSON(http.StatusAccepted, replay)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		qualityBatchError(c, err)
		return
	}
	jobs := []database.QualityTestJob{}
	rejected := []database.QualityTestRejection{}
	for _, id := range req.AccountIDs {
		job, err := h.prepareQualityJob(c.Request.Context(), id, req.qualityTestRequest)
		if err == nil && job.Channel != req.Channel {
			err = errors.New("账号渠道与本批配置不一致")
		}
		if err != nil {
			rejected = append(rejected, database.QualityTestRejection{AccountID: id, Error: err.Error()})
			continue
		}
		jobs = append(jobs, job)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := h.db.EnqueueQualityTestBatch(ctx, req.RequestID, fingerprint, jobs, rejected)
	if err != nil {
		qualityBatchError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, result)
}
func qualityBatchError(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	message := "检测批次操作失败"
	if errors.Is(err, database.ErrQualityTestIdempotency) {
		code = http.StatusConflict
		message = err.Error()
	}
	if errors.Is(err, sql.ErrNoRows) {
		code = http.StatusNotFound
		message = "检测批次不存在"
	}
	c.JSON(code, gin.H{"error": message})
}
func (h *Handler) GetQualityTestBatch(c *gin.Context) {
	result, err := h.db.GetQualityTestBatch(c.Request.Context(), c.Param("id"))
	if err != nil {
		qualityBatchError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *Handler) CancelQualityTestBatch(c *gin.Context) {
	if _, err := h.db.GetQualityTestBatch(c.Request.Context(), c.Param("id")); err != nil {
		qualityBatchError(c, err)
		return
	}
	if err := h.db.CancelQualityTestBatch(c.Request.Context(), c.Param("id")); err != nil {
		qualityBatchError(c, err)
		return
	}
	h.GetQualityTestBatch(c)
}
func (h *Handler) RetryQualityTest(c *gin.Context) {
	if h.qualityTestContext == nil || h.qualityTestContext.Err() != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "检测任务服务不可用"})
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的检测记录 ID"})
		return
	}
	old, err := h.db.GetQualityTestJob(c.Request.Context(), id)
	if err != nil {
		qualityBatchError(c, err)
		return
	}
	job, err := h.prepareQualityJob(c.Request.Context(), old.AccountID, qualityTestRequest{Model: old.Model, ReasoningEffort: old.ReasoningEffort, Prompt: old.Prompt})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	job.PresetKind, job.PresetRef, job.PresetName = old.PresetKind, old.PresetRef, old.PresetName
	result, err := h.db.EnqueueQualityTestBatch(c.Request.Context(), newQualityBatchID(), "retry", []database.QualityTestJob{job}, nil)
	if err != nil {
		qualityBatchError(c, err)
		return
	}
	if len(result.JobIDs) == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": database.ErrQualityTestAccountBusy.Error()})
		return
	}
	stored, err := h.db.GetQualityTestJob(c.Request.Context(), result.JobIDs[0])
	if err != nil {
		qualityBatchError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job": stored})
}
