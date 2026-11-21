package magpie

import "github.com/team/llmgateway/internal/domain/identity"

// CallerKeyToken 是同步钩子需要的令牌最小信息（identity.Token 满足）：
// 改名需要 tokenID（坐标段）与 display_name（可读段 slug 的原料）。
// 别名导出，避免 magpie 包内重复定义。
type TokenRef = identity.CallerKeyToken
