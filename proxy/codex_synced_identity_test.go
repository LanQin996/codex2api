package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestCodexCLIIdentityPoolUsesSyncedVersionInPreserveMode(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.ClientCompatMode = ClientCompatModePreserve
	s.CodexSyncedCLIVersion = "0.155.1"
	s.CodexUserAgentConfig = `{"mode":"pool","pool_mix":{"codex-tui":100}}`
	ApplyRuntimeSettings(s)
	for _, id := range []int64{19, 20, 21, 35, 36} {
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: &auth.Account{DBID: id}, Headers: http.Header{}})
		if err != nil {
			t.Fatal(err)
		}
		ua, v := identity.UserAgent, identity.Version
		if !codexVersionAtLeast(v, "0.155.1") || !strings.Contains(ua, "/"+v) {
			t.Fatalf("account=%d version=%s ua=%s", id, v, ua)
		}
	}
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.150.0")
	headers.Set("Version", "0.150.0")
	headers.Set("Originator", "codex_cli_rs")
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	ua, v := identity.UserAgent, identity.Version
	if ua != "codex_cli_rs/0.150.0" || v != "0.150.0" {
		t.Fatal("preserve mode rewrote an actual client")
	}
}
