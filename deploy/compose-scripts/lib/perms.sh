#!/bin/bash
# 权限归一化（被 magpie-secret-init.sh source，也被 tests/test-perms.sh 直接测）。
#
# 为什么不是「递归 chmod 600」：/config/cache 下有 magpie 自己解包的 ELF（magpie-bun），
# 递归 600 会把 owner 执行位也抹掉，magpie（uid 65532）随后 fork/exec 时报 permission denied。
# 这里的规则：
#   * 目录本身：owner rwx，group/other 无；
#   * /config/cache 之外的文件：只去掉 group/other（保留 owner 的 rwx，含执行位）；
#   * 敏感文件（caller-keys.json / .magpie_web_key）：owner 读写（不主动加/去 x），group/other 无；
#   * /config/cache 整棵树不改模式（由 magpie 自己管理）。
set -u

normalize_config_perms() {
	local d="${1:?config dir required}"
	[ -d "$d" ] || return 0

	# 目录：自己 rwx，其他用户无权限
	if [ "$(id -u)" -eq 0 ]; then
		chown 65532:65532 "$d" 2>/dev/null || true
	fi
	chmod u+rwx,go-rwx "$d" 2>/dev/null || true

	# 非 cache 区域：仅移除 group/other 权限（u+x 保留）
	find "$d" -path "$d/cache" -prune -o -exec chmod go-rwx {} + 2>/dev/null || true

	# 敏感文件：owner 读写，group/other 全无（符号模式不会动 u+x）
	local f
	for f in "$d/magpie/caller-keys.json" "$d/.magpie_web_key"; do
		if [ -e "$f" ]; then
			if [ "$(id -u)" -eq 0 ]; then chown 65532:65532 "$f" 2>/dev/null || true; fi
			chmod u+rw,go-rwx "$f" 2>/dev/null || true
		fi
	done
}
