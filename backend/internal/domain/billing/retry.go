package billing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RetryQueue 账单落库失败时的本地持久重试队列（WAL）。
//
// 为什么需要：DB 抖动/连接中断会让 billing_records 写入失败，而这一笔对应的上游调用
// 已经发生（甚至钱包已扣款）。直接丢弃就会造成「扣了钱没账单」的对账缺口，因此把整条
// Record 追加到本地文件，由后台周期任务重投：
//   - billing_id 唯一索引保证重投幂等：已落库的记录视为成功并移除；
//   - 重投仍失败的记录保留在文件中并告警，绝不丢弃。
//
// 选本地文件而非数据库表：数据库不可用本身正是落库失败的常见原因，队列不能再依赖它。
type RetryQueue struct {
	path string
	log  *slog.Logger
	mu   sync.Mutex
}

// NewRetryQueue 创建本地重试队列；path 为空返回 nil（表示不启用）。
func NewRetryQueue(path string, log *slog.Logger) *RetryQueue {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &RetryQueue{path: path, log: log}
}

// Enqueue 追加一条待重投记录；写盘并 fsync，保证进程崩溃也不丢。
func (q *RetryQueue) Enqueue(rec *Record) error {
	if q == nil || rec == nil {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal retry record: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if dir := filepath.Dir(q.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir retry dir: %w", err)
		}
	}
	f, err := os.OpenFile(q.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open retry file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append retry record: %w", err)
	}
	return f.Sync()
}

// Pending 返回当前待重投条数。
func (q *RetryQueue) Pending() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	lines, err := q.readLinesLocked()
	if err != nil {
		q.log.Error("billing retry queue read failed", "path", q.path, "err", err)
		return 0
	}
	return len(lines)
}

// Flush 重投队列中全部记录：落库成功（或撞唯一索引=已落库）的移除，仍失败的保留。
// 返回 (成功数, 仍失败数, error)。
func (q *RetryQueue) Flush(ctx context.Context, store Store) (int, int, error) {
	if q == nil || store == nil {
		return 0, 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	lines, err := q.readLinesLocked()
	if err != nil {
		return 0, 0, err
	}
	if len(lines) == 0 {
		return 0, 0, nil
	}

	remain := make([]string, 0, len(lines))
	ok := 0
	for _, line := range lines {
		var rec Record
		if uerr := json.Unmarshal([]byte(line), &rec); uerr != nil {
			// 解析不了的行原样保留（不丢数据），由人工排查。
			q.log.Error("billing retry record unparsable, kept in queue", "path", q.path, "err", uerr)
			remain = append(remain, line)
			continue
		}
		if _, ierr := store.Insert(ctx, &rec); ierr != nil && !isUniqueViolation(ierr) {
			remain = append(remain, line)
			continue
		}
		ok++
	}
	if ok > 0 {
		if werr := q.writeLinesLocked(remain); werr != nil {
			return ok, len(remain), werr
		}
	}
	if len(remain) > 0 {
		return ok, len(remain), fmt.Errorf("账单重试队列仍有 %d 条待重投", len(remain))
	}
	return ok, 0, nil
}

// readLinesLocked 读取队列文件全部非空行；文件不存在视为空队列。
func (q *RetryQueue) readLinesLocked() ([]string, error) {
	f, err := os.Open(q.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open retry file: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan retry file: %w", err)
	}
	return out, nil
}

// writeLinesLocked 以「写临时文件 + 原子改名」的方式重写队列，避免写一半损坏。
func (q *RetryQueue) writeLinesLocked(lines []string) error {
	tmp := q.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open retry tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, werr := w.WriteString(line + "\n"); werr != nil {
			_ = f.Close()
			return fmt.Errorf("write retry tmp: %w", werr)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flush retry tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync retry tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close retry tmp: %w", err)
	}
	return os.Rename(tmp, q.path)
}
