// Package audit 提供管理操作审计日志落库（audit_logs 表）。
package audit

import (
	"context"
	"database/sql"
	"expvar"
	"log/slog"
	"time"

	"github.com/team/llmgateway/internal/pkg/idgen"
)

// InsertFailures 审计写入失败累计计数（进程内 expvar）。
// 审计是事后追责的唯一依据，其失效必须比业务失效更早被发现——写入失败不能只留在日志里。
//
// 注意：本服务不暴露 /debug/vars（expvar 只注册在 http.DefaultServeMux，且会一并泄露
// cmdline/memstats），故该计数器外部无法直接读取；出口由 ReportInsertFailures 提供的
// 周期汇总日志承担。改动此处时勿在注释里承诺"可经 /debug/vars 观测"。
var InsertFailures = expvar.NewInt("audit_insert_failures")

// Entry 一条审计记录。Detail 可选，绝不含口令/密钥明文。
type Entry struct {
	UserID     int64
	Username   string
	Action     string
	TargetType string
	TargetID   string
	Detail     any // 可 JSON 序列化结构；nil 则落 NULL
	RequestID  string
	IP         string
}

// Store 审计存储。
type Store struct {
	db *sql.DB
}

// NewStore 创建审计存储。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Insert 写入一条审计记录。
func (s *Store) Insert(ctx context.Context, e Entry) error {
	var uid any
	if e.UserID != 0 {
		uid = e.UserID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs(id, user_id, username, action, target_type, target_id, detail, request_id, ip)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		idgen.New(), uid, nullString(e.Username), e.Action, e.TargetType, e.TargetID, e.Detail, e.RequestID, e.IP)
	if err != nil {
		InsertFailures.Add(1)
	}
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ReportInsertFailures 周期汇总审计写入失败计数，仅在计数发生变化时输出一条 Error 日志。
//
// 为什么用日志而不是指标端点：InsertFailures 是进程内 expvar，本服务不对外暴露
// /debug/vars（会泄露 cmdline/memstats）；改用"总失败数从 0 变正数"作为日志侧信号，
// 配合部署侧的日志告警规则即可发现"审计正在丢"。这是 T1 能存活三轮的直接教训：
// 写入失败静默无声，等于安全控制在被攻击者悄然关闭。
//
// interval ≤ 0 时取 1 分钟。随 ctx 取消而退出。
func ReportInsertFailures(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			last = reportInsertFailuresOnce(logger, last)
		}
	}
}

// reportInsertFailuresOnce 对比当前计数与上次上报值，变化时输出一条 Error 日志并返回当前值；
// 未变化则返回原值。独立成函数以便直接测试"出口确实能读出计数并产生信号"。
func reportInsertFailuresOnce(logger *slog.Logger, last int64) int64 {
	total := InsertFailures.Value()
	if total == last {
		return last
	}
	logger.Error("audit insert failures increased", "total", total, "delta", total-last)
	return total
}
