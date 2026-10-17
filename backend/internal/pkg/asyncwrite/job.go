package asyncwrite

import (
	"context"
	"strconv"
)

// Kind 表示异步落库任务的类别，用于分片路由、WAL 记录编码与崩溃重放分发。
type Kind int

const (
	// KindSession 会话状态写入。
	KindSession Kind = iota
	// KindChannelKeyEvent 渠道 Key 事件写入。
	KindChannelKeyEvent
	// KindChannelModelEvent 渠道模型事件写入。
	KindChannelModelEvent
	// KindCallLog 调用日志写入。
	KindCallLog
	// KindTokenLastUsed 令牌最后使用时间写入（通常可容忍少量丢失，适合内存模式）。
	KindTokenLastUsed
)

// String 便于日志排查，未知类别回落到数值。
func (k Kind) String() string {
	switch k {
	case KindSession:
		return "session"
	case KindChannelKeyEvent:
		return "channel_key_event"
	case KindChannelModelEvent:
		return "channel_model_event"
	case KindCallLog:
		return "call_log"
	case KindTokenLastUsed:
		return "token_last_used"
	default:
		return "kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Job 是一次待持久化的写任务。
//
// Exec 为真正的执行闭包；由于 Go 闭包不可序列化，WAL 不存储 Exec，
// 崩溃恢复时改用 Payload + SetReplayFunc 注册的重放函数来重放该任务。
// Payload 为可选的不透明字节，由业务自行编码（如 JSON），仅用于重放。
type Job struct {
	// Kind 任务类别，决定是否异步、以及重放时使用哪个重放函数。
	Kind Kind
	// Key 业务键，决定分片：相同 Key 的任务串行保序，不同 Key 可并发。
	Key string
	// IdempotencyKey 非空时，同一 Key 在一次运行内至多成功执行一次（进程内去重）。
	IdempotencyKey string
	// Payload 可选负载，仅供 WAL 恢复时重放使用，可被序列化。
	Payload []byte
	// Exec 执行闭包；内存模式下即执行体，WAL 模式下崩溃后由重放函数替代。
	Exec func(ctx context.Context) error
}
