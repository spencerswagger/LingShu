// Package automodel 提供 Auto 模型（对外模型之上的有序别名组）的存储与 CRUD：
// 请求 Auto 名时按成员 position 升序选择第一个可用的成员对外模型。
//
// Auto 分两种作用域：全局（scope='global'，管理员创建，对所有用户可见）
// 与私有（scope='private'，开发者创建，仅对 owner 生效）。Auto 不直接绑定渠道。
//
// 数据模型对齐 db/migrations/0011_auto_models.sql 中 auto_models / auto_model_members 表。
package automodel

import (
	"errors"
	"time"
)

// 作用域常量。
const (
	ScopeGlobal  = "global"  // 全局：管理员创建
	ScopePrivate = "private" // 私有：开发者创建，仅对 owner 生效
)

// ErrNameExists 表示 Auto 名称唯一约束冲突（PG 23505，扁平命名空间下跨作用域唯一）。
var ErrNameExists = errors.New("auto model name already exists")

// AutoModel 对应 auto_models 一行（含 JOIN 出的成员列表）。
type AutoModel struct {
	ID          int64
	Name        string
	Scope       string
	OwnerUserID *int64 // global 为 nil；private 必填
	Enabled     bool
	Description string
	Members     []Member // 按 position 升序
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Member 是 Auto 的一个成员（对外模型引用 + 排序位）。
type Member struct {
	ID              int64 // auto_model_members 行 ID
	ExternalModelID int64
	ExternalName    string // JOIN external_models.external_name 展示用
	Enabled         bool   // JOIN external_models.enabled：成员对外模型是否启用
	Position        int
}
