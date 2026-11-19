#!/bin/bash
set -e
export DEBIAN_FRONTEND=noninteractive

# ────────────────────────────────────────────────────────────────────────────────
# NarwhalCloud Agent install / update script
# ────────────────────────────────────────────────────────────────────────────────

AGENT_BIN_DIR="/opt/narwhal-agent"
AGENT_BINARY="$AGENT_BIN_DIR/narwhal-agent"
AGENT_SERVICE="narwhal-agent"
AGENT_DATA_DIR="/var/lib/narwhal-agent"
AGENT_CONFIG_DIR="/opt/narwhal-agent"
AGENT_CONFIG_FILE="$AGENT_CONFIG_DIR/config.json"
AGENT_DB="$AGENT_CONFIG_DIR/agent.db"
AGENT_WEB_PORT="8792"
RFW_BIN_DIR="$AGENT_BIN_DIR"   # rfw 与 agent 放在同一目录，匹配面板 rfwBinaryPath()
RFW_API_ADDR="127.0.0.1:7734"  # rfw 仅监听本地，由 agent 面板反代
PODMAN_NETWORK="narwhal-net"

# 发布采用语义化 tag（vX.Y.Z），latest 指向最新正式发布；也允许指定具体 tag。
AGENT_RELEASE_TAG="${AGENT_RELEASE_TAG:-latest}"
if [ "$AGENT_RELEASE_TAG" = "latest" ]; then
    DEFAULT_DOWNLOAD_BASE="https://github.com/tl0502/runman-agent/releases/latest/download"
else
    DEFAULT_DOWNLOAD_BASE="https://github.com/tl0502/runman-agent/releases/download/$AGENT_RELEASE_TAG"
fi
DOWNLOAD_BASE="${RUNMAN_AGENT_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
NETAVARK_BASE="${NETAVARK_BASE:-https://github.com/tl0502/netavark-Runman/releases/latest/download}"
# rfw v2 随 agent 同仓库发布，资产名为 rfw-amd64 / rfw-arm64
RFW_BASE="$DOWNLOAD_BASE"

# t EN ZH — returns the string for current language
t() { [ "$LANG_CODE" = "zh" ] && echo "$2" || echo "$1"; }

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"; }
die() { log "$1" >&2; exit 1; }

# ── IPv6 配置备份 / 一键回滚 ───────────────────────────────────────────────────
# 备份对象：内核参数、网卡、journald/zram、Incus 网络及本安装器的 systemd 文件。
# 回滚时按备份快照逐一恢复，便于本机/容器 IPv6 分配策略试错后快速还原。

backup_ipv6_config() {
    # 备份目录在 /var/lib 下，只有 root 能写。
    [ "$(id -u)" = "0" ] || die "$(t "IPv6 config backup requires root." "IPv6 配置备份需要 root 权限。")"
    local ts; ts=$(date +%Y%m%d-%H%M%S)
    local dir="$BACKUP_DIR/ipv6-$ts"
    mkdir -p "$dir"
    # 仅在“变更前”调用：复制当前（原始）状态，并记录受管文件安装前是否已存在，
    # 供卸载/回滚时区分“恢复原始文件”还是“删除安装时新建的文件”。
    [ -f /etc/sysctl.d/99-narwhalcloud.conf ] && cp /etc/sysctl.d/99-narwhalcloud.conf "$dir/sysctl.conf"
    [ -f /etc/network/interfaces ] && cp /etc/network/interfaces "$dir/interfaces"
    [ -f /etc/systemd/journald.conf ] && cp /etc/systemd/journald.conf "$dir/journald.conf"
    [ -f /etc/systemd/zram-generator.conf ] && cp /etc/systemd/zram-generator.conf "$dir/zram-generator.conf"
    [ -f /etc/udev/rules.d/99-loop-directio.rules ] && cp /etc/udev/rules.d/99-loop-directio.rules "$dir/loop-directio.rules"
    [ -f /etc/containers/storage.conf ] && cp /etc/containers/storage.conf "$dir/containers-storage.conf"
    [ -f /etc/systemd/system/rfw.service ] && cp /etc/systemd/system/rfw.service "$dir/rfw.service"
    {
        echo "IPV6_MODE=${IPV6_MODE:-}"
        echo "IPV6_SUBNET=${IPV6_SUBNET:-}"
        echo "IPV6_ADDR=${IPV6_ADDR:-}"
        echo "IPV6_IFACE=${IPV6_IFACE:-}"
        # 关键：记录受管文件在安装前是否已存在（1=已存在需恢复，0=安装时新建需删除）
        echo "HAD_SYSCTL=$( [ -f /etc/sysctl.d/99-narwhalcloud.conf ] && echo 1 || echo 0 )"
        echo "HAD_JOURNALD=$( [ -f /etc/systemd/journald.conf ] && echo 1 || echo 0 )"
        echo "HAD_ZRAM=$( [ -f /etc/systemd/zram-generator.conf ] && echo 1 || echo 0 )"
        echo "HAD_LOOP_RULE=$( [ -f /etc/udev/rules.d/99-loop-directio.rules ] && echo 1 || echo 0 )"
        echo "HAD_STORAGE_CONF=$( [ -f /etc/containers/storage.conf ] && echo 1 || echo 0 )"
        echo "HAD_RFW_SERVICE=$( [ -f /etc/systemd/system/rfw.service ] && echo 1 || echo 0 )"
    } > "$dir/meta.env"
    echo "$dir" > "$BACKUP_DIR/latest"
    echo "$dir"
}

restore_ipv6_config() {
    # 回滚会改写 /etc 下的 sysctl 与网卡配置、重启 journald，必须 root。
    [ "$(id -u)" = "0" ] || die "$(t "IPv6 config rollback requires root." "IPv6 配置回滚需要 root 权限。")"
    local target="${1:-$(cat "$BACKUP_DIR/latest" 2>/dev/null)}"
    [ -n "$target" ] && [ -d "$target" ] || { log "$(t "No IPv6 backup found" "未找到 IPv6 备份")"; return 1; }
    # 元数据只读取固定的 0/1 标记，不 source 文件，避免环境值被当成 shell 执行。
    meta_flag() {
        local name="$1" value
        value=$(sed -n "s/^${name}=\([01]\)$/\1/p" "$target/meta.env" 2>/dev/null | head -1)
        echo "${value:-0}"
    }
    HAD_SYSCTL=$(meta_flag HAD_SYSCTL)
    HAD_JOURNALD=$(meta_flag HAD_JOURNALD)
    HAD_ZRAM=$(meta_flag HAD_ZRAM)
    HAD_LOOP_RULE=$(meta_flag HAD_LOOP_RULE)
    HAD_STORAGE_CONF=$(meta_flag HAD_STORAGE_CONF)
    HAD_RFW_SERVICE=$(meta_flag HAD_RFW_SERVICE)
    # sysctl：有原文件则恢复；无原文件（安装时新建）则直接删除
    if [ -f "$target/sysctl.conf" ]; then
        cp "$target/sysctl.conf" /etc/sysctl.d/99-narwhalcloud.conf && sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1
    elif [ "${HAD_SYSCTL:-0}" = "0" ] && [ -f /etc/sysctl.d/99-narwhalcloud.conf ]; then
        rm -f /etc/sysctl.d/99-narwhalcloud.conf && sysctl --system >/dev/null 2>&1
        log "$(t "Removed install-created sysctl file" "已删除安装时新建的 sysctl 配置文件")"
    fi
    # interfaces：仅当备份存在时恢复（避免误删用户原有配置）
    [ -f "$target/interfaces" ] && cp "$target/interfaces" /etc/network/interfaces

    # 其它受管文件：安装前存在则恢复原文件；安装时新建则删除。
    restore_managed_file() {
        local backup="$1" destination="$2" existed="$3"
        if [ -f "$target/$backup" ]; then
            mkdir -p "$(dirname "$destination")"
            cp "$target/$backup" "$destination"
        elif [ "$existed" = "0" ]; then
            rm -f "$destination"
        fi
    }
    restore_managed_file journald.conf /etc/systemd/journald.conf "$HAD_JOURNALD"
    restore_managed_file zram-generator.conf /etc/systemd/zram-generator.conf "$HAD_ZRAM"
    restore_managed_file loop-directio.rules /etc/udev/rules.d/99-loop-directio.rules "$HAD_LOOP_RULE"
    if [ "${PRESERVE_PODMAN_STORAGE:-0}" = "1" ]; then
        log "$(t "Preserved Podman storage.conf so retained containers remain manageable." "已保留 Podman storage.conf，确保保留的容器仍可管理。")"
    else
        restore_managed_file containers-storage.conf /etc/containers/storage.conf "$HAD_STORAGE_CONF"
    fi
    restore_managed_file rfw.service /etc/systemd/system/rfw.service "$HAD_RFW_SERVICE"
    systemctl daemon-reload 2>/dev/null || true
    systemctl restart systemd-journald 2>/dev/null || true
    log "$(t "IPv6 config restored from $target" "已从 $target 回滚 IPv6 配置")"
}

# 生成独立的 IPv6 回滚脚本，供运维手动执行（无需重跑安装脚本）
install_ipv6_rollback_helper() {
    mkdir -p "$(dirname "$AGENT_BINARY")"
    cat > "$AGENT_BIN_DIR/ipv6-rollback.sh" <<'EOF'
#!/bin/bash
# NarwhalCloud Agent — IPv6 配置一键回滚
# 用法：
#   ./ipv6-rollback.sh              # 回滚到最近一次备份
#   ./ipv6-rollback.sh <备份目录>   # 回滚到指定备份（/var/lib/narwhal-agent/backups/ipv6-xxx）
set -e
BACKUP_DIR="${BACKUP_DIR:-/var/lib/narwhal-agent/backups}"
TARGET="${1:-$(cat "$BACKUP_DIR/latest" 2>/dev/null)}"
[ -n "$TARGET" ] && [ -d "$TARGET" ] || { echo "未找到 IPv6 备份"; exit 1; }
[ -f "$TARGET/sysctl.conf" ] && { cp "$TARGET/sysctl.conf" /etc/sysctl.d/99-narwhalcloud.conf && sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1; }
[ -f "$TARGET/interfaces" ] && cp "$TARGET/interfaces" /etc/network/interfaces
echo "已回滚 IPv6 配置: $TARGET"
EOF
    chmod +x "$AGENT_BIN_DIR/ipv6-rollback.sh"
    log "$(t "IPv6 rollback helper installed: $AGENT_BIN_DIR/ipv6-rollback.sh" "IPv6 回滚辅助脚本已安装: $AGENT_BIN_DIR/ipv6-rollback.sh")"
}

# ── Token 管理 ──────────────────────────────────────────────────────────────

generate_agent_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 24
    else
        tr -dc 'A-Za-z0-9' </dev/urandom | head -c 48
        echo
    fi
}

validate_agent_token() {
    local value="$1" length=${#1}
    [ "$length" -ge 16 ] && [ "$length" -le 512 ] || {
        log "$(t "Token must contain 16-512 characters." "Token 长度必须为 16-512 个字符。")" >&2
        return 1
    }
    case "$value" in
        *$'\n'*|*$'\r'*|*$'\t'*)
            log "$(t "Token must not contain control characters." "Token 不能包含换行或制表符。")" >&2
            return 1
            ;;
    esac
}

read_agent_token() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 1
    jq -r '.token // empty' "$AGENT_CONFIG_FILE" 2>/dev/null
}

write_agent_token() {
    local value="$1"
    [ "$(id -u)" = "0" ] || die "$(t "Token management requires root." "Token 管理需要 root 权限。")"
    [ -f "$AGENT_CONFIG_FILE" ] || die "$(t "Agent config not found: $AGENT_CONFIG_FILE" "未找到 Agent 配置: $AGENT_CONFIG_FILE")"
    command -v jq >/dev/null 2>&1 || die "jq is required"
    validate_agent_token "$value" || exit 1
    # 设置 Token 的意图就是接入平台，顺带把开关打开——否则会留下
    # 「配了 Token 却因为开关关着而永远不连」的半配状态。
    jq --arg token "$value" '.token = $token | .platform_enabled = true' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp"
    chmod 600 "$AGENT_CONFIG_FILE.tmp"
    mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE"
    systemctl restart "$AGENT_SERVICE" 2>/dev/null || true
}

# set_platform_enabled 开关平台接入（off = 独立模式）。
#
# 关闭后 Agent 完全不建立平台连接，仅通过本地面板管理；重新开启会立即恢复连接。
# 与 --rotate-token 一致：改完配置重启服务生效——Agent 的配置在内存里，
# 外部改文件不会被自动重载。
#
# 用法：set_platform_enabled on|off
set_platform_enabled() {
    local mode=$1 v
    # 参数先校验：非法值应当在任何环境检查之前就报出来，
    # 否则机器上没装过 Agent 时会被"配置文件不存在"掩盖。
    case "$mode" in
        on) v=true ;;
        off) v=false ;;
        *) die "$(t "--platform accepts only on|off" "--platform 只接受 on 或 off")" ;;
    esac
    [ "$(id -u)" = "0" ] || die "$(t "Platform switch requires root." "平台开关需要 root 权限。")"
    [ -f "$AGENT_CONFIG_FILE" ] || die "$(t "Agent config not found: $AGENT_CONFIG_FILE" "未找到 Agent 配置: $AGENT_CONFIG_FILE")"
    command -v jq >/dev/null 2>&1 || die "jq is required"
    jq --argjson v "$v" '.platform_enabled = $v' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp"
    chmod 600 "$AGENT_CONFIG_FILE.tmp"
    mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE"
    systemctl restart "$AGENT_SERVICE" 2>/dev/null || true
    if [ "$v" = "true" ]; then
        log "$(t "✓ Platform access enabled; agent restarted." "✓ 已启用平台接入，Agent 已重启。")"
        [ -n "$(read_agent_token)" ] || log "$(t "! Token is empty; the agent will not connect until one is set." "! Token 为空，设置前 Agent 不会连接平台。")"
    else
        log "$(t "✓ Standalone mode: platform connection disabled, panel only." "✓ 独立模式：已关闭平台连接，仅本地面板管理。")"
    fi
}

show_agent_token() {
    [ "$(id -u)" = "0" ] || die "$(t "Showing the Token requires root." "查看 Token 需要 root 权限。")"
    local value
    value=$(read_agent_token) || die "$(t "Agent config not found." "未找到 Agent 配置。")"
    [ -n "$value" ] || die "$(t "The integration Token is empty." "对接 Token 尚未设置。")"
    printf 'NARWHAL_AGENT_TOKEN=%s\n' "$value"
}

rotate_agent_token() {
    local value="${TOKEN_VALUE:-${NARWHAL_AGENT_TOKEN:-}}"
    if [ -z "$value" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        read -rsp "$(t "New Token (leave blank to generate): " "新 Token（留空自动生成）: ")" value
        echo
    fi
    [ -n "$value" ] || value=$(generate_agent_token)
    write_agent_token "$value"
    log "$(t "✓ Integration Token rotated; agent restarted." "✓ 对接 Token 已轮换，Agent 已重启。")"
    printf 'NARWHAL_AGENT_TOKEN=%s\n' "$value"
    log "$(t "Rotate again: bash install.sh --rotate-token" "再次轮换: bash install.sh --rotate-token")"
}

# ── Incus 制品清理 ──────────────────────────────────────────────────────────

# ── 一键卸载 ────────────────────────────────────────────────────────────────
# 仅撤销本安装器引入的变更，不影响既有业务（其它服务、Podman 容器/镜像默认保留）。
# 最关键的是先恢复 IPv6 / 网卡配置（基于安装前的完整备份），再停止服务、清理文件。
do_uninstall() {
    [ "$(id -u)" = "0" ] || die "$(t "Uninstall requires root." "卸载需要 root 权限。")"
    log "$(t "=== NarwhalCloud Agent uninstall ===" "=== NarwhalCloud Agent 卸载 ===")"
    local install_origin_restored=0 previous_virt="unknown"
    local PRESERVE_PODMAN_STORAGE=0
    if [ -f "$AGENT_CONFIG_FILE" ]; then
        previous_virt=$(grep -o '"virt_type"[[:space:]]*:[[:space:]]*"[^"]*"' "$AGENT_CONFIG_FILE" 2>/dev/null \
            | head -n1 | sed 's/.*:[[:space:]]*"//; s/"$//' || true)
    fi

    # 普通卸载承诺保留后端状态，也不要删除指向 /data 的 Podman storage.conf。
    [ "$previous_virt" = "podman" ] && PRESERVE_PODMAN_STORAGE=1

    # 0. 最先恢复 IPv6 / 网卡配置（最关键：避免遗留错误路由/转发影响业务）
    if [ -d "$BACKUP_DIR" ]; then
        local origin
        origin=$(cat "$BACKUP_DIR/install-origin" 2>/dev/null || true)
        if [ -n "$origin" ] && restore_ipv6_config "$origin"; then
            install_origin_restored=1
        else
            log "$(t "System config restore skipped (no install-origin backup)." "系统配置恢复跳过（无安装前基线备份）。")"
        fi
    fi

    # 1. 停止并禁用服务（仅本 agent，不动其它业务服务）
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "$AGENT_SERVICE" 2>/dev/null || true
        systemctl disable "$AGENT_SERVICE" 2>/dev/null || true
        # rfw 是本安装器的可选组件；安装前若已存在，其 service 文件已在上一步恢复。
        if [ "$install_origin_restored" = "1" ] && [ "${HAD_RFW_SERVICE:-0}" = "0" ]; then
            systemctl stop rfw 2>/dev/null || true
            systemctl disable rfw 2>/dev/null || true
            rm -f /etc/systemd/system/rfw.service
        fi
        if [ -x /usr/local/sbin/runman-podman-forwarding ]; then
            /usr/local/sbin/runman-podman-forwarding remove 2>/dev/null || true
        fi
        systemctl stop runman-podman-forwarding 2>/dev/null || true
        systemctl disable runman-podman-forwarding 2>/dev/null || true
    fi
    rm -f "/etc/systemd/system/${AGENT_SERVICE}.service"
    rm -f /etc/systemd/system/runman-podman-forwarding.service
    rm -f /usr/local/sbin/runman-podman-forwarding
    systemctl daemon-reload 2>/dev/null || true

    # 2. 删除安装时新增/修改的受管文件（restore 已按备份删除新建项，这里兜底）
    rm -f "$AGENT_BIN_DIR/ipv6-rollback.sh"

    # 3. 删除 agent 程序/配置目录，但保留 IPv6 备份（便于事后人工恢复）
    rm -rf "$AGENT_BIN_DIR" "$AGENT_CONFIG_DIR"
    if [ -d "$AGENT_DATA_DIR" ]; then
        for f in "$AGENT_DATA_DIR"/*; do
            [ -e "$f" ] || continue
            [ "$f" = "$BACKUP_DIR" ] && continue
            rm -rf "$f"
        done
    fi

    log "$(t "Uninstall complete. Podman containers, narwhal-net, images and /data are preserved." \
        "卸载完成。已保留 Podman 容器、narwhal-net、镜像与 /data 数据盘。")"
    log "$(t "Backups kept at: $BACKUP_DIR (rerun install.sh --rollback-ipv6 if needed)" "备份保留于: $BACKUP_DIR（如有需要可重新运行 install.sh --rollback-ipv6）")"
}

# ── Language selection ────────────────────────────────────────────────────────

# Support non-interactive mode via environment variable or command line argument
INSTALL_RFW_FORCE=0
# 1: 忽略版本戳，强制重新拉取 Podman 基础镜像
FORCE_IMAGE_REFRESH="${FORCE_IMAGE_REFRESH:-0}"
BACKUP_DIR="${BACKUP_DIR:-/var/lib/narwhal-agent/backups}" # IPv6 配置备份目录
IPV6_ROUTED="${IPV6_ROUTED:-0}"         # 1=独立 routed prefix（6in4/WireGuard 等），无需上游 NDP
IPV6_ROUTED="${IPV6_ROUTED:-0}"         # 1=独立 routed prefix（6in4/WireGuard 等），无需上游 NDP
[ "${PODMAN_REGISTRY_MIRROR+x}" = x ] && PODMAN_REGISTRY_MIRROR_EXPLICIT=1 || PODMAN_REGISTRY_MIRROR_EXPLICIT=0
PODMAN_DATA_SIZE="${PODMAN_DATA_SIZE:-}" # Podman XFS 数据盘大小；留空时按根分区可用空间推荐
PODMAN_REGISTRY_MIRROR="${PODMAN_REGISTRY_MIRROR:-}" # docker.io registry mirror，例如 mirror.example.com
VIRT_TYPE_REQUESTED="${VIRT_TYPE_REQUESTED:-}"
NON_INTERACTIVE="${NON_INTERACTIVE:-0}"
ORIGINAL_ARGC=$#
FORCE_MENU=0
UPDATE_ONLY=0
UPDATE_NETWORK_REQUESTED=0
GUIDED_INSTALL=0
TOKEN_ACTION=""
TOKEN_VALUE="${TOKEN_VALUE:-}"
INITIAL_TOKEN="${NARWHAL_AGENT_TOKEN:-}"
GENERATE_TOKEN=0
# 是否由人显式提供了 Token（环境变量 / --token / --generate-token / 交互输入）。
# 仅用于决定新装默认是否接入平台：安装器在未提供时会自动生成一个 Token，
# 但"自动生成"不等于这台机器真的要接平台——没有显式提供时默认独立模式，
# 避免装完就对着默认地址反复试连。
TOKEN_EXPLICIT=0
if [ -n "$INITIAL_TOKEN" ]; then TOKEN_EXPLICIT=1; fi
# 平台接入开关的一次性操作（on/off）。空表示不改，保持配置里现有的值。
PLATFORM_ACTION="${PLATFORM_ACTION:-}"
SKIP_IPV6_PROBE="${SKIP_IPV6_PROBE:-0}"
SYSTEM_ACTION=""
IMAGE_MENU=0

# 一键模式：IPv6 探测 / 备份 / 回滚，处理后退出
IPV6_ONESHOT_MODE="${IPV6_ONESHOT_MODE:-}"
# 一键卸载模式：--uninstall 撤销本安装器引入的全部变更（含 IPv6 / 网卡恢复）后退出
UNINSTALL="${UNINSTALL:-}"

show_help() {
    cat <<'EOF'
Runman Agent installer

  bash install.sh                         Guided menu
  bash install.sh --update-only           Back up and update Agent only; require an existing installation
  bash install.sh --detect-ipv6            Detect native/tunnel IPv6 and routed prefix
  bash install.sh --validate-ipv6 ...      Validate manually supplied IPv6 values
  bash install.sh --show-token             Print current integration Token
  bash install.sh --rotate-token           Generate, store and print a new Token
  bash install.sh --rotate-token --token X Store and print a custom Token
  bash install.sh --platform off           Standalone mode: stop connecting to the platform
  bash install.sh --platform on            Re-enable platform connection
  bash install.sh --status                 Show Agent/backend/service status
  bash install.sh --restart-agent          Restart Agent and optional rfw
  bash install.sh --reset-panel-password   Reset panel password interactively
  bash install.sh --uninstall              Remove Agent; preserve backend containers/images/data

IPv6: --ipv6-mode none|snat|subnet --ipv6-addr ADDR
      --ipv6-subnet CIDR --ipv6-iface IFACE --ipv6-routed
      --skip-ipv6-probe
Podman: --data-size 10G --podman-registry-mirror mirror.example.com
EOF
}

show_agent_status() {
    local virt="not-installed" agent_state="inactive" backend_state="n/a" rfw_state="not-installed" panel_host
    if [ -f "$AGENT_CONFIG_FILE" ] && command -v jq >/dev/null 2>&1; then
        virt=$(jq -r '.virt_type // "unknown"' "$AGENT_CONFIG_FILE" 2>/dev/null || echo unknown)
    fi
    command -v systemctl >/dev/null 2>&1 && agent_state=$(systemctl is-active "$AGENT_SERVICE" 2>/dev/null || true)
    case "$virt" in
        podman) backend_state=$(systemctl is-active podman.socket 2>/dev/null || true) ;;
    esac
    [ -f "$RFW_BIN_DIR/rfw" ] && rfw_state=$(systemctl is-active rfw 2>/dev/null || true)
    panel_host=$(curl -4 -s --max-time 5 ip.sb 2>/dev/null | head -n1 || true)
    [ -n "$panel_host" ] || panel_host='<host-ip>'
    case "$panel_host" in *:*) panel_host="[$panel_host]" ;; esac
    printf '%s\n' \
        "VIRT_TYPE=$virt" \
        "AGENT_SERVICE=$agent_state" \
        "BACKEND_SERVICE=$backend_state" \
        "RFW_SERVICE=$rfw_state" \
        "PANEL=https://$panel_host:$AGENT_WEB_PORT"
}

ensure_podman_network_from_config() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 0
    command -v jq >/dev/null 2>&1 && command -v podman >/dev/null 2>&1 || return 0
    [ "$(jq -r '.virt_type // ""' "$AGENT_CONFIG_FILE" 2>/dev/null)" = "podman" ] || return 0
    podman network exists "$PODMAN_NETWORK" 2>/dev/null && return 0

    local mode subnet container_base container_gw network_json
    mode=$(jq -r '.ipv6_mode // "none"' "$AGENT_CONFIG_FILE")
    log "$(t "Podman network $PODMAN_NETWORK is missing; rebuilding it from config." \
        "Podman 网络 $PODMAN_NETWORK 缺失，正在按现有配置重建。")"
    case "$mode" in
        none)
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 "$PODMAN_NETWORK"
            ;;
        snat)
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
                --ipv6 --subnet=fd91:cafe:cafe:10::/64 --gateway=fd91:cafe:cafe:10::1 \
                "$PODMAN_NETWORK"
            ;;
        subnet)
            subnet=$(jq -r '.ipv6_subnet // ""' "$AGENT_CONFIG_FILE")
            [ -n "$subnet" ] || die "$(t "Cannot rebuild Podman subnet network: ipv6_subnet is empty." \
                "无法重建 Podman 子网：ipv6_subnet 为空。")"
            read -r container_base container_gw <<< "$(python3 - "$subnet" <<'PYEOF'
import ipaddress
import sys

net = ipaddress.IPv6Network(sys.argv[1], strict=False)
offset = 0xcafe << (128 - net.prefixlen - 16) if net.prefixlen <= 112 else 0
container_int = (int(net.network_address) | offset) & ~((1 << 16) - 1)
container_net = ipaddress.IPv6Network(f'{ipaddress.IPv6Address(container_int)}/112', strict=False)
print(container_net.network_address, container_net.network_address + 1)
PYEOF
)"
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
                --ipv6 --subnet="${container_base}/112" --gateway="$container_gw" \
                "$PODMAN_NETWORK"
            network_json="/etc/containers/networks/${PODMAN_NETWORK}.json"
            jq '.options["snat_ipv6"] = "false"' "$network_json" > "${network_json}.tmp" \
                && mv "${network_json}.tmp" "$network_json"
            ;;
        *) die "$(t "Unsupported Podman IPv6 mode in config: $mode" "配置中的 Podman IPv6 模式无效: $mode")" ;;
    esac
    log "$(t "Podman network $PODMAN_NETWORK rebuilt." "Podman 网络 $PODMAN_NETWORK 已重建。")"
}

restart_agent_services() {
    # systemctl restart 与 podman network 都要求 root；先拦住，给出明确原因，
    # 而不是让 polkit / Permission denied 抛一堆看不懂的报错。
    [ "$(id -u)" = "0" ] || die "$(t "Restarting the Agent requires root." "重启 Agent 需要 root 权限。")"
    [ -f "$AGENT_BINARY" ] || die "$(t "Agent is not installed." "Agent 尚未安装。")"
    ensure_podman_network_from_config
    systemctl restart "$AGENT_SERVICE"
    [ -f "$RFW_BIN_DIR/rfw" ] && systemctl restart rfw 2>/dev/null || true
    log "$(t "Agent services restarted." "Agent 服务已重启。")"
    show_agent_status
}

# 读取 config.json 里的面板密码哈希（config.json 是 0600 root:root，需 root 才能读）。
read_config_pass_hash() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 0
    command -v jq >/dev/null 2>&1 || return 0
    jq -r '.web_pass_hash // ""' "$AGENT_CONFIG_FILE" 2>/dev/null || true
}

reset_panel_password() {
    # config.json 是 0600 root:root，Agent 也由 root 拉起：非 root 执行读不到配置，
    # 而失败信息一旦被重定向吞掉，脚本就会静默退出、用户以为已经改好了。
    [ "$(id -u)" = "0" ] || die "$(t "Resetting the panel password requires root." "重置面板密码需要 root 权限。")"
    [ -x "$AGENT_BINARY" ] && [ -f "$AGENT_CONFIG_FILE" ] \
        || die "$(t "Agent is not installed." "Agent 尚未安装。")"
    [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ] \
        || die "$(t "Panel password reset requires an interactive terminal." "重置面板密码需要交互式终端。")"
    local first second pwd_bytes
    read -rsp "$(t "New panel password (minimum 8 characters): " "新面板密码（至少 8 个字符）: ")" first; echo
    read -rsp "$(t "Confirm new panel password: " "再次输入新面板密码: ")" second; echo
    [ "$first" = "$second" ] || die "$(t "Passwords do not match." "两次密码不一致。")"
    [ "${#first}" -ge 8 ] || die "$(t "Password must contain at least 8 characters." "密码至少需要 8 个字符。")"
    # bcrypt 只吃 72 字节以内，超长会被静默截断成空哈希——这里提前挡住。
    pwd_bytes=$(printf '%s' "$first" | wc -c | tr -d ' ')
    [ "$pwd_bytes" -le 72 ] || die "$(t "Password must be at most 72 bytes (bcrypt limit)." "密码最多 72 字节（bcrypt 上限）。")"

    # 记录旧哈希，事后比对：只有配置真的变了才算成功。
    local before after output
    before=$(read_config_pass_hash)

    # 密码走环境变量而不是命令行：命令行参数会出现在 ps 输出里，对所有本机用户可见。
    # Agent 的日志走 stdout，必须连 stderr 一起收，否则失败原因会被吃掉。
    if ! output="$(NARWHAL_AGENT_RESET_PASSWORD="$first" "$AGENT_BINARY" --config "$AGENT_CONFIG_FILE" 2>&1)"; then
        log "$(t "Failed to reset panel password: ${output:-unknown error}" "重置面板密码失败：${output:-未知错误}")" >&2
        exit 1
    fi

    after=$(read_config_pass_hash)
    if [ -z "$after" ]; then
        log "$(t "Could not verify config.json (jq missing); relying on the Agent exit code." "无法校验 config.json（缺少 jq），仅以 Agent 退出码为准。")" >&2
    elif [ "$after" = "$before" ]; then
        log "$(t "The Agent exited successfully but config.json was not updated." "Agent 退出成功，但 config.json 未发生变化。")" >&2
        exit 1
    fi

    if ! systemctl restart "$AGENT_SERVICE"; then
        log "$(t "Password updated, but restarting the Agent failed." "密码已更新，但重启 Agent 失败。")" >&2
        exit 1
    fi
    log "$(t "Panel password reset; Agent restarted." "面板密码已重置，Agent 已重启。")"
}

show_main_menu() {
    printf '\n%s\n' "$(t "Runman Agent guided operations" "Runman Agent 菜单引导")"
    printf '  1) %s\n' "$(t "Install/update (guided network setup)" "安装/更新（网络引导配置）")"
    printf '  2) %s\n' "$(t "Show installation/service status" "查看安装/服务状态")"
    printf '  3) %s\n' "$(t "Repair network and restart Agent/firewall" "修复网络并重启 Agent/防火墙")"
    printf '  4) %s\n' "$(t "Reset Web panel password" "重置 Web 面板密码")"
    printf '  5) %s\n' "$(t "Detect IPv6 only" "仅探测 IPv6")"
    printf '  6) %s\n' "$(t "Validate a manual IPv6 /64" "验证手工 IPv6 /64")"
    printf '  7) %s\n' "$(t "Show integration Token" "显示对接 Token")"
    printf '  8) %s\n' "$(t "Rotate integration Token" "轮换对接 Token")"
    printf '  9) %s\n' "$(t "Back up IPv6 configuration" "备份 IPv6 配置")"
    printf ' 10) %s\n' "$(t "Roll back IPv6 configuration" "回滚 IPv6 配置")"
    printf ' 11) %s\n' "$(t "Uninstall Agent (keep containers/images)" "卸载 Agent（保留容器/镜像）")"
    printf ' 12) %s\n' "$(t "Configure/refresh container images" "配置/刷新容器镜像")"
    printf ' 13) %s\n' "$(t "Agent-only upgrade (keep networking)" "仅升级 Agent（保留网络）")"
    printf '  0) %s\n' "$(t "Exit" "退出")"
    read -rp '> ' _menu_choice
    case "${_menu_choice}" in
        1) GUIDED_INSTALL=1 ;;
        2) SYSTEM_ACTION="status" ;;
        3) SYSTEM_ACTION="restart" ;;
        4) SYSTEM_ACTION="reset-password" ;;
        5) IPV6_ONESHOT_MODE="detect" ;;
        6) IPV6_ONESHOT_MODE="validate" ;;
        7) TOKEN_ACTION="show" ;;
        8) TOKEN_ACTION="rotate" ;;
        9) IPV6_ONESHOT_MODE="backup" ;;
        10) IPV6_ONESHOT_MODE="rollback" ;;
        11) UNINSTALL=1 ;;
        12) GUIDED_INSTALL=1; FORCE_IMAGE_REFRESH=1; IMAGE_MENU=1 ;;
        13) UPDATE_ONLY=1 ;;
        0) exit 0 ;;
        *) die "$(t "Invalid menu selection." "无效的菜单选项。")" ;;
    esac
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        zh) LANG_CODE="zh"; shift ;;
        en) LANG_CODE="en"; shift ;;
        --virt) [ $# -ge 2 ] || die "--virt requires a value"; VIRT_TYPE_REQUESTED="$2"; shift 2 ;;
        --install-rfw) INSTALL_RFW_FORCE=1; shift ;;
        --force-images) FORCE_IMAGE_REFRESH=1; shift ;;
        --data-size) [ $# -ge 2 ] || die "--data-size requires a value"; PODMAN_DATA_SIZE="$2"; shift 2 ;;
        --podman-registry-mirror) [ $# -ge 2 ] || die "--podman-registry-mirror requires a value"; PODMAN_REGISTRY_MIRROR="$2"; PODMAN_REGISTRY_MIRROR_EXPLICIT=1; shift 2 ;;
        --ipv6-mode) [ $# -ge 2 ] || die "--ipv6-mode requires a value"; IPV6_MODE="$2"; shift 2 ;;
        --ipv6-addr) [ $# -ge 2 ] || die "--ipv6-addr requires a value"; IPV6_ADDR="$2"; shift 2 ;;
        --ipv6-subnet) [ $# -ge 2 ] || die "--ipv6-subnet requires a value"; IPV6_SUBNET="$2"; shift 2 ;;
        --ipv6-iface) [ $# -ge 2 ] || die "--ipv6-iface requires a value"; IPV6_IFACE="$2"; shift 2 ;;
        --ipv6-routed) IPV6_ROUTED=1; shift ;;
        --skip-ipv6-probe) SKIP_IPV6_PROBE=1; shift ;;
        --token) [ $# -ge 2 ] || die "--token requires a value"; INITIAL_TOKEN="$2"; TOKEN_VALUE="$2"; TOKEN_EXPLICIT=1; shift 2 ;;
        --generate-token) GENERATE_TOKEN=1; TOKEN_EXPLICIT=1; shift ;;
        -y|--yes|--non-interactive) NON_INTERACTIVE=1; shift ;;
        --menu) FORCE_MENU=1; shift ;;
        --update-only) UPDATE_ONLY=1; shift ;;
        --detect-ipv6) IPV6_ONESHOT_MODE="detect"; shift ;;
        --validate-ipv6) IPV6_ONESHOT_MODE="validate"; shift ;;
        --backup-ipv6) IPV6_ONESHOT_MODE="backup"; shift ;;
        --rollback-ipv6) IPV6_ONESHOT_MODE="rollback"; shift ;;
        --show-token) TOKEN_ACTION="show"; shift ;;
        --rotate-token) TOKEN_ACTION="rotate"; shift ;;
        --platform) [ $# -ge 2 ] || die "--platform requires on|off"; PLATFORM_ACTION="$2"; shift 2 ;;
        --status) SYSTEM_ACTION="status"; shift ;;
        --restart-agent) SYSTEM_ACTION="restart"; shift ;;
        --reset-panel-password) SYSTEM_ACTION="reset-password"; shift ;;
        --uninstall) UNINSTALL=1; shift ;;
        -h|--help) show_help; exit 0 ;;
        *) die "Unknown option: $1" ;;
    esac
done

# 本 fork 仅维护 Podman 后端；KVM / Incus 相关代码保留在 agent 内，但安装器不再提供。
case "$VIRT_TYPE_REQUESTED" in
    ""|podman) ;;
    *) die "This fork only supports the Podman backend (got --virt $VIRT_TYPE_REQUESTED)." ;;
esac
case "${IPV6_MODE:-}" in
    ""|none|snat|subnet) ;;
    *) die "Invalid IPv6 mode '${IPV6_MODE}' (expected none, snat, or subnet)" ;;
esac

# One-shot operations and explicit non-interactive installs must never pause for
# a language selection. Guided/menu installs keep the bilingual prompt.
if [ -z "$LANG_CODE" ] && { [ -n "$SYSTEM_ACTION" ] || [ -n "$TOKEN_ACTION" ] \
    || [ -n "$PLATFORM_ACTION" ] || [ -n "$IPV6_ONESHOT_MODE" ] || [ "$UNINSTALL" = "1" ] \
    || [ "$NON_INTERACTIVE" = "1" ]; }; then
    LANG_CODE="en"
fi
if [ -z "$LANG_CODE" ]; then
    printf "Select language / 选择语言:\n  1) English (default)\n  2) 中文\n"
    if [ -t 0 ]; then
        read -t 10 -rp "> " _lang_choice || _lang_choice=1
    else
        _lang_choice=1
    fi
    case "${_lang_choice}" in
        2) LANG_CODE="zh" ;;
        *) LANG_CODE="en" ;;
    esac
fi

# 无参数且有终端时默认打开总菜单；--menu 可显式打开。
if [ "$FORCE_MENU" = "1" ] || { [ "$ORIGINAL_ARGC" -eq 0 ] && [ -t 0 ]; }; then
    show_main_menu
fi

# ── 一键 Token / 备份 / 回滚 / 卸载（处理完即退出）──
if [ "$UPDATE_ONLY" = "1" ]; then
    [ -z "$SYSTEM_ACTION" ] && [ -z "$TOKEN_ACTION" ] && [ -z "$PLATFORM_ACTION" ] && [ -z "$IPV6_ONESHOT_MODE" ] \
        && [ "$UNINSTALL" != "1" ] && [ "$GENERATE_TOKEN" = "0" ] \
        || die "--update-only cannot be combined with other management actions."
fi
case "$SYSTEM_ACTION" in
    status) show_agent_status; exit 0 ;;
    restart) restart_agent_services; exit 0 ;;
    reset-password) reset_panel_password; exit 0 ;;
esac
# 平台接入开关是独立的一次性操作（改配置 + 重启服务）。
# 不与 token 动作组合：设置 Token 本身就会自动启用平台接入，无需再叠加。
# 冲突检查必须放在 TOKEN_ACTION 分支之前——那边会 exit，之后就检查不到了。
if [ -n "$PLATFORM_ACTION" ] && [ -n "$TOKEN_ACTION" ]; then
    die "$(t "--platform cannot be combined with token actions." "--platform 不能与 Token 操作同时使用。")"
fi
case "$TOKEN_ACTION" in
    show) show_agent_token; exit 0 ;;
    rotate) rotate_agent_token; exit 0 ;;
esac
if [ -n "$PLATFORM_ACTION" ]; then
    set_platform_enabled "$PLATFORM_ACTION"
    exit 0
fi
if [ "$IPV6_ONESHOT_MODE" = "backup" ]; then
    b=$(backup_ipv6_config); log "$(t "IPv6 config backed up to $b" "IPv6 配置已备份至 $b")"; exit 0
elif [ "$IPV6_ONESHOT_MODE" = "rollback" ]; then
    restore_ipv6_config; exit 0
fi
if [ "$UNINSTALL" = "1" ]; then
    do_uninstall
    exit 0
fi

log "$(t "Recommended OS: Debian 13 (Trixie) for best compatibility." "推荐操作系统：使用 Debian 13 (Trixie) 以获得最佳兼容性。")"

# ────────────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────────────

cleanup_locks() {
    killall apt apt-get dpkg 2>/dev/null || true
    rm -f /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
    dpkg --configure -a 2>/dev/null || true
}

install_packages() {
    local max=3 attempt=1
    local base_packages="curl wget python3 uuid-runtime systemd-zram-generator jq chrony iptables iproute2 fdisk bc dnsutils"
    local extra_packages="podman aardvark-dns lxcfs xfsprogs"

    while [ $attempt -le $max ]; do
        log "$(t "Installing packages (attempt $attempt)..." "安装软件包 (第 $attempt 次)...")"
        if apt-get update -qq && apt-get install -y $base_packages $extra_packages; then
            log "$(t "Packages installed." "软件包安装成功。")"
            return 0
        fi
        cleanup_locks
        [ $attempt -eq $max ] && { log "$(t "Package installation failed." "软件包安装失败。")"; exit 1; }
        attempt=$((attempt + 1))
        sleep 5
    done
}

start_service() {
    systemctl daemon-reload
    local svc=$1
    systemctl is-active --quiet "$svc" || systemctl start "$svc"
    systemctl is-enabled --quiet "$svc" || systemctl enable "$svc"
    log "$(t "Service $svc started." "服务 $svc 已启动。")"
}

check_podman_version() {
    if ! command -v podman &>/dev/null; then
        log "$(t "Podman not found." "未找到 Podman。")"
        exit 1
    fi
    local version_str
    version_str=$(podman -v | awk '{print $3}')
    log "$(t "Detected Podman version: $version_str" "检测到 Podman 版本: $version_str")"

    # Compare version $version_str with 5.4.2
    local IFS=.
    local cur=($version_str)
    local min=(5 4 2)
    for i in 0 1 2; do
        if [[ ${cur[i]:-0} -lt ${min[i]} ]]; then
            log "$(t "Error: Podman version must be >= 5.4.2. Current version is $version_str" "错误: Podman 版本必须 >= 5.4.2。当前版本为 $version_str")"
            log "$(t "It is highly recommended to use Debian 13 (Trixie) to get the latest Podman." "强烈建议使用 Debian 13 (Trixie) 以获取最新版本的 Podman。")"
            exit 1
        elif [[ ${cur[i]:-0} -gt ${min[i]} ]]; then
            return 0
        fi
    done
    return 0
}

download_with_retry() {
    local url=$1 dest=$2 max=3 attempt=1 curl_status
    while [ $attempt -le $max ]; do
        log "$(t "Downloading $url (attempt $attempt)..." "下载 $url (第 $attempt 次)...")"
        if curl -fsSL -o "$dest" "$url"; then return 0; fi
        curl_status=$?
        rm -f "$dest"
        log "$(t "Download attempt failed (curl exit $curl_status): $url" "下载尝试失败（curl 退出码 $curl_status）: $url")"
        [ $attempt -eq $max ] && { log "$(t "Download failed." "下载失败。")"; return 1; }
        attempt=$((attempt + 1)); sleep 5
    done
}

# release_asset_digest 从 GitHub release API 取指定资产的 sha256 摘要。
#
# base 是 releases 下载基址（含 tag 或 latest），asset 是资产文件名。
# 返回空串表示取不到摘要（自建镜像源无此能力）——调用方应另有校验兜底。
# 用法：release_asset_digest <下载基址> <资产名>
release_asset_digest() {
    local base=$1 asset=$2 api=""
    case "$base" in
        https://github.com/*/releases/latest/download) api="https://api.github.com/repos/${base#https://github.com/}" ;;
        https://github.com/*/releases/download/*) api="https://api.github.com/repos/${base#https://github.com/}" ;;
    esac
    [ -n "$api" ] || return 0
    api="${api%/releases/*}/releases/latest"
    curl -fsSL --max-time 20 "$api" 2>/dev/null \
        | python3 -c "
import json, sys
try:
    rel = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
name = sys.argv[1]
for a in rel.get('assets') or []:
    if a.get('name') == name:
        d = a.get('digest') or ''
        if d.startswith('sha256:'):
            print(d[len('sha256:'):])
        raise SystemExit(0)
raise SystemExit(0)
" "$asset" 2>/dev/null || true
}

# verify_elf_binary 校验下载到的是目标架构的 ELF64 可执行文件。
#
# 必要性：curl 的 -f 只在 HTTP 错误码时失败，代理或镜像返回的 200 错误页
# 会被当成二进制落盘并 chmod +x 后安装。所有二进制产物（agent / netavark / rfw）
# 都必须过这一关。
#
# 用法：verify_elf_binary <文件> <amd64|arm64>
verify_elf_binary() {
    local file=$1 arch=$2 expected
    [ -s "$file" ] || { log "$(t "Downloaded file is empty: $file" "下载的文件为空：$file")"; return 1; }
    case "$arch" in
        amd64) expected=62 ;;
        arm64) expected=183 ;;
        *) log "$(t "Unknown arch for ELF verification: $arch" "ELF 校验时架构未知：$arch")"; return 1 ;;
    esac
    python3 - "$file" "$expected" <<'PYEOF'
import sys
path, expected = sys.argv[1], int(sys.argv[2])
try:
    with open(path, 'rb') as f:
        header = f.read(20)
except OSError as exc:
    raise SystemExit(f'cannot read {path}: {exc}')
if len(header) != 20 or header[:6] != b'\x7fELF\x02\x01' or int.from_bytes(header[18:20], 'little') != expected:
    raise SystemExit(f'not an ELF64 binary for {expected}')
PYEOF
}

# verify_sha256 校验文件的 sha256 摘要（可选依赖 sha256sum 或 shasum）。
# 期望摘要为空时跳过——自建镜像源未必提供摘要，此时由 ELF 校验兜底。
#
# 用法：verify_sha256 <文件> <期望摘要（可为空）>
verify_sha256() {
    local file=$1 expected=$2 actual
    [ -n "$expected" ] || return 0
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$file" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$file" | awk '{print $1}')
    else
        log "$(t "No sha256 tool available; skipping digest check for $file" "缺少 sha256 工具，跳过 $file 的摘要校验")"
        return 0
    fi
    if [ "$actual" != "$expected" ]; then
        log "$(t "sha256 mismatch for $file: got $actual, want $expected" "$file 的 sha256 不匹配：实际 $actual，期望 $expected")"
        return 1
    fi
    return 0
}

detect_arch() {
    case $(uname -m) in
        x86_64)        echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) log "$(t "Unsupported architecture: $(uname -m)" "不支持的架构: $(uname -m)")"; exit 1 ;;
    esac
}

# ── IPv6 helpers ──────────────────────────────────────────────────────────────

# 从默认路由提取网卡名。
# 不能用固定列号（awk '{print $5}'）：带 nhid 的路由（RFC 5549 nexthop 对象，
# 常见于 AWS EC2 / 部分云内核与较新 iproute2）会在 "default" 和 "via" 之间插入
# "nhid <id>"，使 dev 落到第 7 列。固定列号此时取到的是网关地址而非网卡名，
# 导致后续所有 "ip ... dev <网关地址>" 静默失败，IPv6 检测误判为无地址。
default_route_iface() {
    local family="${1:-6}"
    ip "-${family}" route show default 2>/dev/null \
        | head -1 \
        | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}'
}

ipv6_plus_one() {
    python3 -c "import ipaddress; print(str(ipaddress.IPv6Address('$1') + 1))" 2>/dev/null
}

validate_ipv6_values() {
    local mode="$1" addr="$2" subnet="$3" iface="$4"
    case "$mode" in
        none) return 0 ;;
        snat|subnet) ;;
        *) log "$(t "Invalid IPv6 mode: $mode" "无效 IPv6 模式: $mode")" >&2; return 1 ;;
    esac
    [ -n "$addr" ] || { log "$(t "IPv6 address is required." "必须填写 IPv6 地址。")" >&2; return 1; }
    [ -n "$iface" ] || { log "$(t "IPv6 interface is required." "必须填写 IPv6 网卡。")" >&2; return 1; }
    case "$iface" in
        *[!A-Za-z0-9_.:@-]*) log "$(t "Invalid interface name: $iface" "无效网卡名: $iface")" >&2; return 1 ;;
    esac
    ip link show dev "$iface" >/dev/null 2>&1 || {
        log "$(t "Interface does not exist: $iface" "网卡不存在: $iface")" >&2
        return 1
    }
    python3 - "$mode" "$addr" "$subnet" <<'PY'
import ipaddress
import sys

mode, address, subnet = sys.argv[1:]
addr = ipaddress.IPv6Address(address)
if mode == "subnet":
    if not subnet:
        raise SystemExit("IPv6 subnet is required for subnet mode")
    net = ipaddress.IPv6Network(subnet, strict=False)
    if net.prefixlen > 64:
        raise SystemExit(f"subnet mode requires /64 or larger allocation, got /{net.prefixlen}")
    if addr not in net:
        raise SystemExit(f"host address {addr} is not inside {net}")
PY
}

# 临时绑定一个子网地址并以该地址访问公网，完成后立即清理。
probe_ipv6_values() {
    local mode="$1" addr="$2" subnet="$3" iface="$4" routed="$5"
    [ "$SKIP_IPV6_PROBE" = "1" ] && {
        log "$(t "IPv6 online probe skipped by request." "已按要求跳过 IPv6 在线探测。")"
        return 0
    }

    local source_addr bind_cidr bind_iface connected=0 endpoint
    if [ "$mode" = "snat" ]; then
        source_addr="$addr"
        for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
            curl -6 --interface "$source_addr" -fsS --max-time 8 "$endpoint" >/dev/null 2>&1 && connected=1 && break
        done
    else
        source_addr=$(python3 - "$subnet" <<'PY'
import ipaddress
import sys
net = ipaddress.IPv6Network(sys.argv[1], strict=False)
candidate = net.network_address + min(0xfffe, net.num_addresses - 2)
print(candidate)
PY
)
        if ip -6 addr show | grep -Fq " $source_addr/"; then
            source_addr=$(ipv6_plus_one "$source_addr")
        fi
        if [ "$routed" = "1" ]; then
            bind_iface="lo"
            bind_cidr="$source_addr/128"
        else
            bind_iface="$iface"
            bind_cidr="$source_addr/$(echo "$subnet" | cut -d/ -f2)"
        fi
        if ip -6 addr add "$bind_cidr" dev "$bind_iface" 2>/dev/null; then
            sleep 2
            for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                curl -6 --interface "$source_addr" -fsS --max-time 8 "$endpoint" >/dev/null 2>&1 && connected=1 && break
            done
            ip -6 addr del "$bind_cidr" dev "$bind_iface" 2>/dev/null || true
        fi
    fi
    [ "$connected" = "1" ] || {
        log "$(t "IPv6 source-address connectivity probe failed." "IPv6 独立源地址连通性验证失败。")" >&2
        return 1
    }
    log "$(t "✓ IPv6 values and source-address connectivity verified." "✓ IPv6 参数及独立源地址连通性验证通过。")"
}

prompt_manual_ipv6() {
    local default_iface default_addr default_subnet default_kind answer
    default_iface=$(default_route_iface 6)
    read -rp "$(t "IPv6 uplink interface [$default_iface]: " "IPv6 上行网卡 [$default_iface]: ")" answer
    IPV6_IFACE="${answer:-$default_iface}"
    default_addr=$(ip -o -6 addr show dev "$IPV6_IFACE" scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
    read -rp "$(t "Host/gateway address from the usable prefix [$default_addr]: " "可用前缀内的宿主机/网关地址 [$default_addr]: ")" answer
    IPV6_ADDR="${answer:-$default_addr}"
    default_subnet=$(python3 - "$IPV6_ADDR" 2>/dev/null <<'PY' || true
import ipaddress, sys
print(ipaddress.IPv6Network((ipaddress.IPv6Address(sys.argv[1]), 64), strict=False))
PY
)
    read -rp "$(t "Usable IPv6 subnet CIDR [$default_subnet]: " "可用 IPv6 子网 CIDR [$default_subnet]: ")" answer
    IPV6_SUBNET="${answer:-$default_subnet}"
    case "$IPV6_IFACE" in he-*|wg*|tun*) default_kind=1 ;; *) default_kind=2 ;; esac
    printf '%s\n  1) %s\n  2) %s\n' \
        "$(t "Prefix delivery type:" "前缀下发类型:")" \
        "$(t "Routed/tunnel prefix (HE, WireGuard, provider route)" "路由/隧道前缀（HE、WireGuard、供应商静态路由）")" \
        "$(t "Native on-link /64 (requires NDP)" "原生二层 /64（需要 NDP）")"
    read -rp "[$default_kind] > " answer
    [ "${answer:-$default_kind}" = "1" ] && IPV6_ROUTED=1 || IPV6_ROUTED=0
    IPV6_MODE="subnet"
}

prompt_ipv6_strategy() {
    local choice
    printf '\n%s\n' "$(t "Container network mode:" "容器网络模式:")"
    printf '  1) %s\n' "$(t "NAT4 + public IPv6 (auto detect; recommended)" "NAT4 + 公网 IPv6（自动探测，推荐）")"
    printf '  2) %s\n' "$(t "NAT4 + manual routed/native /64" "NAT4 + 手工路由/原生 /64")"
    printf '  3) %s\n' "$(t "IPv4 NAT only" "仅 IPv4 NAT")"
    printf '  4) %s\n' "$(t "NAT4 + IPv6 SNAT (auto-detect host address)" "NAT4 + IPv6 SNAT（自动探测宿主机地址）")"
    read -rp '[1] > ' choice
    case "${choice:-1}" in
        1) IPV6_DETECT_CONFIRMED=1 ;;
        2) prompt_manual_ipv6 ;;
        3) IPV6_MODE="none" ;;
        4) IPV6_MODE="snat"; IPV6_DETECT_CONFIRMED=1 ;;
        *) die "$(t "Invalid network mode." "无效网络模式。")" ;;
    esac
}

# Detect a separately routed prefix commonly used by HE 6in4/WireGuard setups.
# Such hosts have the tunnel link address on the default interface and one
# address from the routed prefix (often /128 on lo) on another interface.  A
# temporary source-address probe proves the candidate before it is selected.
# Returns "SUBNET|HOST_ADDR" or an empty string.
detect_routed_ipv6_subnet() {
    local uplink_iface="$1" uplink_addr="$2"
    local rec cand_iface cand_cidr candidate host_addr test_addr connected

    while IFS='|' read -r cand_iface cand_cidr; do
        [ -n "$cand_iface" ] && [ "$cand_iface" != "$uplink_iface" ] || continue
        rec=$(python3 - "$cand_cidr" "$uplink_addr" <<'PY' 2>/dev/null || true
import ipaddress
import sys

iface = ipaddress.IPv6Interface(sys.argv[1])
uplink = ipaddress.IPv6Address(sys.argv[2])
# Docker/Podman bridges frequently expose ULA prefixes (fc00::/7). They may
# pass a source-address curl probe through host NAT, but they are not provider-
# routed public prefixes and must never replace the uplink's global address.
if not iface.ip.is_global:
    raise SystemExit(0)
# A /128 on lo is the usual routed-prefix gateway marker.  Derive /64 and
# prove it below; false candidates fail the independent-source probe.
prefix = min(iface.network.prefixlen, 64)
network = ipaddress.IPv6Network((iface.ip, prefix), strict=False)
if uplink in network:
    raise SystemExit(0)
test = network.network_address + 0xfffe
print(f"{network}|{iface.ip}|{test}")
PY
)
        [ -n "$rec" ] || continue
        candidate=$(echo "$rec" | cut -d'|' -f1)
        host_addr=$(echo "$rec" | cut -d'|' -f2)
        test_addr=$(echo "$rec" | cut -d'|' -f3)

        connected=0
        if ip -6 addr add "$test_addr/128" dev lo 2>/dev/null; then
            for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                if curl -6 --interface "$test_addr" -s --max-time 8 "$endpoint" >/dev/null 2>&1; then
                    connected=1
                    break
                fi
            done
            ip -6 addr del "$test_addr/128" dev lo 2>/dev/null || true
        fi
        if [ "$connected" = "1" ]; then
            echo "$candidate|$host_addr"
            return 0
        fi
    done < <(ip -o -6 addr show scope global 2>/dev/null | awk '{print $2 "|" $4}')
    return 0
}

# Returns "none" or "IFACE|ADDR|PREFIX|SUBNET|ROUTED".
detect_and_configure_ipv6() {
    log "$(t "Detecting public IPv6..." "开始检测公网 IPv6...")" >&2
    command -v python3 >/dev/null 2>&1 || {
        log "$(t "python3 is required for safe IPv6 prefix arithmetic." "安全计算 IPv6 前缀需要 python3。")" >&2
        echo "none"
        return 1
    }
    # 多厂商：ip.sb 在境内或某些时段不可达，依次尝试多个公共端点；
    # 全部失败但默认路由存在时，仍按"内部 IPv6 可用"继续，避免误判。
    local connected=0 endpoint
    for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com www.google.com; do
        if curl -6 -s --max-time 5 "$endpoint" >/dev/null 2>&1; then
            connected=1; break
        fi
    done
    if [ "$connected" = "0" ]; then
        if ip -6 route show default 2>/dev/null | grep -q .; then
            log "$(t "External IPv6 probe failed but default route exists, continuing." "公网 IPv6 探测失败但存在默认路由，继续配置。")" >&2
        else
            log "$(t "No public IPv6 detected." "未检测到公网 IPv6。")" >&2
            echo "none"; return 0
        fi
    else
        log "$(t "✓ Public IPv6 available." "✓ 公网 IPv6 可用。")" >&2
    fi

    local iface
    iface=$(default_route_iface 6)
    if [ -z "$iface" ]; then
        log "$(t "No default IPv6 route found." "未找到默认 IPv6 路由。")" >&2
        echo "none"; return 0
    fi
    log "$(t "Default IPv6 interface: $iface" "默认 IPv6 网卡: $iface")" >&2

    local ipv6_full ipv6_addr prefix_len
    # 地址选择策略：
    #   1. 优先选择静态（非 dynamic）的子网地址（排除 /128），按前缀降序排列
    #      /64 优先于 /48，因为 /48 通常是 ISP 的整体分配，/64 才是用户实际可用的子网
    #   2. 如果没有静态子网地址，回退到所有地址（含动态、含 /128）

    # 第一步：静态子网地址（不含 dynamic，不含 /128），按前缀降序——优先更具体的子网
    ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
        | grep "scope global" | grep -v "dynamic" \
        | awk '{print $2}' | grep -v '/128$' | sort -t'/' -k2 -rn | head -1)

    # 第二步：如果无静态子网，尝试所有静态地址（含 /128）
    if [ -z "$ipv6_full" ]; then
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" | grep -v "dynamic" \
            | awk '{print $2}' | sort -t'/' -k2 -rn | head -1)
    fi

    # 第三步：如果无静态地址，回退到动态地址
    if [ -z "$ipv6_full" ]; then
        log "$(t "No static IPv6 found, trying dynamic addresses..." "未找到静态 IPv6，尝试动态地址...")" >&2
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" \
            | awk '{print $2}' | grep -v '/128$' | sort -t'/' -k2 -rn | head -1)
    fi

    # 第四步：最终回退——任意全局地址
    if [ -z "$ipv6_full" ]; then
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" \
            | awk '{print $2}' | sort -t'/' -k2 -rn | head -1)
    fi

    if [ -z "$ipv6_full" ]; then
        log "$(t "No global IPv6 address found." "未找到全局 IPv6 地址。")" >&2
        echo "none"; return 0
    fi

    ipv6_addr=$(echo "$ipv6_full" | cut -d'/' -f1)
    prefix_len=$(echo "$ipv6_full" | cut -d'/' -f2)
    log "$(t "IPv6 address: $ipv6_addr/$prefix_len" "IPv6 地址: $ipv6_addr/$prefix_len")" >&2

    # Prefer a separately routed prefix over the point-to-point/tunnel link
    # prefix.  No NDP is needed for a routed prefix and the uplink address must
    # keep its original mask so the tunnel gateway remains reachable.
    local routed routed_subnet routed_host routed_prefix
    routed=$(detect_routed_ipv6_subnet "$iface" "$ipv6_addr")
    if [ -n "$routed" ]; then
        routed_subnet=$(echo "$routed" | cut -d'|' -f1)
        routed_host=$(echo "$routed" | cut -d'|' -f2)
        routed_prefix=$(echo "$routed_subnet" | cut -d'/' -f2)
        log "$(t "✓ Routed IPv6 prefix confirmed: $routed_subnet (uplink $iface)." \
            "✓ 已验证独立路由 IPv6 前缀: $routed_subnet（上行 $iface）。")" >&2
        echo "$iface|$routed_host|$routed_prefix|$routed_subnet|1"
        return 0
    fi
    # subnet 模式要求前缀 ≤ /64，且 ISP 必须真正允许子网内多个 IP 出站。
    # 有些 ISP 接口显示 /64 但做了严格 uRPF，只允许分配的单个 /128 出站。
    # /65-/127 前缀太小，/128 单地址，均使用 SNAT 模式。
    if [ "$prefix_len" -le 64 ]; then
        # 实际连通性验证：临时添加 addr+1，测试该地址能否独立出站
        local test_addr
        test_addr=$(ipv6_plus_one "$ipv6_addr")
        local subnet_ok=0
        if [ -n "$test_addr" ]; then
            ip addr add "$test_addr/$prefix_len" dev "$iface" 2>/dev/null && {
                sleep 2  # 等待 DAD 完成
                for _ep in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                    if curl -6 --interface "$test_addr" -s --max-time 8 "$_ep" >/dev/null 2>&1; then
                        subnet_ok=1; break
                    fi
                done
                ip addr del "$test_addr/$prefix_len" dev "$iface" 2>/dev/null
            }
        fi
        if [ "$subnet_ok" = "1" ]; then
            log "$(t "✓ Subnet mode confirmed (/$prefix_len), additional IPs are routable." "✓ 子网模式验证通过 (/$prefix_len)，子网内多 IP 可独立出站。")" >&2
            echo "$iface|$ipv6_addr|$prefix_len||0"
        else
            log "$(t "⚠ ISP blocks non-assigned source IPs (strict uRPF), falling back to SNAT mode." "⚠ ISP 严格过滤非分配源 IP（strict uRPF），回退到 SNAT 模式。")" >&2
            echo "$iface|$ipv6_addr|128||0"
        fi
    elif [ "$prefix_len" -eq 128 ]; then
        log "$(t "✓ Public IPv6 single address (/128) detected, SNAT mode." "✓ 检测到公网 IPv6 单地址 (/128)，使用 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128||0"
    else
        # /65-/127：前缀不足以划分子网，回退到 SNAT
        log "$(t "IPv6 prefix /$prefix_len is too small for subnet mode (need ≤/64), falling back to SNAT." "IPv6 前缀 /$prefix_len 不足以用于子网模式（需 ≤/64），回退到 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128||0"
    fi
}

if [ "$IPV6_ONESHOT_MODE" = "detect" ]; then
    detect_and_configure_ipv6
    exit 0
elif [ "$IPV6_ONESHOT_MODE" = "validate" ]; then
    if { [ -z "${IPV6_ADDR:-}" ] || [ -z "${IPV6_SUBNET:-}" ] || [ -z "${IPV6_IFACE:-}" ]; } && [ -t 0 ]; then
        prompt_manual_ipv6
    fi
    IPV6_MODE="${IPV6_MODE:-subnet}"
    validate_ipv6_values "$IPV6_MODE" "${IPV6_ADDR:-}" "${IPV6_SUBNET:-}" "${IPV6_IFACE:-}" || exit 1
    probe_ipv6_values "$IPV6_MODE" "${IPV6_ADDR:-}" "${IPV6_SUBNET:-}" "${IPV6_IFACE:-}" "$IPV6_ROUTED" || exit 1
    printf '%s|%s|%s|%s|%s\n' "$IPV6_IFACE" "$IPV6_ADDR" "$(echo "$IPV6_SUBNET" | cut -d/ -f2)" "$IPV6_SUBNET" "$IPV6_ROUTED"
    exit 0
fi

enable_bbr() {
    log "$(t "Configuring kernel parameters (BBR + forwarding)..." "配置内核参数 (BBR + 转发)...")"
    # 多厂商兼容：DO/Hetzner/部分 VPS 默认 ip_forward=0、IPv6 forwarding=0，
    # 容器/VM 出网会失败；启用 forwarding 后 IPv6 RA 默认会被拒，accept_ra=2 才能保留默认路由。
    cat > /etc/sysctl.d/99-narwhalcloud.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
fs.inotify.max_user_instances=524288
fs.inotify.max_user_watches=1048576
fs.inotify.max_queued_events=65536
kernel.pid_max=4194304
fs.file-max=2097152
fs.nr_open=2097152
vm.swappiness=100
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.forwarding=1
net.ipv6.conf.all.accept_ra=2
net.ipv6.conf.default.accept_ra=2
net.ipv6.conf.all.use_tempaddr=0
net.ipv6.conf.default.use_tempaddr=0
EOF
    sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1 \
        && log "$(t "✓ Kernel parameters configured." "✓ 内核参数已配置。")" \
        || log "$(t "Kernel parameters will take effect after reboot." "内核参数将在重启后生效。")"
}

# rootful podman 的 userns=auto 使用 "containers" 用户的 subuid/subgid 池
# （该用户是占位，不登录、无家目录，专门为 rootful podman 保留 id 范围）。
# 默认给 100 个容器的量（起始 1000000）。
ensure_subuid() {
    if ! id -u containers >/dev/null 2>&1; then
        useradd --comment "Podman userns id pool" --no-create-home --shell /usr/sbin/nologin containers
        log "$(t "✓ Created helper user containers (userns)." "✓ 已创建 userns 占位用户 containers。")"
    fi
    # 仅在 containers: 行不存在时才写入默认池。已存在（管理员自定义过起始
    # ID 或池大小）则保留原值，不静默覆盖——覆盖会打乱存量容器的 uid 映射。
    for f in /etc/subuid /etc/subgid; do
        if grep -q '^containers:' "$f" 2>/dev/null; then
            log "$(t "Kept existing userns pool in $f" "保留 $f 中已有的 userns 池配置。")"
            continue
        fi
        echo 'containers:1000000:6553600' >> "$f"
    done
    # 提示池容量：默认每个容器约占 1 uid + 1 gid（未配置额外 subordinate 范围时），
    # 池耗尽会导致新建容器失败。这里给出可支撑的近似规模，便于提前规划。
    log "$(t "Configured subuid/subgid for containers (userns). Pool supports ~6553600 containers." "已为 containers 配置 subuid/subgid（userns）。该池约可支撑 6553600 个容器。")"
}

configure_journald() {
    log "$(t "Configuring systemd-journald limits..." "配置 systemd-journald 日志限制...")"
    cat > /etc/systemd/journald.conf <<EOF
[Journal]
SystemMaxUse=200M
RuntimeMaxUse=50M
SystemMaxFileSize=50M
RateLimitIntervalSec=10s
RateLimitBurst=1000
ForwardToSyslog=no
EOF
    systemctl restart systemd-journald
    log "$(t "✓ systemd-journald configured and restarted." "✓ systemd-journald 已配置并重启。")"
}

create_xfs_disk() {
    local disk=$1 size=$2 mount=$3 opts=$4
    if [ ! -f "$disk" ]; then
        log "$(t "Creating XFS disk $disk ($size)..." "创建 XFS 磁盘 $disk ($size)...")"
        # 在临时文件中完全预分配，失败时不留下半成品，也不再用 dd 继续填满根分区。
        local creating="${disk}.creating"
        rm -f -- "$creating"
        if ! fallocate -l "$size" "$creating" 2>/dev/null; then
            rm -f -- "$creating"
            log "$(t "Unable to allocate $size; choose a smaller data disk." "无法分配 $size，请选择更小的数据盘。")"
            return 1
        fi
        if ! mkfs.xfs -f "$creating"; then
            rm -f -- "$creating"
            return 1
        fi
        mv "$creating" "$disk"
    else
        log "$(t "$disk already exists, skipping." "$disk 已存在，跳过。")"
    fi
    mkdir -p "$mount"

    # 配置 udev 规则以开启 Direct IO，解决双重缓存导致的内存压力和挂载进程异常问题
    log "$(t "Configuring udev rules for Direct IO..." "配置 udev 规则以开启 Direct IO...")"
    cat > /etc/udev/rules.d/99-loop-directio.rules <<EOF
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="loop*", ATTR{loop/backing_file}=="$disk", ATTR{loop/direct_io}="1"
EOF
    udevadm control --reload-rules && udevadm trigger

    if ! mountpoint -q "$mount"; then
        log "$(t "Mounting $disk..." "正在挂载 $disk...")"
        mount -o "$opts" "$disk" "$mount"
        # 立即尝试对当前设备开启 Direct IO
        local loop_dev
        loop_dev=$(mount | grep "$mount" | awk '{print $1}')
        if [[ "$loop_dev" == /dev/loop* ]]; then
            echo 1 > "/sys/block/${loop_dev#/dev/}/loop/direct_io" 2>/dev/null || true
        fi
    fi
    grep -q "$disk" /etc/fstab || echo "$disk $mount xfs $opts 0 0" >> /etc/fstab
}

recommended_podman_data_size() {
    local avail_mb target
    avail_mb=$(df -Pm / | awk 'NR==2 {print $4}')
    target=$(( (avail_mb - 2048) / 1024 ))
    [ "$target" -gt 20 ] && target=20
    [ "$target" -ge 2 ] || return 1
    printf '%sG\n' "$target"
}

validate_podman_data_size() {
    local value="$1" amount unit requested_mb avail_mb
    case "$value" in
        *[Gg]) amount="${value%[Gg]}"; unit=G ;;
        *[Mm]) amount="${value%[Mm]}"; unit=M ;;
        *) return 1 ;;
    esac
    case "$amount" in ''|*[!0-9]*) return 1 ;; esac
    [ "$amount" -gt 0 ] || return 1
    if [ "$unit" = G ]; then requested_mb=$((amount * 1024)); else requested_mb=$amount; fi
    [ "$requested_mb" -ge 1024 ] || return 1
    avail_mb=$(df -Pm / | awk 'NR==2 {print $4}')
    [ $((requested_mb + 1536)) -le "$avail_mb" ] || {
        log "$(t "Insufficient root filesystem space: requested $value, available ${avail_mb}M; keep at least 1536M free." \
            "根分区空间不足：请求 $value，可用 ${avail_mb}M；安装后至少需保留 1536M。")"
        return 1
    }
}

prompt_podman_options() {
    local recommended choice custom mirror
    recommended=$(recommended_podman_data_size) \
        || die "$(t "At least 3.5 GiB free space is required for a Podman installation." "Podman 安装至少需要约 3.5 GiB 可用空间。")"
    printf '\n%s\n' "$(t "Podman XFS data disk size:" "Podman XFS 数据盘容量:")"
    printf '  1) %s (%s)\n' "$(t "Recommended for current disk" "按当前磁盘推荐")" "$recommended"
    printf '  2) 5G\n  3) 10G\n  4) 20G\n  5) %s\n' "$(t "Custom size" "自定义容量")"
    read -rp '[1] > ' choice
    case "${choice:-1}" in
        1) PODMAN_DATA_SIZE="$recommended" ;;
        2) PODMAN_DATA_SIZE=5G ;;
        3) PODMAN_DATA_SIZE=10G ;;
        4) PODMAN_DATA_SIZE=20G ;;
        5) read -rp "$(t "Custom size (for example 8G): " "自定义容量（例如 8G）: ")" custom; PODMAN_DATA_SIZE="$custom" ;;
        *) die "$(t "Invalid data disk selection." "无效的数据盘选项。")" ;;
    esac
    validate_podman_data_size "$PODMAN_DATA_SIZE" \
        || die "$(t "Invalid or oversized Podman data disk value: $PODMAN_DATA_SIZE" "Podman 数据盘容量无效或超过可用空间: $PODMAN_DATA_SIZE")"
    read -rp "$(t "docker.io registry mirror (blank = direct): " "docker.io 镜像加速地址（留空直连）: ")" mirror
    PODMAN_REGISTRY_MIRROR="${mirror:-$PODMAN_REGISTRY_MIRROR}"
    [ -z "$mirror" ] || PODMAN_REGISTRY_MIRROR_EXPLICIT=1
}

configure_podman_registry_mirror() {
    if [ -z "$PODMAN_REGISTRY_MIRROR" ]; then
        if [ "${PODMAN_REGISTRY_MIRROR_EXPLICIT:-0}" = "1" ]; then
            rm -f -- /etc/containers/registries.conf.d/99-runman-mirror.conf
            log "$(t "Podman docker.io mirror cleared; direct/default registry settings will be used." "已清除 Podman docker.io 镜像加速，将使用直连/系统默认配置。")"
        fi
        return 0
    fi
    local raw="$PODMAN_REGISTRY_MIRROR" location insecure=false
    case "$raw" in
        http://*) insecure=true; location="${raw#http://}" ;;
        https://*) location="${raw#https://}" ;;
        *) location="$raw" ;;
    esac
    location="${location%/}"
    case "$location" in ''|*[!A-Za-z0-9._:/-]*) die "$(t "Invalid Podman registry mirror: $raw" "Podman 镜像加速地址无效: $raw")" ;; esac
    mkdir -p /etc/containers/registries.conf.d
    cat > /etc/containers/registries.conf.d/99-runman-mirror.conf <<EOF
[[registry]]
prefix = "docker.io"
location = "docker.io"

[[registry.mirror]]
location = "$location"
insecure = $insecure
EOF
    log "$(t "Podman docker.io mirror configured: $location" "Podman docker.io 镜像加速已配置: $location")"
}

install_podman_forwarding_compat() {
    # Docker sets the shared iptables FORWARD policy to DROP. Netavark's nft
    # base chain cannot override an earlier DROP, so Podman traffic dies when
    # Docker and Podman coexist. DOCKER-USER is Docker's supported insertion
    # point; limit exceptions strictly to the Runman network subnets.
    cat > /usr/local/sbin/runman-podman-forwarding <<'EOF'
#!/usr/bin/env bash
set -u

apply_rule() {
    local tool="$1"; shift
    "$tool" -w -C DOCKER-USER "$@" 2>/dev/null || "$tool" -w -I DOCKER-USER 1 "$@"
}
delete_rule() {
    local tool="$1"; shift
    while "$tool" -w -C DOCKER-USER "$@" 2>/dev/null; do
        "$tool" -w -D DOCKER-USER "$@" 2>/dev/null || break
    done
}

mode="${1:-apply}"
if iptables -w -S DOCKER-USER >/dev/null 2>&1; then
    if [ "$mode" = remove ]; then
        delete_rule iptables -s 10.91.0.0/20 -j ACCEPT
        delete_rule iptables -d 10.91.0.0/20 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    else
        apply_rule iptables -d 10.91.0.0/20 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
        apply_rule iptables -s 10.91.0.0/20 -j ACCEPT
    fi
fi
if ip6tables -w -S DOCKER-USER >/dev/null 2>&1; then
    if [ "$mode" = remove ]; then
        delete_rule ip6tables -s fd91:cafe:cafe:10::/64 -j ACCEPT
        delete_rule ip6tables -d fd91:cafe:cafe:10::/64 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    else
        apply_rule ip6tables -d fd91:cafe:cafe:10::/64 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
        apply_rule ip6tables -s fd91:cafe:cafe:10::/64 -j ACCEPT
    fi
fi
EOF
    chmod 700 /usr/local/sbin/runman-podman-forwarding
    cat > /etc/systemd/system/runman-podman-forwarding.service <<'EOF'
[Unit]
Description=Runman Podman forwarding compatibility for Docker hosts
After=network-online.target docker.service podman.socket
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/runman-podman-forwarding apply
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
    start_service runman-podman-forwarding
    log "$(t "Podman/Docker forwarding compatibility applied." "已应用 Podman/Docker 共存转发兼容规则。")"
}

download_agent() {
    local arch=$1
    mkdir -p "$AGENT_BIN_DIR"

    # 调试模式：检查本地文件
    if [ "$UPDATE_ONLY" != "1" ] && [ -f "./runman-agent-linux-$arch" ]; then
        log "$(t "Found local agent binary: ./runman-agent-linux-$arch (debug mode)" "找到本地 agent 二进制: ./runman-agent-linux-$arch (调试模式)")"
        cp "./runman-agent-linux-$arch" "$AGENT_BINARY.new"
        chmod +x "$AGENT_BINARY.new"
        [ "${2:-}" = "stage" ] || mv "$AGENT_BINARY.new" "$AGENT_BINARY"
        log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY (from local)" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY (来自本地)")"
        return 0
    fi

    # 生产模式：从远程下载
    log "$(t "Downloading NarwhalCloud Agent ($arch)..." "下载 NarwhalCloud Agent ($arch)...")"
    download_with_retry "$DOWNLOAD_BASE/runman-agent-linux-$arch" "$AGENT_BINARY.new"
    verify_elf_binary "$AGENT_BINARY.new" "$arch" \
        || die "$(t "Downloaded Agent failed ELF/architecture verification." "下载的 Agent 未通过 ELF/架构校验。")"
    verify_sha256 "$AGENT_BINARY.new" "$(release_asset_digest "$DOWNLOAD_BASE" "runman-agent-linux-$arch")" \
        || die "$(t "Downloaded Agent failed sha256 verification." "下载的 Agent 未通过 sha256 校验。")"
    chmod +x "$AGENT_BINARY.new"
    if [ "${2:-}" = "stage" ]; then
        log "$(t "✓ Agent download staged and architecture checked." "✓ Agent 已下载暂存并通过架构检查。")"
    else
        mv "$AGENT_BINARY.new" "$AGENT_BINARY"
        log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY")"
    fi
}

# write_service_file
write_service_file() {
    local virt_type="${1:-podman}"
    mkdir -p "$AGENT_DATA_DIR"

    # Determine After dependencies based on virt type
    local after_target="network-online.target"
    if [ "$virt_type" = "podman" ]; then
        after_target="network-online.target podman.socket"
    fi

    cat > "/etc/systemd/system/$AGENT_SERVICE.service" <<EOF
[Unit]
Description=NarwhalCloud Agent
After=$after_target
Wants=network-online.target

[Service]
Type=simple
User=root
OOMScoreAdjust=-999
KillMode=process
WorkingDirectory=$AGENT_BIN_DIR
ExecStart=$AGENT_BINARY --config $AGENT_CONFIG_FILE
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# write_config_file - Generate the main configuration file
write_config_file() {
    local virt_type="${1:-podman}"
    local ipv6_mode="${2:-none}"
    local ipv6_subnet="${3:-}"
    local ipv6_addr="${4:-}"
    local ipv6_iface="${5:-}"
    local ndp_iface="${6:-}"
    local ndp_subnets="${7:-}"
    local ndp_network="${8:-}"
    local web_user="${9:-admin}"
    local web_pass="${10:-}"
    local agent_token="${11:-}"

    # 装完默认是否接入平台：只有人显式提供了 Token 才置 true。
    # 安装器在未提供时会自动生成 Token（写进配置供日后注册），但自动生成
    # 不代表这台机器要接平台，默认按独立模式处理；否则装完就会对着默认地址
    # 反复试连并刷 401 日志。需要接入时用 `--platform on` 或面板开关打开。
    local platform_enabled=false
    if [ "$TOKEN_EXPLICIT" = "1" ] && [ -n "$agent_token" ]; then
        platform_enabled=true
    fi

    mkdir -p "$AGENT_CONFIG_DIR"
    # 目录同时存放二进制、配置、数据库与 install.sh，收紧为仅属主可进入，
    # 避免其它本地用户列出目录内容或读到数据库。
    chmod 700 "$AGENT_CONFIG_DIR"

    cat > "$AGENT_CONFIG_FILE" <<EOF
{
    "db": "$AGENT_DB",
    "web": ":$AGENT_WEB_PORT",
    "virt_type": "$virt_type",
    "token": "",
    "platform_enabled": $platform_enabled,
    "monitor_nic": "",
    "monitor_disk": "/",
    "web_user": "$web_user",
    "web_pass_hash": "$web_pass",
    "host": "",
    "max_port_forward": 20,
    "ipv6_mode": "$ipv6_mode",
    "ipv6_subnet": "$ipv6_subnet",
    "ipv6_addr": "$ipv6_addr",
    "ipv6_iface": "$ipv6_iface",
    "ndp_iface": "$ndp_iface",
    "ndp_subnets": "$ndp_subnets",
    "ndp_network": "$ndp_network",
    "ipv6_backup_dir": "$BACKUP_DIR",
    "rfw_addr": "$RFW_API_ADDR",
    "update_repo": "tl0502/runman-agent",
    "netavark_base": "$NETAVARK_BASE"
}
EOF
    if [ -n "$agent_token" ]; then
        jq --arg token "$agent_token" '.token = $token' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp"
        mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE"
    fi
    chmod 600 "$AGENT_CONFIG_FILE"
    log "$(t "✓ Configuration file written to $AGENT_CONFIG_FILE" "✓ 配置文件已写入 $AGENT_CONFIG_FILE")"
}

rfw_api_ready() {
    local attempts="${1:-10}" i
    for ((i = 0; i < attempts; i++)); do
        curl -sf "http://$RFW_API_ADDR/api/rules" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

enable_rfw_skb_mode() {
    local unit="/etc/systemd/system/rfw.service"
    [ -f "$unit" ] || return 1
    grep -Fq -- "--xdp-mode skb" "$unit" && return 0
    sed -i '/^ExecStart=/ s/$/ --xdp-mode skb/' "$unit"
    systemctl daemon-reload
    systemctl restart rfw
}

install_rfw() {
    local mode="${1:-0}" # 0: normal (prompt), 1: force (no prompt), 2: update-only (no prompt, only if exists)
    local ARCH
    ARCH=$(detect_arch)

    if [ "$mode" = "0" ]; then
        read -rp "$(t "Install rfw firewall? [Y/n]: " "是否安装 rfw 防火墙? [Y/n]: ")" _rfw
        [[ ! "${_rfw:-Y}" =~ ^[Yy]$ ]] && { log "$(t "Skipping rfw installation." "跳过 rfw 安装。")"; return 0; }
    elif [ "$mode" = "2" ]; then
        [ -f "$RFW_BIN_DIR/rfw" ] || return 0
    fi

    log "$(t "Installing/Updating rfw..." "正在安装/更新 rfw...")"
    mkdir -p "$RFW_BIN_DIR"

    # 调试模式：检查本地文件
    if [ -f "./rfw-$ARCH" ]; then
        log "$(t "Found local rfw binary: ./rfw-$ARCH (debug mode)" "找到本地 rfw 二进制: ./rfw-$ARCH (调试模式)")"
        cp "./rfw-$ARCH" "$RFW_BIN_DIR/rfw"
        chmod +x "$RFW_BIN_DIR/rfw"
    else
        # 生产模式：从远程下载 (即使已存在也更新)
        if download_with_retry "$RFW_BASE/rfw-$ARCH" "$RFW_BIN_DIR/rfw.new"; then
            if verify_elf_binary "$RFW_BIN_DIR/rfw.new" "$ARCH" \
               && verify_sha256 "$RFW_BIN_DIR/rfw.new" "$(release_asset_digest "$RFW_BASE" "rfw-$ARCH")"; then
            chmod +x "$RFW_BIN_DIR/rfw.new"
            systemctl is-active --quiet rfw && systemctl stop rfw
            mv "$RFW_BIN_DIR/rfw.new" "$RFW_BIN_DIR/rfw"
            fi
        else
             log "$(t "Warning: rfw download failed." "警告: rfw 下载失败。")"
             [ -f "$RFW_BIN_DIR/rfw" ] || return 0
        fi
    fi

    if [ ! -f "/etc/systemd/system/rfw.service" ]; then
        # 过滤掉 lo 及虚拟网桥
        mapfile -t interfaces < <(ip -o link show | awk -F': ' '{print $2}' \
            | grep -v "^lo$" | grep -v "^narwhal-net$" | grep -v "^br-" \
            | grep -v "^podman" | grep -v "@")

        if [ ${#interfaces[@]} -eq 0 ]; then
            log "$(t "Error: no available WAN interfaces found." "错误: 未找到可用的 WAN 网卡。")"
            return 1
        elif [ ${#interfaces[@]} -eq 1 ]; then
            SEL_IFACE="${interfaces[0]}"
            log "$(t "Only one WAN interface found, using: $SEL_IFACE" "只找到一个 WAN 网卡，使用: $SEL_IFACE")"
        elif [ "$mode" = "1" ] || [ "$NON_INTERACTIVE" = "1" ]; then
            # --install-rfw promises a no-prompt forced install. Prefer the
            # default IPv4 uplink so tunnel/bridge interfaces are not selected.
            SEL_IFACE=$(default_route_iface 4)
            if [ -z "$SEL_IFACE" ] || ! printf '%s\n' "${interfaces[@]}" | grep -Fxq "$SEL_IFACE"; then
                SEL_IFACE="${interfaces[0]}"
            fi
            log "$(t "Non-interactive rfw uplink: $SEL_IFACE" "无交互 rfw 上行网卡: $SEL_IFACE")"
        else
            echo "$(t "Available WAN interfaces:" "可用 WAN 网卡：")"
            for i in "${!interfaces[@]}"; do echo "  $((i+1)). ${interfaces[$i]}"; done
            while true; do
                read -rp "$(t "Select interface number (1-${#interfaces[@]}): " "请选择网卡编号 (1-${#interfaces[@]}): ")" choice
                [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#interfaces[@]}" ] && break
                echo "$(t "Invalid choice, please try again." "无效选择，请重新输入。")"
            done
            SEL_IFACE="${interfaces[$((choice-1))]}"
            log "$(t "Selected interface: $SEL_IFACE" "选择网卡: $SEL_IFACE")"
        fi

        cat > /etc/systemd/system/rfw.service <<EOF
[Unit]
Description=RFW eBPF Firewall
After=network.target

[Service]
Type=simple
User=root
Environment=RUST_LOG=info
WorkingDirectory=$RFW_BIN_DIR
ExecStart=$RFW_BIN_DIR/rfw --iface $SEL_IFACE --api-addr $RFW_API_ADDR
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        log "$(t "✓ rfw service file written. API listens on $RFW_API_ADDR (local only)." "✓ rfw 服务文件已写入，API 监听 $RFW_API_ADDR（仅本地）。")"
    fi
    
    start_service rfw

    # The API starts only after eBPF/XDP attaches. Some cloud NICs (notably
    # ARM virtual NICs) reject the default native mode but support generic SKB.
    _rfw_ready=0
    if rfw_api_ready 10; then
        _rfw_ready=1
    elif journalctl -u rfw --no-pager -n 60 2>/dev/null | grep -Eiq 'XDP.*(attach|附加)|attach.*XDP'; then
        log "$(t "rfw XDP native mode is unsupported; retrying in SKB mode." "rfw 默认 XDP 模式不受该网卡支持，正在改用兼容的 SKB 模式重试。")"
        if enable_rfw_skb_mode && rfw_api_ready 15; then
            _rfw_ready=1
            log "$(t "✓ rfw API is ready in SKB mode." "✓ rfw API 已通过 SKB 模式就绪。")"
        fi
    fi

    if [ "$_rfw_ready" = "1" ]; then
        # Check if rules already exist
        _current_rules=$(curl -sf "http://$RFW_API_ADDR/api/rules")
        if [ "$_current_rules" != "[]" ] && [ -n "$_current_rules" ]; then
            log "$(t "rfw rules already exist, skipping default rules." "rfw 规则已存在，跳过默认规则安装。")"
        else
            log "$(t "Installing default rfw rules..." "安装默认 rfw 规则...")"
            _geoip_countries='["CN"]'

            # Block inbound http/socks/fet from high-risk regions
            for _proto in http socks fet; do
                curl -sf -X POST "http://$RFW_API_ADDR/api/rules" \
                    -H "Content-Type: application/json" \
                    -d "{\"priority\":100,\"direction\":\"in\",\"protocol\":\"$_proto\",\"action\":\"block\",\"port_start\":0,\"enabled\":true,\"ip_type\":\"geoip\",\"countries\":$_geoip_countries}" \
                    >/dev/null
            done

            # Block outbound SMTP ports (anti-spam)
            for _port in 25 465 587; do
                curl -sf -X POST "http://$RFW_API_ADDR/api/rules" \
                    -H "Content-Type: application/json" \
                    -d "{\"priority\":100,\"direction\":\"out\",\"protocol\":\"tcp\",\"action\":\"block\",\"port_start\":$_port,\"enabled\":true,\"ip_type\":\"any\"}" \
                    >/dev/null
            done
            log "$(t "✓ Default rfw rules installed/verified." "✓ 默认 rfw 规则已安装/确认。")"
        fi
    else
        log "$(t "Warning: rfw API not ready, skipping default rules. Check: journalctl -u rfw --no-pager -n 80" "警告：rfw API 未就绪，跳过默认规则安装。请检查：journalctl -u rfw --no-pager -n 80")"
    fi

    log "$(t "✓ rfw firewall installed/updated." "✓ rfw 防火墙已安装/更新。")"
}

# 从已存在的配置文件读取虚拟化类型（更新流程判断需要刷新哪类镜像）
detect_installed_virt_type() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 0
    grep -o '"virt_type"[[:space:]]*:[[:space:]]*"[^"]*"' "$AGENT_CONFIG_FILE" 2>/dev/null \
        | head -n1 | sed 's/.*:[[:space:]]*"//; s/"$//' || true
}

prompt_existing_image_update() {
    local choice value
    printf '\n%s\n' "$(t "Podman image management:" "Podman 镜像管理:")"
    printf '  1) %s\n' "$(t "Refresh the built-in Debian and Alpine images" "刷新内置 Debian 与 Alpine 镜像")"
    printf '  2) %s\n' "$(t "Set docker.io registry mirror and refresh" "设置 docker.io 镜像加速并刷新")"
    printf '  3) %s\n' "$(t "Clear installer-managed registry mirror" "清除安装脚本管理的镜像加速")"
    read -rp '[1] > ' choice
    case "${choice:-1}" in
        1) ;;
        2) read -rp "$(t "Registry mirror (host[:port][/prefix] or URL): " "镜像加速地址（host[:port][/prefix] 或 URL）: ")" value; [ -n "$value" ] || die "$(t "Mirror cannot be empty." "镜像加速地址不能为空。")"; PODMAN_REGISTRY_MIRROR="$value"; PODMAN_REGISTRY_MIRROR_EXPLICIT=1 ;;
        3) PODMAN_REGISTRY_MIRROR=""; PODMAN_REGISTRY_MIRROR_EXPLICIT=1 ;;
        *) die "$(t "Invalid image operation." "无效的镜像操作。")" ;;
    esac
}

# ── Update flow ───────────────────────────────────────────────────────────────

# Never interpret an incomplete/custom original installation as a fresh install.
preflight_existing_install() {
    [ "$(id -u)" = "0" ] || die "Update requires root."
    for dep in jq python3 curl flock; do
        command -v "$dep" >/dev/null 2>&1 || die "Update requires $dep; install it first."
    done
    [ -f "$AGENT_BINARY" ] && [ -f "$AGENT_CONFIG_FILE" ] \
        || die "Incomplete/nonstandard installation: expected $AGENT_BINARY and $AGENT_CONFIG_FILE. No fresh installation attempted."
    jq -e 'type == "object"' "$AGENT_CONFIG_FILE" >/dev/null \
        || die "Invalid existing config.json; update aborted."
    local virt command_line
    virt=$(detect_installed_virt_type)
    case "$virt" in
        podman) ;;
        incus|cloudhv) die "Existing backend is $virt; this fork only maintains Podman. Refusing to touch it." ;;
        *) die "Unknown existing virt_type; update aborted." ;;
    esac
    [ -z "$VIRT_TYPE_REQUESTED" ] || [ "$VIRT_TYPE_REQUESTED" = "$virt" ] \
        || die "An update cannot switch virtualization backends. Existing backend: $virt"
    [ "$UPDATE_NETWORK_REQUESTED" = "0" ] && [ -z "${IPV6_MODE:-}" ] \
        && [ -z "${IPV6_SUBNET:-}" ] && [ -z "${IPV6_ADDR:-}" ] && [ -z "${IPV6_IFACE:-}" ] \
        || die "An update preserves existing networking; configure network changes separately."
    if [ "$UPDATE_ONLY" = "1" ]; then
        [ "$IMAGE_MENU" = "0" ] && [ "$FORCE_IMAGE_REFRESH" != "1" ] \
            && [ "$INSTALL_RFW_FORCE" != "1" ] && [ -z "$INITIAL_TOKEN" ] \
            && [ "$PODMAN_REGISTRY_MIRROR_EXPLICIT" != "1" ] \
            || die "--update-only cannot be combined with Token, image or firewall changes."
    fi
    command_line=$(systemctl show "$AGENT_SERVICE" -p ExecStart --value)
    [[ "$command_line" == *"$AGENT_BINARY"* && "$command_line" == *"$AGENT_CONFIG_FILE"* ]] \
        || die "Nonstandard service ExecStart/config path; manual migration is required."
    AGENT_DB=$(jq -r '.db // empty' "$AGENT_CONFIG_FILE")
    AGENT_DB="${AGENT_DB:-$AGENT_CONFIG_DIR/agent.db}"
    [[ "$AGENT_DB" = /* ]] && [ -f "$AGENT_DB" ] \
        || die "Existing database missing or not an absolute path: $AGENT_DB; update aborted."
}

backup_existing_install() {
    mkdir -p "$BACKUP_DIR"
    UPGRADE_BACKUP=$(mktemp -d "$BACKUP_DIR/upgrade-$(date +%Y%m%d-%H%M%S)-XXXXXX")
    chmod 700 "$UPGRADE_BACKUP"
    cp -p "$AGENT_BINARY" "$UPGRADE_BACKUP/narwhal-agent"
    cp -p "$AGENT_CONFIG_FILE" "$UPGRADE_BACKUP/config.json"
    systemctl cat "$AGENT_SERVICE" > "$UPGRADE_BACKUP/service.txt"
    # SQLite's online backup API includes committed WAL data; copying a live
    # agent.db alone can silently lose VM records and port-forward rules.
    python3 - "$AGENT_DB" "$UPGRADE_BACKUP/agent.db" <<'PY'
import pathlib
import sqlite3
import sys
with sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri() + '?mode=ro', uri=True, timeout=30) as source:
    with sqlite3.connect(sys.argv[2]) as target:
        source.backup(target)
        if target.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
            raise RuntimeError('Database backup failed integrity check')
PY
    chmod 600 "$UPGRADE_BACKUP/config.json" "$UPGRADE_BACKUP/agent.db" "$UPGRADE_BACKUP/service.txt"
    log "$(t "Pre-upgrade backup (keep private):" "升级前备份（请妥善保密）:") $UPGRADE_BACKUP"
}

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ] || [ -f "$AGENT_CONFIG_FILE" ]; then
    log "$(t "Existing NarwhalCloud Agent detected, updating..." "检测到已安装的 NarwhalCloud Agent，执行更新流程...")"
    preflight_existing_install
    exec 9>/run/lock/narwhal-agent-install.lock
    flock -n 9 || die "Another Agent installation/update is already running."
    backup_existing_install
    ARCH=$(detect_arch)
    # Finish downloading before changing config or replacing the running binary.
    download_agent "$ARCH" stage

    _installed_virt=$(detect_installed_virt_type)
    if [ "$IMAGE_MENU" = "1" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        prompt_existing_image_update
    fi

    if [ -n "$INITIAL_TOKEN" ]; then
        write_agent_token "$INITIAL_TOKEN"
        log "$(t "✓ Integration Token updated from command line/environment." "✓ 已通过命令行/环境变量更新对接 Token。")"
    fi

    # 配置迁移：为旧配置补齐新增字段（保留已有值，仅补充缺省），避免升级后新功能无法生效
    if command -v jq >/dev/null 2>&1 && [ -f "$AGENT_CONFIG_FILE" ]; then
        # 空字符串也视为未设置：旧配置里 update_repo 可能已被写成 ""，
        # 只用 // 无法覆盖（// 只对 null/false 生效），必须显式判空。
        jq --arg repo "tl0502/runman-agent" --arg nb "$NETAVARK_BASE" '. + {
               update_repo:     (if ((.update_repo // "") == "") then $repo else .update_repo end),
               netavark_base:   (if ((.netavark_base // "") == "") then $nb else .netavark_base end),
               ipv6_backup_dir: (.ipv6_backup_dir // "/var/lib/narwhal-agent/backups")
             }' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp" \
            && mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE" \
            && chmod 600 "$AGENT_CONFIG_FILE"
    fi

    ARCH=$(detect_arch)
    if [ "$UPDATE_ONLY" != "1" ] && [ "$_installed_virt" = "podman" ]; then
        check_podman_version
    fi

    # 重新应用 sysctl，确保旧安装升级后也获得最新内核参数（forwarding/accept_ra 等）
    if [ "$UPDATE_ONLY" != "1" ]; then
        enable_bbr
        configure_journald
    fi

    mv "$AGENT_BINARY.new" "$AGENT_BINARY"

    # Update netavark
    if [ "$UPDATE_ONLY" != "1" ] && [ "$_installed_virt" = "podman" ] && [ -f "/usr/libexec/podman/netavark" ]; then
        if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark.new"; then
            if verify_elf_binary "/usr/libexec/podman/netavark.new" "$ARCH" \
               && verify_sha256 "/usr/libexec/podman/netavark.new" "$(release_asset_digest "$NETAVARK_BASE" "netavark-new-$ARCH")"; then
                chmod +x /usr/libexec/podman/netavark.new
                mv /usr/libexec/podman/netavark.new /usr/libexec/podman/netavark
                log "$(t "✓ Custom netavark updated." "✓ 自定义 netavark 已更新。")"
            else
                rm -f /usr/libexec/podman/netavark.new
                log "$(t "netavark download failed verification; keeping the existing binary." "netavark 下载未通过校验，保留原有二进制。")"
            fi
        fi
        if [ -f "/usr/lib/podman/aardvark-dns" ] && [ ! -f "/usr/libexec/podman/aardvark-dns" ]; then
            ln -sf /usr/lib/podman/aardvark-dns /usr/libexec/podman/aardvark-dns
        fi
    fi

    if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null; then
        systemctl restart "$AGENT_SERVICE" \
            || die "Agent restart failed. Backup: $UPGRADE_BACKUP; inspect journalctl -u $AGENT_SERVICE"
        sleep 3
        systemctl is-active --quiet "$AGENT_SERVICE" \
            || die "Agent did not remain active. Backup: $UPGRADE_BACKUP; inspect journalctl -u $AGENT_SERVICE"
    else
        log "$(t "Agent was stopped; leaving it stopped." "Agent 原本已停止，本次保持停止状态。")"
    fi
    log "$(t "✓ NarwhalCloud Agent updated." "✓ NarwhalCloud Agent 已更新。")"
    if [ "$UPDATE_ONLY" = "1" ]; then
        log "$(t "Agent-only update complete; backend, images and host networking unchanged." "仅 Agent 更新完成；后端、镜像与宿主机网络未改动。")"
        exit 0
    fi

    # Refresh base images for Podman (the only backend this fork maintains).
    # 已运行的实例使用的是镜像副本，替换基础镜像只影响之后新建的实例。
    VIRT_TYPE="$_installed_virt"
    ensure_podman_network_from_config
    configure_podman_registry_mirror
    install_podman_forwarding_compat
    if [ "$FORCE_IMAGE_REFRESH" = "1" ]; then
        for image in "docker.io/narwhalcloud/debian:podman" "docker.io/narwhalcloud/alpine:podman"; do
            podman pull "$image" || die "$(t "Failed to refresh Podman image: $image" "Podman 镜像刷新失败: $image")"
        done
    fi

    # Update rfw if binary exists or force install requested
    if [ "$INSTALL_RFW_FORCE" = "1" ]; then
        install_rfw 1
    else
        install_rfw 2
    fi

    IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
    echo ""
    log "========================================"
    log "$(t "✓ Update complete!" "✓ 更新完成！")"
    log "$(t "IP: $IP" "IP: $IP")"
    log "========================================"
    exit 0
fi

# ── Fresh install ─────────────────────────────────────────────────────────────

[ "$UPDATE_ONLY" != "1" ] || die "No standard Agent installation found; --update-only will not perform a fresh installation."
[ "$IMAGE_MENU" = "1" ] && die "$(t "Agent is not installed; use menu option 1 first." "Agent 尚未安装，请先使用菜单选项 1 安装。")"

log "$(t "Starting fresh installation..." "开始全新安装流程...")"


# ── Virtualization type selection ──────────────────────────────────────────────

# 本 fork 只维护 Podman 后端：不再询问，也不接受其它类型（--virt 已做前置校验）。
VIRT_TYPE="podman"
log "$(t "Backend: podman (the only backend this fork maintains)" "后端: podman（本 fork 只维护 Podman 后端）")"

if [ "$VIRT_TYPE" = "podman" ]; then
    if [ ! -f /xfs_disk.img ]; then
        if [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
            prompt_podman_options
        else
            [ -n "$PODMAN_DATA_SIZE" ] || PODMAN_DATA_SIZE=$(recommended_podman_data_size) \
                || die "$(t "At least 3.5 GiB free space is required for a Podman installation." "Podman 安装至少需要约 3.5 GiB 可用空间。")"
            validate_podman_data_size "$PODMAN_DATA_SIZE" \
                || die "$(t "Invalid or oversized Podman data disk value: $PODMAN_DATA_SIZE" "Podman 数据盘容量无效或超过可用空间: $PODMAN_DATA_SIZE")"
        fi
    elif [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        read -rp "$(t "docker.io registry mirror (blank = keep direct/default): " "docker.io 镜像加速地址（留空保持直连/现有配置）: ")" _podman_mirror
        [ -z "$_podman_mirror" ] || PODMAN_REGISTRY_MIRROR="$_podman_mirror"
    fi
fi

# ── Installation ───────────────────────────────────────────────────────────────

install_packages
# 在任何受管系统文件变更之前保存唯一的安装基线。后续手工 --backup-ipv6
# 只更新 latest，不覆盖 install-origin，确保 --uninstall 永远恢复到安装前。
if [ ! -s "$BACKUP_DIR/install-origin" ]; then
    if b=$(backup_ipv6_config); then
        echo "$b" > "$BACKUP_DIR/install-origin"
        log "$(t "System config baseline saved to $b" "系统配置安装前基线已保存至 $b")"
    fi
fi
if [ "$VIRT_TYPE" = "podman" ]; then
    check_podman_version
    ensure_subuid
fi
enable_bbr
configure_journald
start_service chrony
if [ "$VIRT_TYPE" = "podman" ]; then
    start_service lxcfs
fi
cat > /etc/systemd/zram-generator.conf <<'EOF'
[zram0]
compression-algorithm=lz4
zram-size=ram/2
fs-type=swap
swap-priority=100
EOF
start_service systemd-zram-setup@zram0.service

ARCH=$(detect_arch)
log "$(t "Architecture: $(uname -m) ($ARCH)" "架构: $(uname -m) ($ARCH)")"

if [ "$VIRT_TYPE" = "podman" ]; then
    start_service podman.socket
fi

# XFS data disk (Podman only - for container storage)
if [ "$VIRT_TYPE" = "podman" ]; then
    if [ -f "/xfs_disk.img" ]; then
        log "$(t "Data disk already exists, skipping." "数据盘已存在，跳过。")"
        mkdir -p /data
        # 启用 noatime 减少元数据压力，增加稳定性
        mountpoint -q /data || mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
        # 确保对已存在的镜像也应用配置规则
        cat > /etc/udev/rules.d/99-loop-directio.rules <<EOF
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="loop*", ATTR{loop/backing_file}=="/xfs_disk.img", ATTR{loop/direct_io}="1"
EOF
        udevadm control --reload-rules && udevadm trigger
        _loop_dev=$(mount | grep "/data" | awk '{print $1}')
        if [[ "$_loop_dev" == /dev/loop* ]]; then
            echo 1 > "/sys/block/${_loop_dev#/dev/}/loop/direct_io" 2>/dev/null || true
        fi
    else
        # 增加 noatime 优化性能和稳定性；容量已在菜单/参数阶段校验。
        create_xfs_disk "/xfs_disk.img" "$PODMAN_DATA_SIZE" "/data" "defaults,pquota,loop,noatime" \
            || die "$(t "Failed to create the Podman XFS data disk." "Podman XFS 数据盘创建失败。")"
    fi
fi

if [ "$VIRT_TYPE" = "podman" ]; then
    cat > /etc/containers/storage.conf <<'EOF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/data/containers/storage"
EOF
    mkdir -p /data/containers/storage
    configure_podman_registry_mirror
fi

configure_host_ipv6_routing() {
    local iface="$1"
    local addr="$2"

    # 禁止 SLAAC 自动配置，防止内核生成额外的临时地址
    sysctl -w "net.ipv6.conf.$iface.autoconf=0" > /dev/null
    echo "net.ipv6.conf.$iface.autoconf=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    # 不处理 RA 中的前缀信息（彻底阻止 SLAAC 动态地址），但保留默认路由
    sysctl -w "net.ipv6.conf.$iface.accept_ra_pinfo=0" > /dev/null
    echo "net.ipv6.conf.$iface.accept_ra_pinfo=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    # forwarding=1 后必须 accept_ra=2 才能继续接收 RA 默认路由
    sysctl -w "net.ipv6.conf.$iface.accept_ra=2" > /dev/null
    echo "net.ipv6.conf.$iface.accept_ra=2" >> /etc/sysctl.d/99-narwhalcloud.conf

    # 将宿主机地址改为 /128，消除 /64 连接路由，避免 Podman 容器子网冲突
    if [ -n "$addr" ]; then
        local current_full
        current_full=$(ip -6 addr show dev "$iface" scope global 2>/dev/null \
            | awk '{print $2}' | grep -F "$addr/" | head -1)
        if [ -n "$current_full" ] && [ "$(echo "$current_full" | cut -d'/' -f2)" != "128" ]; then
            ip addr del "$current_full" dev "$iface" 2>/dev/null || true
            ip addr add "$addr/128" dev "$iface" 2>/dev/null || true
        fi
        # 移除 SLAAC/RA 动态生成的全局地址（如 EUI-64 /64），它们会产生 /64 连接路由
        ip -6 addr flush dev "$iface" scope global dynamic 2>/dev/null || true
        # 持久化到 /etc/network/interfaces
        if grep -q "iface $iface inet6 static" /etc/network/interfaces 2>/dev/null; then
            sed -i "/iface $iface inet6 static/,/^[^ ]/ s|address .*|address $addr/128|" /etc/network/interfaces
        else
            printf '\niface %s inet6 static\n    address %s/128\n' "$iface" "$addr" >> /etc/network/interfaces
        fi
    fi

    log "$(t "✓ Host IPv6 set to /128, SLAAC disabled, RA route preserved." "✓ 宿主机 IPv6 已设为 /128，SLAAC 已禁用，RA 默认路由保留。")"
}

# ── IPv6 detection ────────────────────────────────────────────────────────────

# 初始化 IPv6 模式变量
# IPV6_CONFIG: 检测原始结果，"none" 或 "iface|addr|prefix"
# IPV6_MODE:   最终运行模式，"none" / "snat" / "subnet"（写入 config.json）
# 三种后端的交互安装统一进入网络场景菜单；命令行已给 --ipv6-mode 时不再询问。
# Podman 固定保留 NAT4，因此不提供纯 IPv6 选项。
if [ "$NON_INTERACTIVE" != "1" ] && [ -z "${IPV6_MODE:-}" ]; then
    prompt_ipv6_strategy
fi

# 保存用户通过环境变量显式指定的值（如 IPV6_MODE=subnet bash install.sh）
_USER_IPV6_MODE="${IPV6_MODE:-}"
IPV6_MODE="none"
MANUAL_IPV6_CONFIG=0

IPV6_CONFIG="none"

if [ "$_USER_IPV6_MODE" = "none" ]; then
    log "$(t "IPv6 mode pre-set to 'none', skipping detection." "IPv6 模式已预设为 'none'，跳过 IPv6 检测。")"
elif [ "$_USER_IPV6_MODE" = "snat" ] && [ -n "${IPV6_ADDR:-}" ] && [ -n "${IPV6_IFACE:-}" ]; then
    MANUAL_IPV6_CONFIG=1
    log "$(t "Manual IPv6 SNAT values provided, validating..." "已提供手工 IPv6 SNAT 参数，开始验证...")"
elif [ "$_USER_IPV6_MODE" = "subnet" ] && [ -n "${IPV6_ADDR:-}" ] && [ -n "${IPV6_SUBNET:-}" ] && [ -n "${IPV6_IFACE:-}" ]; then
    # 用户提供了完整的自定义配置（模式 + 地址 + 子网），跳过检测
    MANUAL_IPV6_CONFIG=1
    log "$(t "Custom IPv6 config provided (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET), skipping detection." \
        "已提供自定义 IPv6 配置 (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET)，跳过检测。")"
elif [ -n "$_USER_IPV6_MODE" ]; then
    log "$(t "IPv6 mode pre-set to '$_USER_IPV6_MODE', auto-detecting IPv6..." "IPv6 模式已预设为 '$_USER_IPV6_MODE'，自动检测 IPv6...")"
    IPV6_CONFIG=$(detect_and_configure_ipv6)
else
    if [ "$NON_INTERACTIVE" = "1" ] || [ "${IPV6_DETECT_CONFIRMED:-0}" = "1" ]; then
        _ipv6=Y
    else
        read -rp "$(t "Enable public IPv6 detection? [Y/n]: " "是否进行公网 IPv6 检测? [Y/n]: ")" _ipv6
    fi
    if [[ "${_ipv6:-Y}" =~ ^[Yy]$ ]]; then
        IPV6_CONFIG=$(detect_and_configure_ipv6)
        if [ "$IPV6_CONFIG" = "none" ]; then
            read -rp "$(t "No IPv6 connectivity detected. Continue without IPv6? [Y/n]: " "未检测到 IPv6 连通性，是否不配置 IPv6 继续? [Y/n]: ")" _fb
            [[ "${_fb:-Y}" =~ ^[Nn]$ ]] && { log "$(t "Aborted." "已退出。")"; exit 1; }
        fi
    fi
fi

# ── IPv6 变量规范化（合并检测结果与用户 env var，统一供后续虚拟化分支使用）────────
# 优先使用用户通过 env var 显式提供的值，检测结果作为回退
IPV6_ADDR="${IPV6_ADDR:-}"
IPV6_SUBNET="${IPV6_SUBNET:-}"
IPV6_IFACE="${IPV6_IFACE:-}"
IPV6_PREFIX=""

if [ "$IPV6_CONFIG" != "none" ]; then
    # 从检测结果填充（env var 已有值时不覆盖）
    [ -z "$IPV6_IFACE" ] && IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
    [ -z "$IPV6_ADDR"  ] && IPV6_ADDR=$(echo  "$IPV6_CONFIG" | cut -d'|' -f2)
    IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)
    [ -z "$IPV6_SUBNET" ] && IPV6_SUBNET=$(echo "$IPV6_CONFIG" | cut -d'|' -f4)
    [ "$IPV6_ROUTED" = "1" ] || IPV6_ROUTED=$(echo "$IPV6_CONFIG" | cut -d'|' -f5)
fi

# 从 IPV6_SUBNET 提取前缀长度（当检测未运行或用户自定义子网时）
if [ -z "$IPV6_PREFIX" ] && [ -n "$IPV6_SUBNET" ]; then
    IPV6_PREFIX=$(echo "$IPV6_SUBNET" | cut -d'/' -f2)
fi

# IPV6_IFACE 未知时，从默认路由自动推断
if [ -z "$IPV6_IFACE" ] && [ -n "$IPV6_ADDR" ]; then
    IPV6_IFACE=$(default_route_iface 6)
fi

# 决定最终 IPv6 模式（用户显式指定优先，否则按前缀自动判断）
if [ -n "$_USER_IPV6_MODE" ]; then
    IPV6_MODE="$_USER_IPV6_MODE"
elif [ -n "$IPV6_PREFIX" ] && [ "$IPV6_PREFIX" -le 64 ]; then
    IPV6_MODE="subnet"
    # 未提供 IPV6_SUBNET 时，从 addr/prefix 计算
    if [ -z "$IPV6_SUBNET" ]; then
        IPV6_SUBNET=$(python3 -c "
import ipaddress
net = ipaddress.IPv6Network('$IPV6_ADDR/$IPV6_PREFIX', strict=False)
print(str(net))" 2>/dev/null)
    fi
elif [ -n "$IPV6_PREFIX" ]; then
    IPV6_MODE="snat"
fi
# IPV6_PREFIX 为空（无任何 IPv6）时 IPV6_MODE 保持 "none"

# 健壮性校验：subnet 模式必须有可用子网信息，否则降级
if [ "$IPV6_MODE" = "subnet" ] && [ -z "$IPV6_SUBNET" ]; then
    if [ -n "$IPV6_ADDR" ]; then
        log "$(t "Warning: subnet mode requested but no subnet info detected, falling back to SNAT." \
            "警告: 请求 subnet 模式但未检测到可用子网信息，降级到 SNAT。")"
        IPV6_MODE="snat"
    else
        log "$(t "Warning: subnet mode requested but no IPv6 detected, falling back to 'none'." \
            "警告: 请求 subnet 模式但未检测到 IPv6，降级到 'none'。")"
        IPV6_MODE="none"
    fi
fi

[ -n "$IPV6_ADDR" ] && log "$(t "IPv6: mode=$IPV6_MODE addr=$IPV6_ADDR prefix=${IPV6_PREFIX:--} iface=${IPV6_IFACE:--}" \
    "IPv6: 模式=$IPV6_MODE 地址=$IPV6_ADDR 前缀=${IPV6_PREFIX:--} 网卡=${IPV6_IFACE:--}")"

# ── Network setup (conditional on virtualization type) ────────────────────────

if [ "$VIRT_TYPE" = "podman" ]; then
    # 先安装自定义 netavark（支持 snat_ipv6 选项），再操作网络
    if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark"; then
        if verify_elf_binary "/usr/libexec/podman/netavark" "$ARCH" \
           && verify_sha256 "/usr/libexec/podman/netavark" "$(release_asset_digest "$NETAVARK_BASE" "netavark-new-$ARCH")"; then
            chmod +x /usr/libexec/podman/netavark
            log "$(t "✓ Custom netavark installed." "✓ 自定义 netavark 已安装。")"
        else
            rm -f /usr/libexec/podman/netavark
            log "$(t "netavark download failed verification; using system default." "netavark 下载未通过校验，使用系统默认版本。")"
        fi
    else
        log "$(t "Warning: netavark download failed, using system default." "警告: netavark 下载失败，使用系统默认版本。")"
    fi
    # 确保 netavark 能找到 aardvark-dns（Debian 默认安装在 /usr/lib/podman 下）
    if [ -f "/usr/lib/podman/aardvark-dns" ] && [ ! -f "/usr/libexec/podman/aardvark-dns" ]; then
        ln -sf /usr/lib/podman/aardvark-dns /usr/libexec/podman/aardvark-dns
    fi

if podman network exists "$PODMAN_NETWORK" 2>/dev/null; then
    log "$(t "Podman network $PODMAN_NETWORK already exists, skipping." "Podman 网络 $PODMAN_NETWORK 已存在，跳过创建。")"
    # 确保已有网络禁用内部 DNS 代理，容器直接使用设置的 1.1.1.1 公网 DNS
    sed -i 's/"dns_enabled": true/"dns_enabled": false/' "/etc/containers/networks/${PODMAN_NETWORK}.json" 2>/dev/null || true
elif [ "$IPV6_MODE" = "none" ]; then
    log "$(t "Creating Podman network (IPv4 only)..." "创建 Podman 网络（仅 IPv4）...")"
    podman network create \
        --driver=bridge \
        --disable-dns \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (IPv4 only, no IPv6)." "✓ Podman 网络创建完成（仅 IPv4，无 IPv6）。")"
elif [ "$IPV6_MODE" = "snat" ]; then
    log "$(t "Creating Podman network (ULA IPv6, SNAT)..." "创建 Podman 网络（ULA IPv6，SNAT 模式）...")"
    podman network create \
        --driver=bridge \
        --disable-dns \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        --ipv6 \
        --subnet=fd91:cafe:cafe:10::/64 \
        --gateway=fd91:cafe:cafe:10::1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (ULA IPv6, SNAT via host address)." "✓ Podman 网络创建完成（ULA IPv6，通过宿主机地址 SNAT）。")"
elif [ "$IPV6_MODE" = "subnet" ]; then
    # 计算容器 /112 子网：在宿主机前缀内偏移固定量，避免与 EUI-64/隐私地址冲突
    read -r CONTAINER_BASE CONTAINER_GW <<< "$(python3 - <<PYEOF
import ipaddress
net = ipaddress.IPv6Network('$IPV6_ADDR/$IPV6_PREFIX', strict=False)
base_int = int(net.network_address)
prefix = int('$IPV6_PREFIX')
offset = 0xcafe << (128 - prefix - 16) if prefix <= 112 else 0
container_int = (base_int | offset) & ~((1 << (128 - 112)) - 1)
container_net = ipaddress.IPv6Network(f'{ipaddress.IPv6Address(container_int)}/112', strict=False)
gw = container_net.network_address + 1
print(str(container_net.network_address), str(gw))
PYEOF
)"
    log "$(t "Container subnet: ${CONTAINER_BASE}/112  gateway: ${CONTAINER_GW}" "容器子网: ${CONTAINER_BASE}/112  网关: ${CONTAINER_GW}")"

    if [ -n "$IPV6_IFACE" ] && [ "$IPV6_ROUTED" != "1" ]; then
        configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"
    fi

    podman network create \
        --driver=bridge \
        --disable-dns \
        --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
        --ipv6 \
        --subnet="${CONTAINER_BASE}/112" --gateway="$CONTAINER_GW" \
        "$PODMAN_NETWORK"
    # snat_ipv6=false 不能通过 CLI 传入（Podman CLI 有白名单校验），直接注入 JSON
    NETWORK_JSON="/etc/containers/networks/${PODMAN_NETWORK}.json"
    jq '.options["snat_ipv6"] = "false"' "$NETWORK_JSON" > "${NETWORK_JSON}.tmp" \
        && mv "${NETWORK_JSON}.tmp" "$NETWORK_JSON" \
        && log "$(t "✓ snat_ipv6=false injected into network config." "✓ snat_ipv6=false 已注入网络配置。")" \
        || log "$(t "Warning: failed to inject snat_ipv6=false." "警告: 注入 snat_ipv6=false 失败。")"

    NDP_IFACE="$IPV6_IFACE"
    NDP_SUBNETS="${CONTAINER_BASE}/112"
    NDP_NETWORK="$PODMAN_NETWORK"
    log "$(t "✓ NDP responder will be handled by NarwhalCloud Agent." "✓ NDP responder 将由 NarwhalCloud Agent 内置处理。")"
fi

install_podman_forwarding_compat

# Podman auto-restart service
cat > /usr/lib/systemd/system/podman-restart.service <<'EOF'
[Unit]
Description=Podman Start All Containers With Restart Policy
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/usr/bin/podman start --all --filter restart-policy=always --filter restart-policy=unless-stopped
ExecStop=/usr/bin/podman stop --all --filter restart-policy=always --filter restart-policy=unless-stopped

[Install]
WantedBy=default.target
EOF
start_service podman-restart

# Pull container images
    log "$(t "Pulling container images..." "拉取容器镜像...")"
    for image in "docker.io/narwhalcloud/debian:podman" "docker.io/narwhalcloud/alpine:podman"; do
        podman images --format "{{.Repository}}:{{.Tag}}" | grep -q "$image" \
            && log "$(t "Image $image already exists." "镜像 $image 已存在。")" \
            || { log "$(t "Pulling $image..." "拉取 $image...")"; podman pull "$image" \
                || log "$(t "Warning: failed to pull $image" "警告: $image 拉取失败")"; }
    done
fi  # End of Podman network setup

# IPv6 回滚辅助脚本（幂等）：配置了 IPv6 时提供一个可手动执行的恢复入口
if [ "$IPV6_MODE" != "none" ]; then
    install_ipv6_rollback_helper
fi

# ── Install agent ─────────────────────────────────────────────────────────────

download_agent "$ARCH"

# 初始化 NDP 参数（Podman public IPv6 的 subnet 分支已填好，这里做兜底）
NDP_IFACE="${NDP_IFACE:-}"
NDP_SUBNETS="${NDP_SUBNETS:-}"
NDP_NETWORK="${NDP_NETWORK:-}"

# 生成默认 Web 面板用户名和随机密码
DEFAULT_USER="admin"
DEFAULT_PASS=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 12 2>/dev/null || openssl rand -base64 9 | tr -dc A-Za-z0-9 | head -c 12)

# 首次安装必须生成或接收一个可轮换的对接 Token。交互输入不回显；留空自动生成。
if [ "$GENERATE_TOKEN" = "1" ]; then
    INITIAL_TOKEN=$(generate_agent_token)
elif [ -z "$INITIAL_TOKEN" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
    read -rsp "$(t "Integration Token (leave blank to generate): " "对接 Token（留空自动生成）: ")" INITIAL_TOKEN
    echo
    if [ -n "$INITIAL_TOKEN" ]; then TOKEN_EXPLICIT=1; fi
fi
[ -n "$INITIAL_TOKEN" ] || INITIAL_TOKEN=$(generate_agent_token)
validate_agent_token "$INITIAL_TOKEN" || die "$(t "Invalid integration Token." "无效的对接 Token。")"

# 生成配置文件（包含所有启动参数、IPv6配置和磁盘限制）
write_config_file "$VIRT_TYPE" "$IPV6_MODE" "$IPV6_SUBNET" "$IPV6_ADDR" "$IPV6_IFACE" "$NDP_IFACE" "$NDP_SUBNETS" "$NDP_NETWORK" "$DEFAULT_USER" "$DEFAULT_PASS" "$INITIAL_TOKEN"

write_service_file "$VIRT_TYPE"
start_service "$AGENT_SERVICE"

# ── Optional rfw ─────────────────────────────────────────────────────────────

if [ "$INSTALL_RFW_FORCE" = "1" ]; then
    install_rfw 1
elif [ "$NON_INTERACTIVE" = "1" ]; then
    log "$(t "Non-interactive mode: skipping optional rfw installation." "非交互模式：跳过可选 rfw 安装。")"
else
    install_rfw 0
fi

if [ "$MANUAL_IPV6_CONFIG" = "1" ]; then
    validate_ipv6_values "$IPV6_MODE" "$IPV6_ADDR" "$IPV6_SUBNET" "$IPV6_IFACE" \
        || die "$(t "Manual IPv6 validation failed." "手工 IPv6 参数验证失败。")"
    probe_ipv6_values "$IPV6_MODE" "$IPV6_ADDR" "$IPV6_SUBNET" "$IPV6_IFACE" "$IPV6_ROUTED" \
        || die "$(t "Manual IPv6 online verification failed; use --skip-ipv6-probe only when validation is intentionally offline." \
            "手工 IPv6 在线验证失败；仅在明确离线验证时使用 --skip-ipv6-probe。")"
fi

# ── Done ──────────────────────────────────────────────────────────────────────

IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
echo ""
log "========================================"
log "$(t "✓ NarwhalCloud Agent installation complete!" "✓ NarwhalCloud Agent 安装完成！")"
log "$(t "IP:           $IP" "IP:           $IP")"
log "$(t "Web panel:    https://$IP:$AGENT_WEB_PORT (首次访问需信任自签证书)" "面板地址:     https://$IP:$AGENT_WEB_PORT（首次访问需信任自签证书）")"
log "$(t "Username:     $DEFAULT_USER" "用户名:       $DEFAULT_USER")"
log "$(t "Password:     $DEFAULT_PASS" "密码:         $DEFAULT_PASS")"
log "$(t "Token:        $INITIAL_TOKEN" "对接 Token:    $INITIAL_TOKEN")"
echo ""
log "$(t "Token rotation: bash install.sh --rotate-token" "Token 轮换:    bash install.sh --rotate-token")"
log "$(t "Custom Token:  bash install.sh --rotate-token --token '<new-token>'" "自定义 Token:   bash install.sh --rotate-token --token '<新Token>'")"
if [ "$TOKEN_EXPLICIT" = "1" ]; then
    log "$(t "Platform:     enabled  (standalone: bash install.sh --platform off)" "平台接入:     已启用（切独立模式: bash install.sh --platform off）")"
else
    log "$(t "Platform:     standalone  (connect: bash install.sh --platform on)" "平台接入:     独立模式（接入平台: bash install.sh --platform on）")"
fi
log "$(t "Network:      IPv4 NAT=on, IPv6=$IPV6_MODE" "网络模式:      IPv4 NAT=开启, IPv6=$IPV6_MODE")"
log "========================================"
