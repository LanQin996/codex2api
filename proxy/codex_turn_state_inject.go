package proxy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// 观测到的 state 实测在 300 字符上下，留一个数量级的余量即可；超限的一律丢弃，
// 截断后的 state 既不能复用也会误导排查。
const maxObservedCodexTurnStateBytes = 4096

// observedCodexTurnState 规整一个观测值：只接受单行可见字符串，超限丢弃。
func observedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedCodexTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

var codexTurnStateFrameNeedles = [][]byte{[]byte("turn-state"), []byte("Turn-State")}

// codexTurnStateFromFrame 从 WS 事件帧里找上游回带的 turn state。官方契约里 WS 路径
// 的值来自握手响应头或 response.metadata 事件；这里按键名等值（大小写不敏感）在几个
// 已知承载位置上找，找不到返回空。先做一次零分配的子串预检，避免每帧都解析 JSON。
func codexTurnStateFromFrame(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	mentioned := false
	for _, needle := range codexTurnStateFrameNeedles {
		if bytes.Contains(payload, needle) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return ""
	}
	for _, path := range []string{"headers", "response.headers", "response.client_metadata", "client_metadata", "response.metadata", "metadata", "response", ""} {
		object := root
		if path != "" {
			object = root.Get(path)
		}
		if !object.IsObject() {
			continue
		}
		state := ""
		object.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
				state = value.String()
				return false
			}
			return true
		})
		if state = observedCodexTurnState(state); state != "" {
			return state
		}
	}
	return ""
}

// ObserveCodexTurnStateFrame 供 WS 中继在逐帧转发时调用：发现上游回带的 turn state
// 就记到本次尝试的追踪里（用量日志据此显示“回带 Turn State”）。
func ObserveCodexTurnStateFrame(ctx context.Context, payload []byte) string {
	state := codexTurnStateFromFrame(payload)
	if state != "" {
		noteUpstreamTurnState(ctx, state)
	}
	return state
}
