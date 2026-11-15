// rewrite.go 实现「只改 model、其余字节逐字不动」的请求体改写。
//
// 为什么不用「解 JSON → 改字段 → 重新序列化」：重新序列化会重排键序、改写空白与数字表示，
// 违反「除 model 外字节原样」的契约（上游对请求体做指纹/缓存/签名校验时会立刻暴露）。
// 因此这里用 json.Decoder 的 InputOffset 定位顶层 model 的**值区间**，只替换这段字节。
package magpie

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrBodyNotObject 表示请求体不是 JSON 对象（本链路只接受对象，与既有网关口径一致）。
var ErrBodyNotObject = errors.New("请求体必须是 JSON 对象")

// ErrMissingModel 表示请求体没有顶层的 model 字符串字段。
var ErrMissingModel = errors.New("请求体缺少 model 字段")

// byteSpan 是请求体内的字节区间 [start, end)。
type byteSpan struct{ start, end int }

// stringSpans 定位顶层键 key 的**字符串值**区间（出现多次时全部返回，按出现顺序）。
// 非字符串取值（数字/对象/null）返回 ErrMissingModel 的变体（值类型不对同样无法改写）。
func stringSpans(body []byte, key string) ([]byteSpan, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyNotObject, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrBodyNotObject
	}
	var spans []byteSpan
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBodyNotObject, err)
		}
		name, _ := kt.(string)
		// 键字面量结束位置之后，跳过空白与 ':' 得到值起点。
		vstart := int(dec.InputOffset())
		for vstart < len(body) && isJSONSpace(body[vstart]) {
			vstart++
		}
		if vstart >= len(body) || body[vstart] != ':' {
			return nil, fmt.Errorf("%w: 缺少冒号", ErrBodyNotObject)
		}
		vstart++
		for vstart < len(body) && isJSONSpace(body[vstart]) {
			vstart++
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBodyNotObject, err)
		}
		vend := int(dec.InputOffset())
		if name != key {
			continue
		}
		if len(raw) == 0 || raw[0] != '"' {
			return nil, ErrMissingModel
		}
		spans = append(spans, byteSpan{start: vstart, end: vend})
	}
	return spans, nil
}

func isJSONSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// ModelFromJSON 读取顶层 model（不改写，仅用于准入判定）。
func ModelFromJSON(body []byte) (string, error) {
	spans, err := stringSpans(body, "model")
	if err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return "", ErrMissingModel
	}
	// 取最后一处（JSON 语义：重复键后者生效）。
	s := spans[len(spans)-1]
	var v string
	if err := json.Unmarshal(body[s.start:s.end], &v); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMissingModel, err)
	}
	return v, nil
}

// RewriteModelJSON 把顶层 model 的值替换为 magpieModel，其余字节逐字保留。
// 返回新切片（不改入参）。
//
// 顶层出现**多个 model** 时只改**生效的那一处**（JSON 语义：重复键后者生效，与
// ModelFromJSON 的取值口径一致）。此前是把每一处都替换掉：调用方写
// `{"model":"a","model":"b"}` 会被改成两处都是代理模型名，等于擅自改写了请求体里
// 一处语义上被忽略的字节，违反「除生效的 model 外逐字不动」的契约（矩阵 P-05）。
func RewriteModelJSON(body []byte, magpieModel string) ([]byte, error) {
	spans, err := stringSpans(body, "model")
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, ErrMissingModel
	}
	quoted, err := json.Marshal(magpieModel)
	if err != nil {
		return nil, err
	}
	// 只替换最后一处（生效值）；其余出现的 model 逐字节保留。
	s := spans[len(spans)-1]
	out := make([]byte, 0, len(body)-(s.end-s.start)+len(quoted))
	out = append(out, body[:s.start]...)
	out = append(out, quoted...)
	out = append(out, body[s.end:]...)
	return out, nil
}

// StreamRequested 报告请求体是否声明了流式（body.stream == true）。解析失败视为非流式。
func StreamRequested(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}
