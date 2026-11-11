// Package randbody 生成**随机**的上游响应体样本，供「原文可见性」这类用例使用。
//
// 为什么要随机：固定样例（某个厂商某段文案）测不出「有没有按内容白名单/字段裁剪/映射表把原文吃掉」——
// 只要实现里有一条针对该样例的特判，固定样例照样过。随机值的意义就是让任何「按内容判断」的实现露馅：
// 字段名随机、嵌套层级随机、长度随机、字符集随机（含 <>&"' 与中文）。
//
// 只给测试用（internal/testutil 下，不进生产二进制依赖链）。
package randbody

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
)

// alphabet 覆盖 ASCII 字母数字、URL/JSON 敏感标点、空白与中文：
// <>&"' 在 JSON 序列化时会被转义，是最容易暴露「按形状裁剪」的字符；中文用于验证按 rune 边界截断。
const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
	" <>&\"'{}[]:;,./\\|?=#%$@!*()_+-" +
	"\n\t" +
	"中文测试内容错误请求参数模型不存在余额不足上游拒绝"

// Text 生成 [minLen, maxLen) 长度的随机文本（按 rune 计，含中文与标点）。
func Text(rnd *rand.Rand, minLen, maxLen int) string {
	if maxLen <= minLen {
		maxLen = minLen + 1
	}
	n := minLen + rnd.Intn(maxLen-minLen)
	runes := []rune(alphabet)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(runes[rnd.Intn(len(runes))])
	}
	return b.String()
}

// Word 生成 [3,12) 长度的随机标识（仅字母数字，可直接当 JSON 键名）。
func Word(rnd *rand.Rand) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	n := 3 + rnd.Intn(9)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(letters[rnd.Intn(len(letters))])
	}
	return b.String()
}

// AlnumID 生成「看起来像凭据」的长标识：大小写字母 + 数字混排、长度 ≥ minLen（默认取 20 起）。
// 用途是钉住「不得按样式脱敏」——历史清洗规则会把 ≥20 位、含数字与字母、且带大写（或 ≥32 位）
// 的连续串整段替换成 ***；这类串在真实报文里通常是 request-id / 上游自造的 id / 第三方 token，
// 运营和调用方都需要看到它。
func AlnumID(rnd *rand.Rand, minLen int) string {
	if minLen < 20 {
		minLen = 20
	}
	const upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	n := minLen + rnd.Intn(12)
	var b strings.Builder
	// 前三位保证同时含大写、小写、数字，命中「混合串」的判定条件。
	b.WriteByte(upper[rnd.Intn(len(upper))])
	b.WriteByte(lower[rnd.Intn(len(lower))])
	b.WriteByte(digits[rnd.Intn(len(digits))])
	all := upper + lower + digits
	for b.Len() < n {
		b.WriteByte(all[rnd.Intn(len(all))])
	}
	return b.String()
}

// Value 生成随机 JSON 值：字符串/数字/布尔/null/数组/对象（递归深度受 depth 限制）。
func Value(rnd *rand.Rand, depth int) any {
	if depth <= 0 {
		switch rnd.Intn(4) {
		case 0:
			return Text(rnd, 1, 40)
		case 1:
			return rnd.Intn(100000) - 50000
		case 2:
			return rnd.Intn(2) == 0
		default:
			return nil
		}
	}
	switch rnd.Intn(6) {
	case 0:
		return Text(rnd, 1, 80)
	case 1:
		return rnd.Float64() * 1000
	case 2:
		return rnd.Intn(2) == 0
	case 3:
		return nil
	case 4:
		n := 1 + rnd.Intn(3)
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			arr = append(arr, Value(rnd, depth-1))
		}
		return arr
	default:
		return Doc(rnd, depth-1)
	}
}

// Doc 生成随机 JSON 对象：键名随机、键数随机、嵌套层级随机。
func Doc(rnd *rand.Rand, depth int) map[string]any {
	n := 1 + rnd.Intn(4)
	m := make(map[string]any, n)
	for i := 0; i < n; i++ {
		m[Word(rnd)] = Value(rnd, depth)
	}
	return m
}

// OpenAIError 生成随机 OpenAI 形状错误（顶层 error 是对象）：
// message/type/code 内容全随机，并可能带嵌套 details —— 断言必须只看形状，不能看文案。
func OpenAIError(rnd *rand.Rand) map[string]any {
	e := map[string]any{"message": Text(rnd, 8, 200)}
	if rnd.Intn(2) == 0 {
		e[Word(rnd)] = Text(rnd, 3, 30)
	}
	if rnd.Intn(3) == 0 {
		e["details"] = []any{Doc(rnd, 2), Doc(rnd, 1)}
	}
	doc := map[string]any{"error": e}
	if rnd.Intn(3) == 0 {
		doc[Word(rnd)] = Value(rnd, 1)
	}
	return doc
}

// HTML 生成随机 HTML 错误页（非 JSON 形状）。
func HTML(rnd *rand.Rand) string {
	return fmt.Sprintf("<html><body><h1>%s</h1><div id=%q>%s</div></body></html>",
		Text(rnd, 3, 30), Word(rnd), Text(rnd, 10, 120))
}

// JSONText 把任意值序列化成 JSON 文本（不额外加空白，便于逐字节比对）。
func JSONText(v any) string {
	// 用 json.Marshal 保证是合法 JSON；调用方按字节比对，不做美化。
	b, err := json.Marshal(v)
	if err != nil {
		panic("randbody: marshal: " + err.Error())
	}
	return string(b)
}

// Digest 给出样本的短指纹（仅用于用例名/日志，便于失败时定位是哪种随机值）。
func Digest(s string) string {
	h := 0
	for _, r := range s {
		h = (h*31 + int(r)) & 0x7fffffff
	}
	return strconv.Itoa(h)
}
