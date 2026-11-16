// query.go 管**原始 query 串**的键名判定与剔除。
//
// 为什么单独抽出来（回归 P1-1）：鉴权侧判断「请求有没有带 ?key=」用的是解码后的键名
// （net/url 的 Query()），而转发侧剔除一度按**原始字面**比较 → `?ke%79=<租户令牌>`、
// `?%6Bey=<租户令牌>` 被判成「带了」却剔不掉，令牌原样出境。
// 现在两处共用同一套解码口径（querySegmentKey），判定与剔除不可能再分叉。
//
// 另一条不变式：剔除只按 '&' 切段、**其余段逐字节原样**保留（含顺序与转义形式），
// 不整体重编码 RawQuery —— 签名/时间戳类参数会被重编码打坏。未命中时返回原串。
package magpie

import (
	"net/url"
	"strings"
)

// querySegmentKey 返回一段 query 的键名（按首个 '=' 之前的原始字面，再做 URL 解码）。
// 解码失败时按原字面比较（不静默放行）。
func querySegmentKey(seg string) string {
	if i := strings.IndexByte(seg, '='); i >= 0 {
		seg = seg[:i]
	}
	if dec, err := url.QueryUnescape(seg); err == nil {
		return dec
	}
	return seg
}

// queryParamValue 取原始 query 里名为 name 的**第一个**参数值（键名与值都按解码口径），
// 与 net/url 的 Query().Get 语义一致，但键名判定走 querySegmentKey（同一口径）。
func queryParamValue(raw, name string) (string, bool) {
	if raw == "" {
		return "", false
	}
	for _, seg := range strings.Split(raw, "&") {
		if seg == "" || querySegmentKey(seg) != name {
			continue
		}
		i := strings.IndexByte(seg, '=')
		if i < 0 {
			return "", true
		}
		v := seg[i+1:]
		if dec, err := url.QueryUnescape(v); err == nil {
			return dec, true
		}
		return v, true
	}
	return "", false
}

// hasQueryParam 报告原始 query 串里是否存在名为 name 的参数（键名按解码口径比较）。
func hasQueryParam(raw, name string) bool {
	_, ok := queryParamValue(raw, name)
	return ok
}

// stripQueryParam 剔除名为 name 的参数（键名按解码口径比较），其余段逐字节原样。
// 未命中时返回原串（「没有这个参数就一个字节都不动」）；全剔完返回空串。
func stripQueryParam(raw, name string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	kept := make([]string, 0, len(parts))
	removed := false
	for _, seg := range parts {
		if seg != "" && querySegmentKey(seg) == name {
			removed = true
			continue
		}
		kept = append(kept, seg)
	}
	if !removed {
		return raw
	}
	// 只可能剔出首尾的空段（'&' 连写），修剪后不留多余分隔符。
	return strings.Trim(strings.Join(kept, "&"), "&")
}
