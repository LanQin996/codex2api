package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"golang.org/x/net/html"
)

type codexTicketHTTPError struct {
	status     int
	retryAfter time.Duration
	detail     string
}

func (e *codexTicketHTTPError) Error() string {
	return fmt.Sprintf("upstream probe status=%d %s", e.status, e.detail)
}

func ticketRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 && seconds <= 86400 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return min(date.Sub(now), 24*time.Hour)
	}
	return 0
}

func applyTicketReplayHeader(headers http.Header, candidate string) {
	// Do not inherit an account's old ticket while acquiring a new candidate.
	headers.Del(codexTurnStateHeader)
	if candidate != "" {
		headers.Set(codexTurnStateHeader, candidate)
	}
}

func acceptedCodexTicket(candidate, replacement string, status int, model, returnedModel string, length int) (string, bool) {
	if status != http.StatusOK || !responseModelMatches(model, returnedModel) || !auth.ValidCodexTurnStateTicketValue(candidate, length) {
		return "", false
	}
	if replacement == "" {
		return candidate, true
	}
	if !auth.ValidCodexTurnStateTicketValue(replacement, length) {
		return "", false
	}
	return replacement, true
}

// Read beyond a large SVG logo, but retain only bounded visible text.
func ticketErrorSummary(body []byte, contentType string) string {
	text := string(body)
	if strings.Contains(strings.ToLower(contentType), "html") || strings.HasPrefix(strings.TrimSpace(text), "<") {
		z := html.NewTokenizer(bytes.NewReader(body))
		var out strings.Builder
		skip := 0
		for {
			kind := z.Next()
			if kind == html.ErrorToken {
				if z.Err() != io.EOF && out.Len() == 0 {
					return "HTML error page (unreadable)"
				}
				break
			}
			token := z.Token()
			hidden := token.Data == "svg" || token.Data == "script" || token.Data == "style"
			if kind == html.StartTagToken && hidden {
				skip++
			}
			if kind == html.EndTagToken && hidden && skip > 0 {
				skip--
			}
			if kind == html.TextToken && skip == 0 {
				out.WriteString(token.Data)
				out.WriteByte(' ')
			}
		}
		text = out.String()
		if strings.TrimSpace(text) == "" {
			text = "HTML rejection page (no visible text within 64 KiB)"
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 2048 {
		text = string(runes[:2048]) + "…"
	}
	return text
}

func redactTicketDiagnostic(text, token, proxyURL, candidate string) string {
	secrets := []string{token, proxyURL, candidate}
	if u, err := url.Parse(proxyURL); err == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, u.User.String(), u.User.Username(), password)
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}
