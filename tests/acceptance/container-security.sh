#!/usr/bin/env bash
# 容器安全加固的「行为」验收 —— 在宿主机（rootful podman）上直跑，不需要 agent 进程。
#
# 为什么需要它：
#   v0.12.x 引入 no_new_privileges 后，单元测试全绿（每个字段都设对了），但 OCI runtime
#   是 crun 时（podman 默认装 crun 的机器），NNP 会让 crun 的 AppArmor profile 切换退化
#   成堆叠标签 <profile>//&crun，而 /etc/apparmor.d/base 只放行
#   signal peer=@{profile_name}（标签须完全相等），于是容器内 root 之间发不出任何真实信号：
#   kill 报 Permission denied、systemctl stop 挂死，只有 kill -0 能过（走 signal set=("exists")）。
#   同一台 Debian 13 上 runc + NNP 正常、crun + NNP 必坏，与发行版无关。
#   光断言配置字段拦不住这类问题，必须断言容器内的实际能力。
#
# 用法:
#   ./container-security.sh [镜像]            # 只跑正向检查（应全部 PASS）
#   ./container-security.sh --with-control    # 额外跑一次已知的错误组合，证明本脚本确实能抓到问题
#
# 参数必须与 manager/podman/podman.go 的 containerSecurityConfig() 保持一致，
# 那边改动时同步这里，否则验收会失真。
set -u

IMAGE=${IMAGE:-docker.io/narwhalcloud/debian:podman}
PREFIX="tmp-secacc-$$"
WITH_CONTROL=0

for arg in "$@"; do
	case "$arg" in
	--with-control) WITH_CONTROL=1 ;;
	-h | --help)
		sed -n '2,20p' "$0"
		exit 0
		;;
	*) IMAGE="$arg" ;;
	esac
done

fail=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
	fail=1
}

cleanup() { podman rm -f "${PREFIX}-ok" "${PREFIX}-bad" >/dev/null 2>&1; }
trap cleanup EXIT

wait_ready() {
	local c=$1 i
	for i in $(seq 1 60); do
		podman exec "$c" true >/dev/null 2>&1 && return 0
		sleep 0.5
	done
	return 1
}

# start_container <名字> <额外的 security-opt...>
start_container() {
	local name=$1
	shift
	podman run -d --rm --name "$name" --network=none \
		--userns=auto:size=65536 --pids-limit=2048 \
		"$@" "$IMAGE" sleep 900 >/dev/null 2>&1 || return 1
	wait_ready "$name"
}

check_suite() {
	local c=$1
	local aa label nnp uidmap denials_before denials_after

	label=$(podman exec "$c" cat /proc/self/attr/current 2>/dev/null)
	printf '  · AppArmor 标签: %s\n' "${label:-<无>}"

	# 1) 同一会话：自己拉起的子进程必须能 kill
	if podman exec "$c" sh -c 'sleep 60 >/dev/null 2>&1 & kill -9 $!; echo $?' 2>/dev/null | tail -1 | grep -qx 0; then
		pass "容器内同会话 kill -9"
	else
		bad "容器内同会话 kill -9（信号投递被拒）"
	fi

	# 2) 跨会话：一个 exec 起进程，另一个 exec 去 kill（等价于用户开两个 SSH 会话）
	local pid
	pid=$(podman exec "$c" sh -c 'sleep 120 >/dev/null 2>&1 & echo $!' 2>/dev/null | tail -1)
	if [ -n "$pid" ] && podman exec "$c" kill -9 "$pid" >/dev/null 2>&1; then
		pass "容器内跨会话 kill -9 (pid=$pid)"
	else
		bad "容器内跨会话 kill -9 (pid=${pid:-?})"
	fi

	# 3) 加固本身不能丢：NNP 仍在
	nnp=$(podman exec "$c" sh -c "awk '/^NoNewPrivs:/{print \$2}' /proc/self/status" 2>/dev/null | tail -1)
	if [ "$nnp" = "1" ]; then
		pass "no_new_privileges 仍然生效"
	else
		bad "no_new_privileges 未生效（got=${nnp:-?}）"
	fi

	# 4) userns 映射仍然是 65536 个 ID（sshd 的 uid 995 / gid 65534 要在映射内）
	uidmap=$(podman exec "$c" cat /proc/self/uid_map 2>/dev/null | tr -s ' ')
	if [ "$(echo "$uidmap" | awk '{print $3}')" = "65536" ]; then
		pass "userns 映射大小 65536"
	else
		bad "userns 映射异常（$uidmap）"
	fi

	# 5) ps 不应该刷 ptrace 拒绝（同一问题是它的伴生症状，也会拖慢 ps/top）
	#    精简镜像可能没有 ps，CI runner 上 dmesg 也可能受限 —— 拿不到就跳过，不误报。
	if ! podman exec "$c" sh -c 'command -v ps' >/dev/null 2>&1; then
		echo "  · 跳过 ps 检查（镜像内无 ps）"
	elif ! dmesg >/dev/null 2>&1; then
		echo "  · 跳过 ps 检查（dmesg 不可读）"
	else
		denials_before=$(dmesg | grep -c 'apparmor="DENIED".*ptrace' || true)
		podman exec "$c" ps -ef >/dev/null 2>&1
		sleep 1
		denials_after=$(dmesg | grep -c 'apparmor="DENIED".*ptrace' || true)
		if [ "${denials_after:-0}" -le "${denials_before:-0}" ]; then
			pass "容器内 ps 无新增 AppArmor ptrace 拒绝"
		else
			bad "容器内 ps 触发 $((denials_after - denials_before)) 条 AppArmor ptrace 拒绝"
		fi
	fi
}

echo "镜像: $IMAGE"

# userns=auto 的 ID 池来自 containers 用户的 subuid/subgid；缺了会直接创建失败，
# 提前给出可执行的修法，避免把环境问题误读成安全配置问题。
if ! grep -q '^containers:' /etc/subuid 2>/dev/null; then
	bad "缺少 userns ID 池（/etc/subuid 无 containers 行）：useradd -r -M -s /usr/sbin/nologin containers && echo 'containers:1000000:6553600' >> /etc/subuid 与 /etc/subgid"
	exit 1
fi

echo "[1/2] 按当前代码的安全配置创建容器"
if start_container "${PREFIX}-ok" --security-opt no-new-privileges --security-opt apparmor=unconfined; then
	check_suite "${PREFIX}-ok"
else
	bad "容器创建失败（镜像是否存在？podman 是否可用？）"
fi

if [ "$WITH_CONTROL" = "1" ]; then
	# 这个缺陷只在 crun 上出现（runc 不会堆叠）。宿主没有 crun 就跳过，不能因此判失败，
	# 否则 runc 机器上这套验收永远是红的，反而没人看。
	if ! command -v crun >/dev/null 2>&1; then
		echo "[2/2] 跳过对照组：宿主未安装 crun（该缺陷在 runc 上不复现）"
	else
		echo "[2/2] 对照组：crun + 只开 NNP、不给 AppArmor unconfined（历史缺陷，应当失败）"
		if start_container "${PREFIX}-bad" --runtime=crun --security-opt no-new-privileges; then
			if podman exec "${PREFIX}-bad" sh -c 'sleep 60 >/dev/null 2>&1 & kill -9 $!; echo $?' 2>/dev/null | tail -1 | grep -qx 0; then
				bad "对照组居然能 kill —— 说明本脚本没测到真实路径，验收不可信"
			else
				pass "对照组按预期失败（脚本确实能抓到该问题）"
			fi
		else
			bad "对照组容器创建失败"
		fi
	fi
else
	echo "[2/2] 跳过对照组（加 --with-control 可验证脚本本身的检出能力）"
fi

echo
if [ "$fail" -eq 0 ]; then
	echo "全部通过"
else
	echo "存在失败项 —— 这套配置不能发布"
fi
exit "$fail"
