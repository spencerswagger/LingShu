#!/bin/sh
# 重建「本批迁移文件」哈希基线（一份清单，发布前与 PR 描述逐条比对）。
#
#   sh restamp.sh [迁移目录] [输出文件]
#   默认：../../internal/db/migrations 与 stdout
#
# 为什么要有这个脚本：迁移框架的幂等键是**完整文件名**，没有内容指纹列，
# 所以「同号同名异内容」在库层面无法自证 —— 唯一防线是发布前对文件系统算哈希。
# 手工抄哈希必然出现「PR 描述里的值与分支上的值不是同一次算的」，这里一次算完。
set -eu
DIR=${1:-$(CDPATH= cd -- "$(dirname -- "$0")/../../../internal/db/migrations" && pwd)}
OUT=${2:-}
{
  echo "# V2 迁移链哈希基线（$(date -u '+%Y-%m-%dT%H:%M:%SZ')）"
  echo "# 目录: $DIR"
  for f in "$DIR"/*.sql; do
    b=$(basename "$f")
    case "$b" in
      0019*|0020*|0022*|0023*|0024*) ;;
      *) continue ;;
    esac
    printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$b"
  done
} | if [ -n "$OUT" ]; then tee "$OUT"; else cat; fi
