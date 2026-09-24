package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExcelRouteExplicitAccountBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	t.Setenv("EXCEL_ROUTES_FILE", path)
	if err := os.WriteFile(path, []byte(`{"42":{"credential_mode":"oauth"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Account{DBID: 42, Models: []string{"gpt-5.6-sol"}}
	if !a.SupportsCodexModel("gpt-5.6-sol-excel") || !a.SupportsCodexModel("gpt-5.6-sol") {
		t.Fatal("same account must admit both routes")
	}
	if (&Account{DBID: 43}).SupportsCodexModel("gpt-5.6-sol-excel") {
		t.Fatal("unconfigured account admitted Excel")
	}
	if err := os.WriteFile(path, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if a.SupportsCodexModel("gpt-5.6-sol-excel") {
		t.Fatal("invalid configuration must fail closed")
	}
}
