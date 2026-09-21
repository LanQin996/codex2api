package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCodexTicketReplayAcceptsRotationButRejectsDegradation(t *testing.T) {
	candidate := convergenceTurnStateValue(1, 10)
	replacement := convergenceTurnStateValue(2, 10)
	for _, tc := range []struct {
		name, returned, model string
		status                int
		want                  string
	}{
		{"rotated", replacement, "gpt-6-astra", 200, replacement},
		{"echo", candidate, "", 200, candidate},
		{"accepted without replacement", "", "", 200, candidate},
		{"degraded", testTurnStateValue(11), "", 200, ""},
		{"malformed", strings.Repeat("a", 292), "", 200, ""},
		{"wrong model", replacement, "other-model", 200, ""},
		{"denied", replacement, "", 403, ""},
		{"rate limited", "", "", 429, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := acceptedCodexTicket(candidate, tc.returned, tc.status, "gpt-6-astra", tc.model, 292)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("accepted=%t length=%d", ok, len(got))
			}
		})
	}
}

func TestCodexTicketReplayHeaderOverridesOldAccountTicket(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "stale-account-ticket")
	applyTicketReplayHeader(headers, "")
	if headers.Get(codexTurnStateHeader) != "" {
		t.Fatal("acquisition leaked old ticket")
	}
	applyTicketReplayHeader(headers, "candidate")
	if headers.Get(codexTurnStateHeader) != "candidate" {
		t.Fatal("candidate not sent on replay")
	}
}

func TestCodexTicketHTMLReasonAfterLargeLogo(t *testing.T) {
	page := "<html><head><style>hidden-css</style></head><body><svg><path d='" + strings.Repeat("1 ", 5000) + "'/></svg><h1>Access denied</h1><p>Request blocked &amp; unavailable.</p><script>secret-script</script></body></html>"
	got := ticketErrorSummary([]byte(page), "text/html")
	if got != "Access denied Request blocked & unavailable." {
		t.Fatalf("summary=%q", got)
	}
}

func TestCodexTicketDiagnosticRedactsCredentials(t *testing.T) {
	proxy := "http://proxy-user:proxy-secret@proxy.example:3010"
	got := redactTicketDiagnostic(proxy+" proxy-user proxy-secret access-secret ticket-secret", "access-secret", proxy, "ticket-secret")
	for _, secret := range []string{"proxy-user", "proxy-secret", "access-secret", "ticket-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
}

func TestCodexTicketRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 120 * time.Second}, {now.Add(3 * time.Minute).Format(http.TimeFormat), 3 * time.Minute}, {"invalid", 0}, {"-1", 0},
	} {
		if got := ticketRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.value, got, tc.want)
		}
	}
}
