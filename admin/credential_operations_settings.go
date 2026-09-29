package admin

import (
	"context"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"time"
)

func (h *Handler) GetCredentialOperationsSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	s, err := h.db.GetCredentialOperationsSettings(ctx)
	if err != nil {
		writeError(c, 500, "读取 2FA 后台设置失败")
		return
	}
	c.JSON(200, s)
}

func (h *Handler) UpdateCredentialOperationsSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var s database.CredentialOperationsSettings
	if c.ShouldBindJSON(&s) != nil || s.Concurrency < 1 || s.Concurrency > 8 {
		writeError(c, 400, "2FA 后台并发数必须为 1–8 的整数")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.db.SetCredentialOperationsSettings(ctx, s); err != nil {
		writeError(c, 500, "保存 2FA 后台设置失败")
		return
	}
	c.JSON(200, s)
}
