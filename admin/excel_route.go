package admin

import (
	"net/url"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func excelBridgeConfigured() bool {
	u, err := url.Parse(os.Getenv("EXCEL_BRIDGE_URL"))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") &&
		len(os.Getenv("EXCEL_BRIDGE_API_KEY")) >= 32
}

func (h *Handler) excelRouteModeForRow(row *database.AccountRow) string {
	mode := row.GetCredential(auth.ExcelRouteCredentialKey)
	if auth.ValidExcelRouteMode(mode) {
		return mode
	}
	if h.store != nil {
		if account := h.store.FindByID(row.ID); account != nil {
			if route, enabled := account.ExcelRoute(); enabled {
				return route.CredentialMode
			}
		}
	}
	return "off"
}

func excelEligibleRow(row *database.AccountRow) bool {
	upstream := strings.TrimSpace(row.GetCredential("upstream_type"))
	return (upstream == "" || upstream == "codex") &&
		!strings.EqualFold(row.GetCredential("auth_mode"), "agentIdentity")
}
