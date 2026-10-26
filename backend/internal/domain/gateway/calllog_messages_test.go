package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件是 Owl PR #63 建议 4 的回归：`req_messages` 全量落库**不设上限**会撑爆单条日志，
// 但也不能回退成「只留最后一条 / 丢 system 与前文」（那是改造前的 bug）。
// 现在的口径：超总量时保留 system + 最新、从最旧的非 system 开始裁，并且**显式标记**裁剪原因。

// TestCapReqMessages_UnderLimitUnchanged 未超限：原样返回、无标记（零差异）。
func TestCapReqMessages_UnderLimitUnchanged(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"s"}`),
		json.RawMessage(`{"role":"user","content":"hello"}`),
	}
	out, info := capReqMessages(msgs)
	if info != nil {
		t.Fatalf("未超限不应产生裁剪标记: %+v", info)
	}
	if len(out) != len(msgs) {
		t.Fatalf("未超限不应改变条数: %d", len(out))
	}
}

// TestCapReqMessages_KeepsSystemAndNewest 超总量：保留 system 与最新消息，
// 从最旧的非 system 开始裁，并给出可对账的标记。
func TestCapReqMessages_KeepsSystemAndNewest(t *testing.T) {
	big := strings.Repeat("x", 40*1024) // 每条 40KB
	msgs := []json.RawMessage{json.RawMessage(`{"role":"system","content":"SYS"}`)}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, json.RawMessage(`{"role":"user","content":"`+big+`"}`))
	}
	out, info := capReqMessages(msgs)
	if info == nil {
		t.Fatal("超总量必须给出裁剪标记")
	}
	if info.Reason != "size" || info.LimitBytes != reqMessagesMaxBytes {
		t.Fatalf("标记内容不对: %+v", info)
	}
	if info.OriginalCount != len(msgs) || info.KeptCount != len(out) || info.DroppedCount != len(msgs)-len(out) {
		t.Fatalf("标记对不上账: info=%+v kept=%d", info, len(out))
	}
	// system 必须保留（这是原 bug 的核心：不许丢 system 与前文语义）。
	if string(out[0]) != `{"role":"system","content":"SYS"}` {
		t.Fatalf("system 消息被裁掉: %s", out[0])
	}
	// 保留的最后一条应是原最后一条（最新优先）。
	if string(out[len(out)-1]) != string(msgs[len(msgs)-1]) && !strings.Contains(string(out[len(out)-1]), "正文过长已截断") {
		t.Fatalf("最新消息未保留: %s", out[len(out)-1])
	}
	// 总量受控。
	total := 0
	for _, m := range out {
		total += len(m)
	}
	if total > reqMessagesMaxBytes {
		t.Fatalf("裁剪后总量 %d > 上限 %d", total, reqMessagesMaxBytes)
	}
}

// TestCapReqMessages_TruncatesSingleHugeContent 单条超长：按 rune 边界截断正文并留标记，
// 不乱码、不切成半个字符（与 truncateUTF8 同一套实现）。
func TestCapReqMessages_TruncatesSingleHugeContent(t *testing.T) {
	huge := strings.Repeat("中文内容", 60*1024) // ~720KB，含大量多字节字符
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"sys"}`),
		json.RawMessage(`{"role":"user","content":"` + huge + `"}`),
	}
	out, info := capReqMessages(msgs)
	if info == nil || info.ContentCut == 0 {
		t.Fatalf("单条超长应记 content_cut: %+v", info)
	}
	if len(out) != 2 {
		t.Fatalf("单条超长不应丢消息: %d", len(out))
	}
	if !utf8.Valid(out[1]) || strings.ContainsRune(string(out[1]), utf8.RuneError) {
		t.Fatal("截断后的消息不是合法 UTF-8 或含 U+FFFD")
	}
	if !strings.Contains(string(out[1]), "正文过长已截断") {
		t.Fatal("截断后应有省略标记")
	}
	var msg map[string]any
	if err := json.Unmarshal(out[1], &msg); err != nil {
		t.Fatalf("截断后的消息不是合法 JSON: %v", err)
	}
}

// TestCaptureReqIncrement_CarriesTruncMarker 端到端：capture 返回的标记会随快照落库，
// 让排查者知道「看到的不是全量」。
func TestCaptureReqIncrement_CarriesTruncMarker(t *testing.T) {
	big := strings.Repeat("y", 60*1024)
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-4","messages":[{"role":"system","content":"sys"}`)
	for i := 0; i < 8; i++ {
		sb.WriteString(`,{"role":"user","content":"` + big + `"}`)
	}
	sb.WriteString(`]}`)
	g := &Gateway{}
	msgs, _, trunc := g.captureReqIncrement(nil, []byte(sb.String()))
	if trunc == nil {
		t.Fatal("超限消息应返回裁剪标记")
	}
	if len(msgs) == 0 || !strings.Contains(string(msgs[0]), `"role":"system"`) {
		t.Fatal("裁剪后仍应保留 system 消息")
	}
	// 标记随快照落库（req_params.messages_truncated）。
	src := newReqParamsSource([]byte(sb.String()))
	raw := src.buildReqParams(reqParamsInput{
		Endpoint: "/v1/chat/completions", InboundProtocol: "openai-chat", MessagesTruncated: trunc,
	})
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if snap.MessagesTruncated == nil || snap.MessagesTruncated.Reason != "size" {
		t.Fatalf("快照缺裁剪标记: %+v", snap.MessagesTruncated)
	}
	if snap.MessagesTruncated.OriginalCount != 9 {
		t.Fatalf("original_count=%d, want 9", snap.MessagesTruncated.OriginalCount)
	}
}
