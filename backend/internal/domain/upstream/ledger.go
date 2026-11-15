package upstream

import (
	"context"
	"errors"
	"fmt"
)

// 本文件只定义「账本落库写入通道」的接口与占位实现，不定义业务字段。
//
// 为什么现在只有 Values 切片、没有具名字段：账本的最小九字段与 offset 分页口径由 Ark 在
// 0022 迁移里定稿（字段名、类型、唯一键都还没拍板）。这里若先自行命名（比如猜 amount / ts /
// ref），迁移 DDL 一落地就得全体改名，账单侧、对账侧、前端三处跟着改，属于典型返工。
// 因此本切片只锁定「通道形态」：
//   - 一次写一条，返回账本条目引用（供 billing_records.ledger_entry_id 关联）；
//   - 写入必须幂等（同一业务事实重复落库只能得到同一条账本，归因对账要求 100% 匹配）；
//   - 读取按 offset + limit 分页（POC 前的最小形态，绝不臆造 cursor/分页键）。

// LedgerFieldCount 是账本最小字段数（九）。Ark 定稿后由真实字段替换 Values，
// 在此之前本包用「长度必须等于 9」守住载荷形状，避免写入方漏字段。
const LedgerFieldCount = 9

// LedgerRecord 是一条待落库账本记录：九字段按定稿顺序放在 Values 里。
//
// 字段顺序与语义以 0022 迁移定稿为准（本文件不写死任何一个列名）。
type LedgerRecord struct {
	// Values 长度必须等于 LedgerFieldCount，顺序即定稿字段顺序。
	Values []any
}

// Validate 校验载荷形状（字段数量），避免半条账本落库后对账永远配不上。
func (r LedgerRecord) Validate() error {
	if len(r.Values) != LedgerFieldCount {
		return fmt.Errorf("账本记录应有 %d 个字段，got %d", LedgerFieldCount, len(r.Values))
	}
	return nil
}

// LedgerRef 是账本条目引用（外部标识），由写入通道返回，账单侧只存这个值。
type LedgerRef string

// LedgerWriter 是账本落库的唯一写入口。
//
// 实现方约定（Ark 定稿补齐字段后仍然成立）：
//   - Append 幂等：同一业务事实重复调用返回同一 LedgerRef，不得产生第二条账本；
//   - Append 失败必须返回可诊断错误（写明哪条记录、卡在什么约束），不得静默吞掉；
//   - List 按 offset + limit 分页，offset 从 0 开始；limit <= 0 由实现取默认值。
type LedgerWriter interface {
	// Append 落一条账本（幂等），返回账本条目引用。
	Append(ctx context.Context, rec LedgerRecord) (LedgerRef, error)
	// List 按 offset 分页读取账本（对账与账单详情用）。
	List(ctx context.Context, offset, limit int) ([]LedgerRecord, error)
}

// ErrLedgerUnavailable 表示账本写通道尚未装配（本切片只有占位实现）。
//
// 明确报「未装配」而不是返回零值成功：账本是钱的口径，静默成功等于丢账。
var ErrLedgerUnavailable = errors.New("账本写通道未装配（阶段一占位：等待 0022 字段定稿）")

// UnavailableWriter 是占位实现：任何调用都返回 ErrLedgerUnavailable。
type UnavailableWriter struct{}

// UnavailableWriter 必须满足 LedgerWriter（编译期断言，接口形态漂移立刻红）。
var _ LedgerWriter = (*UnavailableWriter)(nil)

// NewUnavailableWriter 创建占位写通道（装配层在 0022 落地前用它占位）。
func NewUnavailableWriter() *UnavailableWriter { return &UnavailableWriter{} }

// Append 实现 LedgerWriter（占位：明确失败）。
func (w *UnavailableWriter) Append(ctx context.Context, rec LedgerRecord) (LedgerRef, error) {
	if err := rec.Validate(); err != nil {
		return "", err
	}
	return "", ErrLedgerUnavailable
}

// List 实现 LedgerWriter（占位：明确失败）。
func (w *UnavailableWriter) List(ctx context.Context, offset, limit int) ([]LedgerRecord, error) {
	return nil, ErrLedgerUnavailable
}
