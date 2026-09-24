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

const ExcelRouteCredentialKey = "excel_route_mode"

func ValidExcelRouteMode(mode string) bool {
	return mode == "off" || mode == "oauth" || mode == "session_file"
}

func (a *Account) ExcelRouteMode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.excelRouteMode
}

func (s *Store) ApplyAccountExcelRouteMode(id int64, mode string) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.excelRouteMode = mode
		a.mu.Unlock()
	}
}

func IsExcelModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-6-astra-excel", "gpt-6-sol-excel", "gpt-5.6-luna-excel", "gpt-5.6-terra-excel", "gpt-5.6-sol-excel":
		return true
	}
	return false
}

func (a *Account) ExcelRoute() (ExcelRoute, bool) {
	if a == nil || a.IsRelayStyle() || a.IsCodexAgentIdentity() {
		return ExcelRoute{}, false
	}
	if mode := a.ExcelRouteMode(); mode != "" {
		return ExcelRoute{CredentialMode: mode}, mode == "oauth" || mode == "session_file"
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
