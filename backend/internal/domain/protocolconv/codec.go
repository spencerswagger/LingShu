package protocolconv

import (
	"bytes"
	"encoding/json"
	"strings"
)

type RequestCodec interface {
	ParseRequest(body []byte) (*CanonicalRequest, *LossyReport, error)
	SerializeRequest(req *CanonicalRequest) ([]byte, *LossyReport, error)
}

type ResponseCodec interface {
	ParseResponse(body []byte) (*CanonicalResponse, *LossyReport, error)
	SerializeResponse(resp *CanonicalResponse) ([]byte, *LossyReport, error)
}

type StreamCodec interface {
	DecodeEvent(data []byte) ([]CanonicalEvent, *LossyReport, error)
	EncodeEvent(ev CanonicalEvent) ([][]byte, *LossyReport, error)
}

type Codec interface {
	RequestCodec
	ResponseCodec
	StreamCodec
}

// marshalJSONNoEscape 以禁用 HTML 转义的编码器序列化 JSON，避免工具 schema/参数中的 <、>、& 被转义。
func marshalJSONNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// splitDataURL 拆解 data URL（data:<media_type>;base64,<data>）为媒体类型与 base64 数据。
func splitDataURL(url string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := url[len("data:"):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	return strings.TrimSuffix(meta, ";base64"), payload, true
}
