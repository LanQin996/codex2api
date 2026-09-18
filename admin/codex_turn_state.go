package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type codexTurnStateTicketStatus struct {
	Model            string    `json:"model"`
	State            string    `json:"state"`
	CapturedAt       time.Time `json:"captured_at,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	LastAttempt      time.Time `json:"last_attempt,omitempty"`
	LastSuccess      time.Time `json:"last_success,omitempty"`
	NextAttempt      time.Time `json:"next_attempt,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

// GetCodexTurnStateStatus returns metadata only; opaque ticket values never
// leave the runtime cache through this endpoint.
func (h *Handler) GetCodexTurnStateStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": proxy.CodexTurnStateRuntimeStatus()})
}

type codexTurnStateProbeRequest struct {
	AccountID int64  `json:"account_id"`
	Model     string `json:"model"`
}

// ProbeCodexTurnState queues an immediate probe for one account/model. When no
// account is supplied, all configured account/model pairs are scheduled.
func (h *Handler) ProbeCodexTurnState(c *gin.Context) {
	var req codexTurnStateProbeRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && !strings.Contains(strings.ToLower(err.Error()), "eof") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体无效"})
			return
		}
	}
	queued := proxy.TriggerCodexTurnStateProbes(req.AccountID, req.Model)
	if queued == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "自动采集器未启动或没有可探测账号"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"queued": queued})
}
