package auth

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// Excel routes attach to existing account IDs, never create scheduler identities.
// The file contains no tokens; credentials are read from the selected account
// immediately before dispatch, so ordinary OAuth refresh remains authoritative.
type ExcelRoute struct {
	CredentialMode string `json:"credential_mode"`
}

func IsExcelModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.6-luna-excel", "gpt-5.6-terra-excel", "gpt-5.6-sol-excel":
		return true
	}
	return false
}

func (a *Account) ExcelRoute() (ExcelRoute, bool) {
	if a == nil || a.IsRelayStyle() || a.IsCodexAgentIdentity() {
		return ExcelRoute{}, false
	}
	path := strings.TrimSpace(os.Getenv("EXCEL_ROUTES_FILE"))
	if path == "" {
		return ExcelRoute{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1024*1024 {
		return ExcelRoute{}, false
	}
	var routes map[string]ExcelRoute
	if json.Unmarshal(data, &routes) != nil {
		return ExcelRoute{}, false
	}
	route, ok := routes[strconv.FormatInt(a.ID(), 10)]
	if !ok || (route.CredentialMode != "oauth" && route.CredentialMode != "session_file") {
		return ExcelRoute{}, false
	}
	return route, true
}
