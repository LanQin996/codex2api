package auth

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CodexRouteCookiesCredentialKey stores model-scoped infrastructure cookies.
// Only __oailb and __cflb are accepted; login/session cookies are never stored.
const CodexRouteCookiesCredentialKey = "codex_route_cookies"

const (
	maxCodexRouteCookieValueLen = 4096
	maxCodexRouteCookies        = 16
	maxCodexRouteCookieModels   = 64
)

type CodexRouteCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  int64  `json:"expires,omitempty"`
	HostOnly bool   `json:"host_only"`
	Secure   bool   `json:"secure"`
}

type routeCookieJar struct {
	mu      sync.Mutex
	byModel map[string][]CodexRouteCookie
}

var codexRouteCookieJars sync.Map // map[int64]*routeCookieJar

func ResetCodexRouteCookiesForTest(accountID int64) {
	if accountID > 0 {
		codexRouteCookieJars.Delete(accountID)
	}
}

func NormalizeCodexRouteCookieScope(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "wss" {
		scheme = "https"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if scheme != "https" || !isCodexRouteCookieHost(host) {
		return "", false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return "https://" + host + path, true
}

func (a *Account) routeCookieJar() *routeCookieJar {
	if a == nil {
		return nil
	}
	if a.DBID > 0 {
		if v, ok := codexRouteCookieJars.Load(a.DBID); ok {
			return v.(*routeCookieJar)
		}
		seeded := &routeCookieJar{byModel: cloneRouteCookieMap(a.codexRouteCookies)}
		actual, _ := codexRouteCookieJars.LoadOrStore(a.DBID, seeded)
		return actual.(*routeCookieJar)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.localRouteCookies == nil {
		a.localRouteCookies = &routeCookieJar{byModel: cloneRouteCookieMap(a.codexRouteCookies)}
	}
	return a.localRouteCookies
}

func (a *Account) ObserveCodexRouteSetCookies(model, scopeURL string, lines []string, now time.Time) bool {
	model = strings.TrimSpace(model)
	u, ok := parseCodexRouteCookieScope(scopeURL)
	if a == nil || model == "" || !ok || len(lines) == 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if jar.byModel == nil {
		jar.byModel = map[string][]CodexRouteCookie{}
	}
	before := routeCookieIdentity(jar.byModel[model])
	cookies := dropExpiredRouteCookies(jar.byModel[model], now)
	host := strings.ToLower(u.Hostname())
	for _, line := range lines {
		cookies = absorbRouteCookie(cookies, host, u.Path, line, now)
	}
	if len(cookies) > maxCodexRouteCookies {
		cookies = append([]CodexRouteCookie(nil), cookies[len(cookies)-maxCodexRouteCookies:]...)
	}
	if len(cookies) == 0 {
		delete(jar.byModel, model)
		return before != ""
	}
	if _, exists := jar.byModel[model]; !exists && len(jar.byModel) >= maxCodexRouteCookieModels {
		return false
	}
	jar.byModel[model] = cookies
	return routeCookieIdentity(cookies) != before
}

// ReplaceCodexRouteCookies replaces the whole model pair. It is used when a
// newly minted ticket has been validated; absent cookies deliberately clear the
// old pair so the ticket and route are never mixed across rotations.
func (a *Account) ReplaceCodexRouteCookies(model, scopeURL string, lines []string, now time.Time) bool {
	model = strings.TrimSpace(model)
	u, ok := parseCodexRouteCookieScope(scopeURL)
	if a == nil || model == "" || !ok {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	before := routeCookieIdentity(jar.byModel[model])
	cookies := make([]CodexRouteCookie, 0, 2)
	host := strings.ToLower(u.Hostname())
	for _, line := range lines {
		cookies = absorbRouteCookie(cookies, host, u.Path, line, now)
	}
	if len(cookies) > maxCodexRouteCookies {
		cookies = cookies[len(cookies)-maxCodexRouteCookies:]
	}
	if len(cookies) == 0 {
		delete(jar.byModel, model)
		return before != ""
	}
	if jar.byModel == nil {
		jar.byModel = map[string][]CodexRouteCookie{}
	}
	if _, exists := jar.byModel[model]; !exists && len(jar.byModel) >= maxCodexRouteCookieModels {
		return false
	}
	jar.byModel[model] = append([]CodexRouteCookie(nil), cookies...)
	return routeCookieIdentity(cookies) != before
}

func (a *Account) ClearCodexRouteCookies(model string) bool {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return false
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if len(jar.byModel[model]) == 0 {
		return false
	}
	delete(jar.byModel, model)
	return true
}

func (a *Account) ClearAllCodexRouteCookies() {
	if a == nil {
		return
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	jar.byModel = map[string][]CodexRouteCookie{}
	jar.mu.Unlock()
}

func (a *Account) CodexRouteCookieHeader(model, scopeURL string, now time.Time) string {
	model = strings.TrimSpace(model)
	u, ok := parseCodexRouteCookieScope(scopeURL)
	if a == nil || model == "" || !ok {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	host, path := strings.ToLower(u.Hostname()), u.Path
	if path == "" {
		path = "/"
	}
	matched := make([]CodexRouteCookie, 0, 2)
	for _, cookie := range jar.byModel[model] {
		if routeCookieExpired(cookie, now) || !cookie.Secure || !isCodexRouteCookieName(cookie.Name) {
			continue
		}
		if !routeCookieDomainMatches(host, cookie.Domain, cookie.HostOnly) || !routeCookiePathMatches(cookie.Path, path) {
			continue
		}
		matched = append(matched, cookie)
	}
	sort.Slice(matched, func(i, j int) bool {
		if len(matched[i].Path) != len(matched[j].Path) {
			return len(matched[i].Path) > len(matched[j].Path)
		}
		return matched[i].Name < matched[j].Name
	})
	parts := make([]string, 0, len(matched))
	for _, cookie := range matched {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

func (a *Account) SnapshotCodexRouteCookies() map[string][]CodexRouteCookie {
	out := map[string][]CodexRouteCookie{}
	if a == nil {
		return out
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	now := time.Now()
	for model, cookies := range jar.byModel {
		live := dropExpiredRouteCookies(cookies, now)
		if len(live) > 0 {
			out[model] = append([]CodexRouteCookie(nil), live...)
		}
	}
	return out
}

func RouteCookiesFromCredential(raw any) map[string][]CodexRouteCookie {
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string][]CodexRouteCookie, len(fields))
	for model, rawList := range fields {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		items, ok := rawList.([]any)
		if !ok {
			continue
		}
		for _, rawItem := range items {
			m, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			cookie := CodexRouteCookie{
				Name: stringValue(m["name"]), Value: stringValue(m["value"]),
				Domain: strings.ToLower(stringValue(m["domain"])), Path: stringValue(m["path"]),
				Expires: int64Value(m["expires"]), HostOnly: boolValue(m["host_only"]), Secure: boolValue(m["secure"]),
			}
			if validStoredRouteCookie(cookie) {
				out[model] = append(out[model], cookie)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *Account) adoptStoredRouteCookies(stored map[string][]CodexRouteCookie) {
	if a == nil {
		return
	}
	a.codexRouteCookies = cloneRouteCookieMap(stored)
	if a.DBID > 0 {
		codexRouteCookieJars.LoadOrStore(a.DBID, &routeCookieJar{byModel: cloneRouteCookieMap(stored)})
	}
}

func parseCodexRouteCookieScope(scope string) (*url.URL, bool) {
	normalized, ok := NormalizeCodexRouteCookieScope(scope)
	if !ok {
		return nil, false
	}
	u, err := url.Parse(normalized)
	return u, err == nil && u != nil
}

func isCodexRouteCookieHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host == "chatgpt.com" || host == "chat.openai.com" || host == "chatgpt-staging.com" || strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
}

func isCodexRouteCookieName(name string) bool { return name == "__oailb" || name == "__cflb" }

func validRouteCookieValue(value string) bool {
	if value == "" || len(value) > maxCodexRouteCookieValueLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] == ';' || value[i] >= 0x7f {
			return false
		}
	}
	return true
}

func validStoredRouteCookie(c CodexRouteCookie) bool {
	return isCodexRouteCookieName(c.Name) && c.Secure && validRouteCookieValue(c.Value) && isCodexRouteCookieHost(c.Domain) && strings.HasPrefix(c.Path, "/")
}

func absorbRouteCookie(cookies []CodexRouteCookie, host, requestPath, line string, now time.Time) []CodexRouteCookie {
	parsed, err := http.ParseSetCookie(line)
	if err != nil || parsed == nil || !isCodexRouteCookieName(parsed.Name) || !parsed.Secure {
		return cookies
	}
	path := parsed.Path
	if path == "" {
		path = defaultRouteCookiePath(requestPath)
	}
	hostOnly := strings.TrimSpace(parsed.Domain) == ""
	domain := host
	if !hostOnly {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(parsed.Domain)), ".")
		if !routeCookieDomainAccepted(host, domain) {
			return cookies
		}
	}
	if !strings.HasPrefix(path, "/") || !isCodexRouteCookieHost(domain) {
		return cookies
	}
	if parsed.MaxAge < 0 || (parsed.MaxAge == 0 && !parsed.Expires.IsZero() && !parsed.Expires.After(now)) {
		return removeRouteCookie(cookies, parsed.Name, domain, path, hostOnly)
	}
	if !validRouteCookieValue(parsed.Value) {
		return cookies
	}
	expires := int64(0)
	if parsed.MaxAge > 0 {
		expires = now.Add(time.Duration(parsed.MaxAge) * time.Second).Unix()
	} else if !parsed.Expires.IsZero() {
		expires = parsed.Expires.Unix()
	}
	next := CodexRouteCookie{Name: parsed.Name, Value: parsed.Value, Domain: domain, Path: path, Expires: expires, HostOnly: hostOnly, Secure: true}
	for i := range cookies {
		if sameRouteCookieSlot(cookies[i], next) {
			cookies[i] = next
			return cookies
		}
	}
	return append(cookies, next)
}

func routeCookieDomainAccepted(host, domain string) bool {
	return domain != "" && strings.Contains(domain, ".") && !strings.ContainsAny(domain, " /;\\") && routeCookieDomainMatches(host, domain, false)
}
func routeCookieDomainMatches(host, domain string, hostOnly bool) bool {
	host, domain = strings.ToLower(host), strings.ToLower(domain)
	if hostOnly || host == domain {
		return host == domain
	}
	return strings.HasSuffix(host, "."+domain)
}
func routeCookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return len(requestPath) == len(cookiePath) || strings.HasSuffix(cookiePath, "/") || requestPath[len(cookiePath)] == '/'
}
func defaultRouteCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		return "/"
	}
	if slash := strings.LastIndex(requestPath, "/"); slash > 0 {
		return requestPath[:slash]
	}
	return "/"
}
func sameRouteCookieSlot(a, b CodexRouteCookie) bool {
	return a.Name == b.Name && a.Domain == b.Domain && a.Path == b.Path && a.HostOnly == b.HostOnly
}
func removeRouteCookie(cookies []CodexRouteCookie, name, domain, path string, hostOnly bool) []CodexRouteCookie {
	out := cookies[:0]
	for _, c := range cookies {
		if !sameRouteCookieSlot(c, CodexRouteCookie{Name: name, Domain: domain, Path: path, HostOnly: hostOnly}) {
			out = append(out, c)
		}
	}
	return out
}
func routeCookieExpired(c CodexRouteCookie, now time.Time) bool {
	return c.Expires > 0 && !now.Before(time.Unix(c.Expires, 0))
}
func dropExpiredRouteCookies(cookies []CodexRouteCookie, now time.Time) []CodexRouteCookie {
	out := make([]CodexRouteCookie, 0, len(cookies))
	for _, c := range cookies {
		if !routeCookieExpired(c, now) && validStoredRouteCookie(c) {
			out = append(out, c)
		}
	}
	return out
}
func routeCookieIdentity(cookies []CodexRouteCookie) string {
	cloned := append([]CodexRouteCookie(nil), cookies...)
	sort.Slice(cloned, func(i, j int) bool {
		return cloned[i].Name+cloned[i].Domain+cloned[i].Path < cloned[j].Name+cloned[j].Domain+cloned[j].Path
	})
	var b strings.Builder
	for _, c := range cloned {
		b.WriteString(c.Name)
		b.WriteByte('|')
		b.WriteString(c.Domain)
		b.WriteByte('|')
		b.WriteString(c.Path)
		b.WriteByte('|')
		b.WriteString(c.Value)
		b.WriteByte('\n')
	}
	return b.String()
}
func cloneRouteCookieMap(in map[string][]CodexRouteCookie) map[string][]CodexRouteCookie {
	out := make(map[string][]CodexRouteCookie, len(in))
	for model, cookies := range in {
		out[model] = append([]CodexRouteCookie(nil), cookies...)
	}
	return out
}
func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }
func boolValue(v any) bool     { b, _ := v.(bool); return b }
func int64Value(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
