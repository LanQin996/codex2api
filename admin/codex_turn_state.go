package admin

import (
	"fmt"
	"github.com/codex2api/security"
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

func normalizeCodexTurnStateProxyUpdate(raw, existing string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if raw == "***" {
		return existing, nil
	}
	parsed, err := security.ParseProxyURL(raw)
	if err != nil {
		return "", fmt.Errorf("采集代理格式无效：请填写 http:// 或 socks5:// 开头的完整地址，账号密码与地址之间使用 @，不要使用转义反斜杠")
	}
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		if password == "***" {
			old, oldErr := security.ParseProxyURL(existing)
			if oldErr == nil && old.User != nil && old.Scheme == parsed.Scheme && old.Host == parsed.Host && old.User.Username() == parsed.User.Username() {
				return existing, nil
			}
			return "", fmt.Errorf("修改代理地址或用户名后，请重新填写真实密码；不能使用脱敏占位符 ***")
		}
	}
	return parsed.String(), nil
}
