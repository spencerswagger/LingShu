// Package billingday 是「账期日界」的唯一口径。
//
// 为什么必须有这一层：账期按**自然日**切账，而账期起点 `subscriptions.cycle_start` 是 `DATE`
// （没有时区语义）。pgx 把 DATE 扫成 `UTC 0 点`，进程日界却是北京时间（UTC+8）。
// 两件事一旦直接比较就会出错：
//
//   - 「今天起账」的订阅在北京时间 00:00–08:00 被判成 `pending`（未生效）：
//     `2026-11-17 00:00Z` 晚于 `2026-11-17 00:00+08:00`（= 11-16T16:00Z）；
//   - 分摊窗口比列表窗口晚 8 小时：Go 侧用 UTC 0 点、SQL 侧让 PG 按会话时区把 DATE 转成
//     timestamptz，同一个账期出现两个窗口，同一笔账单在列表与分摊里归属不同期。
//
// 所以：**DATE 值一律先还原成账期日（CivilDay），瞬时值一律先落到账期日的 0 点（DayStart）**，
// 比较与窗口计算只许走本包。`Location().String()` 是 SQL 侧 `AT TIME ZONE $n` 的参数，
// 保证库里的窗口与 Go 里的窗口同源（不依赖 PG 会话的 TimeZone）。
package billingday

import (
	"sync"
	"time"
)

// Zone 是账期日界所在时区。全系统只有这一处定义（改时区 = 改这一行 + 数据重算）。
const Zone = "Asia/Shanghai"

// Location 返回账期日界时区。没有 tzdata 的精简镜像里 LoadLocation 会失败，
// 此时回落固定 UTC+8 —— 中国不实行夏令时，等价于 Asia/Shanghai 的日界。
//
// 只解析一次（sync.OnceValue）：LoadLocation 每次都要读一次 zoneinfo 文件，
// 账期窗口计算在分摊/列表的每行都会用到；同时保证同一进程里返回**同一个指针**
// （窗口比较、指针相等的断言不会被时区对象本身干扰）。
var Location = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation(Zone); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
})

// Name 返回 SQL 里 `AT TIME ZONE $n` 用的时区名。
func Name() string { return Location().String() }

// DayStart 把瞬时 t 归到它所在账期日的 0 点。
//
// 例：`2026-11-17 02:00 +08:00` → `2026-11-17 00:00 +08:00`；
// `2026-11-16 17:00Z`（= 北京 11-17 01:00）→ `2026-11-17 00:00 +08:00`。
func DayStart(t time.Time) time.Time {
	loc := Location()
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// CivilDay 把「日历日期值」还原成账期日的 0 点。
//
// 只取 Y/M/D：DATE 列被驱动扫成 UTC 0 点，那个 UTC 是驱动产物、不是语义。
// 例：库里 `2026-11-17` → pgx 给出 `2026-11-17 00:00Z` → 这里返回 `2026-11-17 00:00 +08:00`。
func CivilDay(t time.Time) time.Time {
	loc := Location()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// Date 返回账期日的 `YYYY-MM-DD` 文本。
//
// 写 DATE 列、下发 `CycleStart` 都用它：**不要**把 time.Time 直接塞给 DATE 列，
// 那会经过一次「按会话时区取日期」，在非 UTC+ 时区上整整差一天。
func Date(t time.Time) string { return t.Format("2006-01-02") }

// CycleEnd 返回账期右界（左闭右开）。month=+1 月，year=+1 年，once=当天结束；
// 未知周期按 1 个月处理（宁可摊窄，也不摊到一年）。
//
// **必须与 SQL 侧一致**：列表窗口用的是 PG 的 `cycle_start + interval '1 month'`，
// 而 PG 对月末是**截断**（2026-01-31 + 1 month = 2026-02-28）；
// Go 的 AddDate 是**溢出**（2026-01-31 → 2026-03-03）。
// 两者差 3 天，同一个账期在导出侧（Go 计算）与列表侧（SQL 计算）就会错位，
// 所以这里显式截断到目标月的最后一天，与 PG 同语义（用例见 billingday_test.go 与
// subscription/store_pg_test.go 的 PG 对照）。
//
// 入参先归到账期日 0 点：DATE 列扫出来是 UTC 0 点，直接 AddDate 会把右界也带成 UTC。
func CycleEnd(day time.Time, cycle string) time.Time {
	start := CivilDay(day)
	switch cycle {
	case "year":
		return addMonthsClamped(start, 12)
	case "once":
		return start.AddDate(0, 0, 1)
	default:
		return addMonthsClamped(start, 1)
	}
}

// addMonthsClamped 加 n 个月，日超出目标月长度时截断到月末（与 PG interval 语义一致）。
func addMonthsClamped(t time.Time, n int) time.Time {
	months := int(t.Month()) - 1 + n
	year := t.Year() + months/12
	month := time.Month(((months%12)+12)%12 + 1)
	day := t.Day()
	if last := daysIn(year, month); day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

// daysIn 返回某年某月的天数（下月 0 日 = 本月最后一天）。
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
