// Package idgen 提供雪花 ID（Snowflake）生成与 JSON 序列化支持。
//
// 雪花 ID 结构：1 位符号（恒 0）+ 41 位毫秒时间戳（相对自定义纪元）+ 5 位工作节点 + 12 位序列号。
// 同一毫秒内最多 4096 个 ID；工作节点支持 0~31。返回 int64，线程安全。
//
// 背景：应用层主键从数据库自增（BIGSERIAL）切换为应用层生成，便于分布式多实例部署、
// 前端回传（雪花 ID 超出 JS 安全整数，须以字符串传输）与审计轨迹可追踪。
package idgen

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// epochMs 自定义纪元：2024-01-01 00:00:00 UTC（毫秒）。
// 41 位毫秒时间戳在此纪元下可持续约 69 年。
var epochMs = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

const (
	workerBits  = 5  // 工作节点位数
	seqBits     = 12 // 每毫秒序列号位数
	maxWorker   = -1 ^ (-1 << workerBits)
	maxSequence = -1 ^ (-1 << seqBits)

	workerShift = seqBits
	timeShift   = workerBits + seqBits

	// maxBackwardMs 可容忍的时钟回拨上限（毫秒）。超过则告警（仍按逻辑时间继续，不阻塞）。
	maxBackwardMs = 5000
	// rollbackWarnIntervalMs 大幅回拨告警的最小间隔，避免持续回拨时刷屏。
	rollbackWarnIntervalMs = 5000
)

// Generator 雪花 ID 生成器（goroutine 安全）。
type Generator struct {
	mu           sync.Mutex
	workerID     int64
	lastStamp    int64
	sequence     int64
	lastWarnWall int64 // 上次回拨告警的墙钟毫秒（限流告警用）
}

// NewGenerator 创建指定工作节点的生成器；workerID 越界（<0 或 >31）时回退为 0。
func NewGenerator(workerID int64) *Generator {
	if workerID < 0 || workerID > maxWorker {
		workerID = 0
	}
	return &Generator{workerID: workerID}
}

// defaultGen 默认单例生成器（worker=0，适配单机/单实例部署；多实例部署请显式分配 workerID）。
var defaultGen = NewGenerator(0)

// Next 返回下一个雪花 ID（线程安全）。
//
// 时钟异常处理（不阻塞、不产生重复/负数 ID）：
//   - 时钟回拨：退让到上次逻辑时间戳之后继续，绝不回退（否则会重复）。
//   - 回拨幅度超过 maxBackwardMs：告警（限流，避免刷屏），但仍按逻辑时间继续。
//   - 系统时钟早于纪元（now<0）：同样退让到逻辑时间，保证返回值为正（符号位恒 0）。
//   - 同毫秒序列耗尽：借用下一逻辑毫秒（而非忙等自旋），避免在回拨期间长时间持锁阻塞。
func (g *Generator) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	stamp := time.Now().UnixMilli() - epochMs
	if stamp < g.lastStamp {
		// 覆盖两种异常：时钟回拨，或系统时钟早于纪元（stamp<0，直接输出会变负）。
		if back := g.lastStamp - stamp; back > maxBackwardMs {
			if wall := time.Now().UnixMilli(); wall-g.lastWarnWall > rollbackWarnIntervalMs {
				g.lastWarnWall = wall
				slog.Warn("idgen: 检测到时钟大幅回拨，已按逻辑时间继续", "backward_ms", back)
			}
		}
		stamp = g.lastStamp
	}
	if stamp == g.lastStamp {
		g.sequence++
		if g.sequence > maxSequence {
			// 同毫秒序列耗尽：借用下一逻辑毫秒，保证唯一与单调且无需忙等。
			g.lastStamp++
			g.sequence = 0
			stamp = g.lastStamp
		}
	} else {
		g.sequence = 0
	}
	g.lastStamp = stamp

	return (stamp << timeShift) | (g.workerID << workerShift) | g.sequence
}

// New 使用默认单例生成器返回下一个雪花 ID。
func New() int64 { return defaultGen.Next() }

// ID 是雪花 ID 的 JSON 序列化类型：输出为字符串（超出 JS 安全整数，前端须以字符串回传），
// 反序列化兼容字符串与整数两种形式。
type ID int64

// MarshalJSON 将 ID 编码为 JSON 字符串。
func (i ID) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatInt(int64(i), 10))
}

// UnmarshalJSON 解析 JSON 字符串或整数形式的 ID。
func (i *ID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*i = ID(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*i = ID(n)
	return nil
}

// Int64 返回底层 int64 值。
func (i ID) Int64() int64 { return int64(i) }

// IDs 是雪花 ID 切片的 JSON 序列化类型：数组元素输出为字符串，
// 反序列化兼容字符串与整数两种形式（用于 tag_ids / batch-delete ids 等 id 数组字段）。
type IDs []int64

// MarshalJSON 将 ID 切片编码为 JSON 字符串数组。
func (ids IDs) MarshalJSON() ([]byte, error) {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return json.Marshal(out)
}

// UnmarshalJSON 解析 JSON 字符串数组或整数数组。
func (ids *IDs) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return err
			}
			out = append(out, v)
			continue
		}
		var n int64
		if err := json.Unmarshal(item, &n); err != nil {
			return err
		}
		out = append(out, n)
	}
	*ids = out
	return nil
}
