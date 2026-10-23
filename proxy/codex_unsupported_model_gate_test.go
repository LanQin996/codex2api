package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestClassifyCodexUnsupportedModel(t *testing.T) {
	resetCodexVersionGates()
	t.Cleanup(resetCodexVersionGates)
	now := time.Now()

	if got := classifyCodexUnsupportedModel("free", "gpt-6.1-sol", "0.153.3", now); got != codexUnsupportedPlanGate {
		t.Fatalf("free without Pro evidence = %v, want plan", got)
	}
	if got := classifyCodexUnsupportedModel("plus", "gpt-6.1-sol", "0.153.3", now); got != codexUnsupportedPlanGate {
		t.Fatalf("plus without Pro evidence = %v, want plan", got)
	}
	if got := classifyCodexUnsupportedModel("prolite", "gpt-6.1-sol", "0.153.3", now); got != codexUnsupportedVersionGate {
		t.Fatalf("Pro tier = %v, want version", got)
	}
	if got := classifyCodexUnsupportedModel("free", "GPT-6.1-sol", "0.153.3", now.Add(time.Minute)); got != codexUnsupportedVersionGate {
		t.Fatalf("free after Pro failed at the same version = %v, want version", got)
	}
	if got := classifyCodexUnsupportedModel("free", "gpt-6.1-sol", "0.160.0", now.Add(time.Minute)); got != codexUnsupportedPlanGate {
		t.Fatalf("free at another version = %v, want plan", got)
	}
	if got := classifyCodexUnsupportedModel("free", "gpt-6-sol", "0.153.3", now.Add(time.Minute)); got != codexUnsupportedPlanGate {
		t.Fatalf("free on another model = %v, want plan", got)
	}
	if got := classifyCodexUnsupportedModel("free", "gpt-6.1-sol", "0.153.3", now.Add(codexVersionGateMemory)); got != codexUnsupportedPlanGate {
		t.Fatalf("free after the memory window = %v, want plan", got)
	}
	if got := classifyCodexUnsupportedModel("pro", proOnlySparkModel, "0.153.3", now); got != codexUnsupportedPlanGate {
		t.Fatalf("spark on Pro = %v, want plan", got)
	}

	classifyCodexUnsupportedModel("pro", "gpt-6.1-sol", "", now)
	resetCodexVersionGates()
	if got := classifyCodexUnsupportedModel("free", "gpt-6.1-sol", "", now); got != codexUnsupportedPlanGate {
		t.Fatalf("free after reset = %v, want plan", got)
	}
}

func TestCodexOutboundClientVersion(t *testing.T) {
	if got := codexOutboundClientVersion(nil); got != "" {
		t.Fatalf("nil response version = %q", got)
	}
	if got := codexOutboundClientVersion(&http.Response{}); got != "" {
		t.Fatalf("response without request version = %q", got)
	}
	withHeader := &http.Response{Request: &http.Request{Header: http.Header{"Version": []string{"0.159.0"}}}}
	if got := codexOutboundClientVersion(withHeader); got != "0.159.0" {
		t.Fatalf("Version header = %q, want 0.159.0", got)
	}
	fromUA := &http.Response{Request: &http.Request{Header: http.Header{
		"User-Agent": []string{"codex-tui/0.158.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.158.0)"},
	}}}
	if got := codexOutboundClientVersion(fromUA); got != "0.158.0" {
		t.Fatalf("User-Agent fallback = %q, want 0.158.0", got)
	}
}

func TestUnsupportedModelVersionGateSkipsCooldown(t *testing.T) {
	resetCodexVersionGates()
	t.Cleanup(resetCodexVersionGates)
	body := []byte(`{"error":{"message":"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`)
	respAt := func(version string) *http.Response {
		return &http.Response{Header: make(http.Header), Request: &http.Request{Header: http.Header{"Version": []string{version}}}}
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	defer store.Stop()
	pro := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", Status: auth.StatusReady}
	freeSameVersion := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "free", Status: auth.StatusReady}
	freeOtherVersion := &auth.Account{DBID: 3, AccessToken: "at-3", PlanType: "free", Status: auth.StatusReady}
	for _, account := range []*auth.Account{pro, freeSameVersion, freeOtherVersion} {
		store.AddAccount(account)
	}
	handler := &Handler{store: store}

	decision := handler.applyCooldownForModel(pro, http.StatusBadRequest, body, respAt("0.153.3"), "gpt-6.1-sol")
	if decision.Reason != auth.ModelCooldownReasonUnsupported || !decision.ResetAt.IsZero() || pro.IsModelRateLimited("gpt-6.1-sol") {
		t.Fatalf("Pro version gate wrote a cooldown: %#v", decision)
	}
	decision = handler.applyCooldownForModel(freeSameVersion, http.StatusBadRequest, body, respAt("0.153.3"), "gpt-6.1-sol")
	if !decision.ResetAt.IsZero() || freeSameVersion.IsModelRateLimited("gpt-6.1-sol") {
		t.Fatalf("free account at the gated version wrote a cooldown: %#v", decision)
	}
	decision = handler.applyCooldownForModel(freeOtherVersion, http.StatusBadRequest, body, respAt("0.160.0"), "gpt-6.1-sol")
	if decision.ResetAt.IsZero() || !freeOtherVersion.IsModelRateLimited("gpt-6.1-sol") {
		t.Fatalf("plan-gated free account must keep the model cooldown: %#v", decision)
	}
}

func TestCodexClientVersionRaisedRunsHooksAndResetsGates(t *testing.T) {
	resetCodexVersionGates()
	t.Cleanup(resetCodexVersionGates)
	codexClientVersionRaisedHooks.mu.Lock()
	saved := codexClientVersionRaisedHooks.hooks
	codexClientVersionRaisedHooks.hooks = nil
	codexClientVersionRaisedHooks.mu.Unlock()
	t.Cleanup(func() {
		codexClientVersionRaisedHooks.mu.Lock()
		codexClientVersionRaisedHooks.hooks = saved
		codexClientVersionRaisedHooks.mu.Unlock()
	})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	defer store.Stop()
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "free", Status: auth.StatusReady}
	store.AddAccount(account)
	store.MarkModelUnsupported(account, "gpt-6.1-sol", 30*time.Minute)
	cleared := 0
	OnCodexClientVersionRaised(func() {
		cleared += store.ClearModelCooldownsByReason(auth.ModelCooldownReasonUnsupported, 0)
	})
	classifyCodexUnsupportedModel("pro", "gpt-6.1-sol", "0.153.3", time.Now())

	if (&CodexClientVersionsSyncResult{}).anyUpdated() {
		t.Fatal("empty sync result reported an update")
	}
	if !(&CodexClientVersionsSyncResult{VSCode: CodexAppBuildSyncResult{Updated: true}}).anyUpdated() {
		t.Fatal("updated VSCode pair was not reported")
	}
	notifyCodexClientVersionRaised()

	if cleared != 1 || account.IsModelRateLimited("gpt-6.1-sol") {
		t.Fatalf("cleared = %d, rate limited = %t; want the unsupported cooldown dropped", cleared, account.IsModelRateLimited("gpt-6.1-sol"))
	}
	if got := classifyCodexUnsupportedModel("free", "gpt-6.1-sol", "0.153.3", time.Now()); got != codexUnsupportedPlanGate {
		t.Fatalf("version gate survived the raise: %v", got)
	}
}

func TestGPT61SolSharesGPT6AuthMetadata(t *testing.T) {
	for _, id := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		if info := modelInfoForID(id, ModelSourceOfficialCodexDocs); info.APIKeyAuthAvailable {
			t.Fatalf("%s APIKeyAuthAvailable = true, want false like the rest of the GPT-6 family", id)
		}
	}
}
