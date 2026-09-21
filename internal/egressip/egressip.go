// Package egressip 查询一条出口代理实际使用的公网出口 IP。
//
// 用途是让运营者核对"铸造票据的出口"和"后续携带该票据的出口"是不是同一个 IP：
// 调用方给出代理 URL，本包经该代理请求 IP 回显端点，返回回显到的出口 IP。
//
// 只依赖标准库。代理 URL 带凭据，所以返回的错误一律经过脱敏（见 redact）：
// 调用方拿到的错误文本里既不会出现代理地址，也不会出现账号密码。
package egressip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout 是单次出口 IP 查询的整体上限：无论调用方给的 ctx 多宽松都不会
	// 超过它，避免慢端点拖住探针收尾。
	DefaultTimeout = 5 * time.Second
	// maxResponseBytes 限制回显响应体，异常端点不会拖住调用方。
	maxResponseBytes = 4096
	// redactedPlaceholder 替换错误文本里的代理地址与凭据。
	redactedPlaceholder = "[redacted]"
)

// defaultEndpoints 依次尝试的回显端点：主用 ip-api.com（形态与 admin 的代理检测
// 一致），必要时用 api6.ipify.org 兜底。两者都必须经同一代理请求，绝不回退直连。
var defaultEndpoints = []string{
	"http://ip-api.com/json/?lang=&fields=status,message,query",
	"https://api6.ipify.org?format=json",
}

// Lookup 经 proxyURL 请求出口 IP 回显端点，返回该出口的公网 IP。
// proxyURL 为空、不可达或响应不可解析时返回错误；调用方不得回退直连探测。
func Lookup(ctx context.Context, proxyURL string) (string, error) {
	return LookupEndpoints(ctx, proxyURL, defaultEndpoints)
}

// LookupEndpoints 是 Lookup 的可注入端点版本，供测试与自建回显端点使用。
// 端点按顺序尝试，第一个解析出出口 IP 的结果即为答案。
func LookupEndpoints(ctx context.Context, proxyURL string, endpoints []string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := parseProxyURL(proxyURL)
	if err != nil {
		return "", err
	}
	transport, err := newProxyTransport(parsed)
	if err != nil {
		return "", redact(err, parsed)
	}
	defer transport.CloseIdleConnections()

	lookupCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	client := &http.Client{Transport: transport}

	var lastErr error
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		ip, err := fetchExitIP(lookupCtx, client, endpoint)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no egress ip endpoint configured")
	}
	return "", redact(lastErr, parsed)
}

func fetchExitIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("build echo request failed")
	}
	// 一次性查询：不复用连接，也就不把连接留在代理上。
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("echo request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("echo endpoint returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", errors.New("read echo response failed")
	}
	ip := parseExitIP(body)
	if ip == "" {
		return "", errors.New("echo response has no exit ip")
	}
	return ip, nil
}

// parseExitIP 容忍三种回显形态：{"query":...}、{"ip":...} 与纯文本（含 ip= 行）。
// 解析不出合法 IP 时返回空串，由调用方按失败处理。
func parseExitIP(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "{") {
		var payload struct {
			Query string `json:"query"`
			IP    string `json:"ip"`
		}
		if err := json.Unmarshal([]byte(text), &payload); err == nil {
			for _, candidate := range []string{payload.Query, payload.IP} {
				if ip := normalizeIP(candidate); ip != "" {
					return ip
				}
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if ip := normalizeIP(strings.TrimPrefix(strings.TrimSpace(line), "ip=")); ip != "" {
			return ip
		}
	}
	return normalizeIP(text)
}

func normalizeIP(candidate string) string {
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(candidate), `"`))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func parseProxyURL(proxyURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return nil, errors.New("proxy url is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("proxy url is invalid")
	}
	return parsed, nil
}

// redact 把错误文本里可能出现的代理地址与凭据替换成占位符：只有出口 IP 允许离开
// 本进程，代理 URL（含账号密码）不允许出现在任何日志或响应里。
func redact(err error, proxy *url.URL) error {
	if err == nil {
		return nil
	}
	if proxy == nil {
		return err
	}
	text := err.Error()
	secrets := []string{proxy.String(), proxy.Host}
	if proxy.User != nil {
		username := proxy.User.Username()
		password, _ := proxy.User.Password()
		secrets = append(secrets, proxy.User.String(), username+":"+password, username, password)
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, redactedPlaceholder)
		}
	}
	return errors.New(text)
}
