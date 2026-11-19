#!/bin/bash
# 回归：权限归一化不得抹掉 owner 执行位；敏感文件去 group/other；幂等。
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=../lib/perms.sh disable=SC1091
. "$HERE/../lib/perms.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/cache/magpie/bun/1.4.4" "$TMP/magpie"
printf '#!/bin/sh\necho ok\n' > "$TMP/cache/magpie/bun/1.4.4/magpie-bun"
chmod 755 "$TMP/cache/magpie/bun/1.4.4/magpie-bun"
printf '{"k":1}\n' > "$TMP/magpie/caller-keys.json"
chmod 644 "$TMP/magpie/caller-keys.json"
printf 'secret' > "$TMP/.magpie_web_key"
chmod 666 "$TMP/.magpie_web_key"

fail=0
ck() { if [ "$2" = "$3" ]; then printf '  ok   %s\n' "$1"; else printf '  FAIL %s (want %s got %s)\n' "$1" "$2" "$3"; fail=1; fi; }

normalize_config_perms "$TMP"
m1=$(stat -c '%a' "$TMP/cache/magpie/bun/1.4.4/magpie-bun")
ck "cache 里 ELF 执行位保留" 755 "$m1"
j1=$(stat -c '%a' "$TMP/magpie/caller-keys.json")
ck "JSON 去 group/other" 600 "$j1"
w1=$(stat -c '%a' "$TMP/.magpie_web_key")
ck ".magpie_web_key 600" 600 "$w1"
d1=$(stat -c '%a' "$TMP")
ck "目录 700" 700 "$d1"

# 幂等：再跑一次，模式不变
normalize_config_perms "$TMP"
m2=$(stat -c '%a' "$TMP/cache/magpie/bun/1.4.4/magpie-bun")
j2=$(stat -c '%a' "$TMP/magpie/caller-keys.json")
ck "二次执行 ELF 模式不变" "$m1" "$m2"
ck "二次执行 JSON 模式不变" "$j1" "$j2"

if [ "$fail" -ne 0 ]; then echo "权限测试失败" >&2; exit 1; fi
echo "权限测试通过"
