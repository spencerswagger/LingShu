package channelprotocol

import (
	"regexp"
	"sync"
)

// regexCache 缓存已编译的正则（pattern → *regexp.Regexp）：错误策略匹配位于网关热路径，
// 避免每次请求重复编译；编译失败以类型化 nil 缓存（不重复尝试编译非法表达式）。
var regexCache sync.Map

// compiledRegex 返回 pattern 编译后的正则；空串或编译失败返回 nil（视为不约束/不可匹配）。
func compiledRegex(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	if v, ok := regexCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCache.Store(pattern, (*regexp.Regexp)(nil))
		return nil
	}
	regexCache.Store(pattern, re)
	return re
}

// MatchError 在给定错误策略列表中选出「适用于该错误」的最优策略。
//
// 匹配分两步：
//  1. 生效范围过滤（按策略自身的 Scope）：
//     - channel：整渠道命中（不看 modelID）；
//     - models：仅当 modelID ∈ ModelIDs 时命中；
//     此外，若入参 scope 非空，则要求策略 Scope 与之相等（可选收紧过滤）。
//  2. 签名匹配（ErrorMatch 各字段为约束，空/零值表示不约束该维度）：
//     status 非 0 时须相等；errorType/errorCode 非空时须相等；messageRegex 非空时须命中（编译失败视为不命中）。
//
// 优先级：status+errorCode > status+errorType > 仅 status（更具体者优先）；
// 同优先级先出现者优先。无命中返回 (nil, false)。
func MatchError(policies []ErrorPolicy, scope string, modelID int64, status int, errType, errCode, msg string) (*ErrorPolicy, bool) {
	bestRank := -1
	var best *ErrorPolicy
	for i := range policies {
		p := &policies[i]
		if !errorScopeApplies(p, scope, modelID) {
			continue
		}
		if !errorSignatureMatches(p.Match, status, errType, errCode, msg) {
			continue
		}
		if r := errorMatchRank(p.Match); r > bestRank {
			bestRank = r
			best = p
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// errorScopeApplies 判断策略是否适用于当前模型：scope 非空时要求策略 Scope 相等。
func errorScopeApplies(p *ErrorPolicy, scope string, modelID int64) bool {
	if scope != "" && p.Scope != scope {
		return false
	}
	switch p.Scope {
	case ErrorScopeChannel:
		return true
	case ErrorScopeModels:
		return containsInt64(p.ModelIDs, modelID)
	default:
		return false
	}
}

// errorSignatureMatches 判断错误签名是否命中：各约束字段为空/零值表示不约束。
func errorSignatureMatches(m ErrorMatch, status int, errType, errCode, msg string) bool {
	if m.Status != 0 && m.Status != status {
		return false
	}
	if m.ErrorType != "" && m.ErrorType != errType {
		return false
	}
	if m.ErrorCode != "" && m.ErrorCode != errCode {
		return false
	}
	if m.MessageRegex != "" {
		re := compiledRegex(m.MessageRegex)
		if re == nil || !re.MatchString(msg) {
			return false
		}
	}
	return true
}

// errorMatchRank 返回签名具体度排名（越大越优先）：status+code > status+type > 仅 status。
func errorMatchRank(m ErrorMatch) int {
	switch {
	case m.Status != 0 && m.ErrorCode != "":
		return 3
	case m.Status != 0 && m.ErrorType != "":
		return 2
	case m.Status != 0:
		return 1
	default:
		return 0
	}
}

// containsInt64 判断 id 是否在 ids 中。
func containsInt64(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
