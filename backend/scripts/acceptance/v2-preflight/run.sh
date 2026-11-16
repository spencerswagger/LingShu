#!/bin/sh
# V2 迁移链「执行前探测」一条命令（四条探测 + 判定行）。
#
#   用法：sh run.sh <库名或 postgres:// DSN>
#         PGHOST/PGPORT/PGUSER 走环境变量（沙盒里 pg_up.sh 起的临时库默认 127.0.0.1:15432 postgres）
#
# 判定：
#   四条输出全空  → S0，可以直接执行 0024（仍要满足「三步证据」与「键约束/形状断言」）
#   任一条有输出  → 不许执行，按数据层契约 §5.2 状态矩阵走 R-1 / R-2
#
# 设计约束（改这里的脚本前先读）：
#   * 探测清单必须等于 `db.V2MigrationFiles()` —— `internal/db/v2_probe_list_test.go` 会红；
#   * 探测② 只列「本链不建」的对象：2026-11-16 裁定 A 之后 magpie_keys/magpie_model 已由 0019b
#     正当创建，留在这里会让干净库每次误报；
#   * 探测③ 直接输出判定行（缺列/多余列/约束不符），不再让人拿列清单心算；S5b 因此也能自动拦。
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DB=${1:-}
if [ -z "$DB" ]; then
  echo "用法: sh $0 <库名或 postgres:// DSN>" >&2
  exit 2
fi

PSQL=${PSQL:-psql}
total=0
for f in probe-1-records.sql probe-2-legacy-objects.sql probe-3-window-shape.sql probe-4-objects-by-record.sql; do
  out=$("$PSQL" -X -v ON_ERROR_STOP=1 -tA -d "$DB" -f "$DIR/$f" 2>&1) || {
    echo "FAIL | $f 执行报错："
    echo "$out" | sed 's/^/       /'
    exit 1
  }
  out=$(printf '%s' "$out" | sed '/^$/d')
  n=0
  [ -n "$out" ] && n=$(printf '%s\n' "$out" | wc -l | tr -d ' ')
  total=$((total + n))
  if [ "$n" -eq 0 ]; then
    echo "PASS | $f（0 行）"
  else
    echo "FAIL | $f（$n 行）"
    printf '%s\n' "$out" | sed 's/^/       /'
  fi
done

echo "---"
if [ "$total" -eq 0 ]; then
  echo "判定：S0（干净）—— 可执行 0024；执行前后三步证据与保留表行数判据仍需单独出。"
  exit 0
fi
echo "判定：非 S0（$total 行异常）—— 不许执行 0024，按 §5.2 走 R-1/R-2。"
exit 1
