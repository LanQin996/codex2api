package admin

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// attachModelMismatches 把「上游回显模型与实际请求模型不一致」标记挂到账号响应上。
// 标记直接读库（多实例下任一实例记录的都能看到）；分页视图只查当前页账号，
// 旧全量视图整表读取（该表只在上游真的换模型时才有行，通常很小）。
func (h *Handler) attachModelMismatches(ctx context.Context, accounts []accountResponse, byIDs bool) {
	if h == nil || h.db == nil || len(accounts) == 0 {
		return
	}
	var rows []*database.AccountModelMismatchRow
	var err error
	if byIDs {
		ids := make([]int64, 0, len(accounts))
		for i := range accounts {
			ids = append(ids, accounts[i].ID)
		}
		rows, err = h.db.ListModelMismatchesForAccounts(ctx, ids)
	} else {
		rows, err = h.db.ListModelMismatches(ctx)
	}
	if err != nil {
		log.Printf("获取账号模型不一致标记失败: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	byAccount := make(map[int64][]modelMismatchResponse)
	for _, row := range rows {
		item := modelMismatchResponse{
			Model:         row.Model,
			UpstreamModel: row.UpstreamModel,
			HitCount:      row.HitCount,
		}
		if !row.FirstSeenAt.IsZero() {
			item.FirstSeenAt = row.FirstSeenAt.Format(time.RFC3339)
		}
		if !row.LastSeenAt.IsZero() {
			item.LastSeenAt = row.LastSeenAt.Format(time.RFC3339)
		}
		byAccount[row.AccountID] = append(byAccount[row.AccountID], item)
	}
	for i := range accounts {
		accounts[i].ModelMismatches = byAccount[accounts[i].ID]
	}
}

// ClearAccountModelMismatches 清除某账号的全部模型不一致标记。
// DELETE /api/admin/accounts/:id/model-mismatches
func (h *Handler) ClearAccountModelMismatches(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cleared, err := h.db.ClearModelMismatches(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "账号模型不一致标记已清除",
		"cleared": cleared,
	})
}
