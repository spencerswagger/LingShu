package magpie

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// errorBodyCaptureLimit 是错误体捕获上限（读到 8KB 就够提取 error.message；不攒整包）。
const errorBodyCaptureLimit = 8 << 10

// upstreamErrorSummaryMaxBytes 是落账单的摘要上限（与 billing 侧兜底一致）。
const upstreamErrorSummaryMaxBytes = 512

// errorBodyCapture 在错误响应上旁路记录一段错误体（不改写转发字节）。
//
// 为什么不用 io.ReadAll：错误体可能很大（HTML 502 页、栈信息），而运营只需要
// 「哪一类错误 + 上游自己怎么说」。8KB 足够覆盖 OpenAI/Anthropic 的错误对象。
type errorBodyCapture struct {
	buf      bytes.Buffer
	limit    int
	secret   string
	overflow bool
}

func newErrorBodyCapture(secret string) *errorBodyCapture {
	return &errorBodyCapture{limit: errorBodyCaptureLimit, secret: secret}
}

// Write 始终返回 len(p), nil：它是 TeeReader 的目标，返回短写会把上游响应截断。
func (c *errorBodyCapture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	remain := c.limit - c.buf.Len()
	if remain > 0 {
		if len(p) <= remain {
			_, _ = c.buf.Write(p)
		} else {
			_, _ = c.buf.Write(p[:remain])
			c.overflow = true
		}
	} else {
		c.overflow = true
	}
	return len(p), nil
}

func (c *errorBodyCapture) Summary() string {
	if c == nil {
		return ""
	}
	return summarizeUpstreamError(c.buf.Bytes(), c.secret)
}

// summarizeUpstreamError 把上游错误体变成一行可入库的摘要：
//   - OpenAI/Anthropic 形状：优先取 `error.message`（+ `error.code`/`error.type` 作前缀）；
//   - 其它 JSON：原文 JSON 文本；
//   - 非 JSON（HTML/纯文本）：原文；
//   - 折叠所有空白为单空格，遮蔽本次凭据，按 UTF-8 边界截到 512B。
func summarizeUpstreamError(raw []byte, secret string) string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if e, ok := obj["error"]; ok {
			switch v := e.(type) {
			case string:
				text = v
			case map[string]any:
				msg, _ := v["message"].(string)
				code, _ := v["code"].(string)
				typ, _ := v["type"].(string)
				switch {
				case msg != "" && code != "":
					text = code + ": " + msg
				case msg != "" && typ != "":
					text = typ + ": " + msg
				case msg != "":
					text = msg
				}
			}
		} else if msg, ok := obj["message"].(string); ok && msg != "" {
			text = msg
		}
	}

	text = strings.ReplaceAll(text, "\x00", "")
	// 与 errbody.MaskSecrets 同口径：短串（<4）不做替换，否则会把正常单词抹成 <redacted>。
	if secret = strings.TrimSpace(secret); len(secret) >= 4 {
		// 先替换带 Bearer 前缀的形态，再替换裸串；避免把 "Bearer <redacted>" 留下半截。
		text = strings.ReplaceAll(text, "Bearer "+secret, "Bearer <redacted>")
		text = strings.ReplaceAll(text, secret, "<redacted>")
	}
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= upstreamErrorSummaryMaxBytes {
		return text
	}
	cut := upstreamErrorSummaryMaxBytes
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

// 编译期确保 errorBodyCapture 是 io.Writer（TeeReader 的要求）。
var _ io.Writer = (*errorBodyCapture)(nil)
