# Runman Agent（二开版 · Podman 专精）

> 把你的闲置服务器切分成 NAT VPS 实例出租，平摊托管成本。
>
> 本项目 fork 自 [narwhal-cloud/runman-agent](https://github.com/narwhal-cloud/runman-agent)，面向自有部署场景做了二次开发：**只维护 Podman 后端**，自建发布与更新通道，并新增了容器网络隔离、弹性 IP、安全加固与品牌自定义等能力。

[English](README.en.md) · [协议对接指南](PROTOCOL.md)

---

## 这是什么

Agent 是运行在宿主机（母鸡）上的后台守护进程，负责将服务器资源切分成 NAT VPS 实例，管理容器生命周期、端口转发、流量统计，并通过 gRPC 双向流对接平台（协议见 [PROTOCOL.md](PROTOCOL.md)）。同时提供本地 Web 管理面板（默认 `:8792`）。

```
你的服务器 ──► Runman Agent ──► NAT VPS 实例（容器，narwhal-net 10.91.0.0/20 内网）
                 │
                 └── gRPC 双向流对接平台：心跳上报、命令下发、流量计费
```

## 与上游的主要差异

| 差异点 | 说明 |
|---|---|
| **只维护 Podman 后端** | 安装器拒绝 `--virt incus/cloudhv`；Go 代码中保留的 incus/cloudhv 分支不再维护 |
| **自建 netavark** | 使用 [tl0502/netavark-Runman](https://github.com/tl0502/netavark-Runman)（新增 `snat_ipv6` 选项），安装/更新时自动替换 `/usr/libexec/podman/netavark` |
| **容器网络隔离与白名单** | bridge 家族 nft 规则实现容器间隔离；宿主机端口白名单 + 内网网段白名单（v0.5.x） |
| **弹性 IP 模型** | 新增 `public_ipv4`：所有容器共用一个宿主机 IPv4 做入口监听 + 出口 SNAT |
| **安全加固** | userns=auto、no_new_privs、pids 上限、capabilities 精细控制（默认开启） |
| **品牌自定义** | 面板站点名称、站点图标（favicon）可通过面板/API 替换 |
| **自建更新通道** | Agent 自更新与安装器下载源均指向 `tl0502/runman-agent`，`netavark_base` 可配置 |
| **语义化版本 + CI 发布** | push tag `v*` 触发 GitHub Actions 自动测试、构建并发布 Release |
| **API 兼容承诺** | 平台对接 API 口径不变；只新增可选字段，旧客户端零影响 |

## 系统要求

| 项目 | 要求 |
|---|---|
| 操作系统 | **Debian 13 (Trixie)** 强烈推荐（依赖 `apt`、`podman`、`systemd-zram-generator`） |
| 架构 | x86_64 (amd64) · aarch64 (arm64) |
| 内存 | ≥ 1 GB RAM |
| 磁盘 | 建议 ≥ 10 GB；Podman 数据盘最低保留 1536 MiB 根分区空间 |
| 后端 | Podman（安装器自动安装）；已有 Docker 业务可共存，不会触碰 |

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/tl0502/runman-agent/main/install.sh)
```

或使用最新 Release 的安装脚本：

```bash
bash <(curl -fsSL https://github.com/tl0502/runman-agent/releases/latest/download/install.sh)
```

安装器从 `tl0502/runman-agent` Releases 下载 Agent 与 rfw 二进制，从 `tl0502/netavark-Runman` 下载自建 netavark。需要固定版本时设置 `AGENT_RELEASE_TAG=vX.Y.Z`；自建下载源设置 `RUNMAN_AGENT_DOWNLOAD_BASE=https://...` 与 `NETAVARK_BASE=https://...`。

### 一键参数示例

```bash
# 仅 IPv4 NAT
bash install.sh zh --virt podman --nat4 --non-interactive --generate-token

# NAT4 + IPv6 SNAT（单个公网 IPv6 场景）
bash install.sh zh --virt podman --data-size 3G --ipv6-mode snat \
  --non-interactive --generate-token

# NAT4 + 手工路由 /64（HE 6in4、WireGuard、供应商静态路由）
bash install.sh zh --virt podman --nat4 --non-interactive \
  --ipv6-mode subnet --ipv6-addr 2001:db8:100::1 \
  --ipv6-subnet 2001:db8:100::/64 --ipv6-iface wg6 --ipv6-routed

# Podman registry mirror（能直连 docker.io 就留空）
bash install.sh zh --virt podman --data-size 8G --ipv6-mode none \
  --podman-registry-mirror https://mirror.example.com --non-interactive

# 只做 IPv6 探测/验证，不安装任何组件
bash install.sh zh --detect-ipv6 --non-interactive
bash install.sh zh --validate-ipv6 --ipv6-mode subnet \
  --ipv6-addr 2001:db8:100::1 --ipv6-subnet 2001:db8:100::/64 \
  --ipv6-iface wg6 --ipv6-routed
```

### 总菜单

无参数执行 `bash install.sh zh` 进入菜单：

| 菜单 | 操作 | 是否修改系统 |
|---:|---|---|
| 1 | 安装/更新（网络引导配置） | 是 |
| 2 | 查看安装/服务状态 | 否 |
| 3 | 修复网络并重启 Agent/防火墙 | 是，可恢复 |
| 4 | 重置 Web 面板密码 | 是 |
| 5–6 | 探测 / 验证 IPv6 | 探测仅临时绑定测试地址 |
| 7–8 | 显示 / 轮换对接 Token | 轮换会重启 Agent |
| 9–10 | 备份 / 回滚 IPv6 配置 | 是 |
| 11 | 卸载 Agent（保留容器/镜像） | 是 |
| 12 | 配置/刷新容器镜像 | 是 |
| 13 | 仅升级 Agent（保留网络） | 是，Agent 短暂重启 |
| 0 | 退出 | — |

---

## 弹性 IP 模型（重要）

容器本身始终使用 `narwhal-net`（`10.91.0.0/20`）内网地址。新增配置 `public_ipv4` 指定一个宿主机 IPv4 后：

- **入口**：所有端口映射只在该地址上监听；
- **出口**：容器出站流量统一 SNAT 成该地址；
- **入口与出口必须是同一个 IP**。

上报给平台的 `entry_host`（入口地址）取值优先级：

```
host（手动入口，支持 DDNS 域名） > public_ipv4 > 自动探测公网 IPv4（api-ipv4.ip.sb）
```

弹性 IP 场景下，`public_ipv4` 正是上报给平台的 `entry_host`。`public_ipv6` 为预留字段，当前版本未实现。

## 容器网络隔离与访问控制

v0.5.x 起默认开启，全部通过 `runman-hostfw` nft 表实现（**不使用 netavark isolate**——br_netfilter 未加载时同桥流量不过 FORWARD hook，isolate 不生效）：

| 表 | 作用 |
|---|---|
| `inet runman-hostfw` | INPUT 宿主机端口白名单（`host_allow_ports`）+ FORWARD 内网网段拦截（`container_allow_subnets`） |
| `bridge runman-hostfw` | 容器间隔离：`meta ibrname == obrname drop`，容器只能访问网关与出站 |

行为约定：

- `container_isolate`（默认 true）：容器之间不能互相访问；
- `host_allow_ports`：允许容器访问的宿主机 TCP 端口白名单，为空时容器不能主动访问宿主机任何端口；
- `container_allow_subnets`：允许访问的内网网段白名单（CIDR），为空时容器默认不能访问任何私有网段（10/8、172.16/12、192.168/16、169.254/16），只能出公网。

> 技术坑：netavark setup 时若链已存在会跳过重建，重装网络必须先 `nft delete table inet netavark` 再 reload。菜单 **3** 已内置该修复逻辑。

## 安全加固

创建容器时默认应用（均可在 `config.json` 关闭）：

| 配置 | 默认 | 说明 |
|---|---|---|
| `container_userns` | true | `userns=auto`，容器内 root 映射为宿主机非特权 uid，逃逸后拿不到宿主 root（依赖 `/etc/subuid`、`/etc/subgid`） |
| `container_no_new_privs` | true | 阻断 setuid 提权 |
| `container_pids_limit` | 2048 | cgroup pids 上限 |
| `container_drop_caps` / `container_add_caps` | 空 | 创建容器时额外 drop/add 的 capabilities（注意：systemd + sshd 镜像对默认 capability 集依赖较强，激进 drop 需先测试） |

## IPv6

| 模式 | 触发条件 | 行为 |
|---|---|---|
| `none` | 无公网 IPv6 | 仅 IPv4 |
| `snat` | 单个 `/128` 或前缀 `/65`–`/127` | 容器使用 ULA `fd91:cafe:cafe:10::/64`，经宿主机公网 IPv6 SNAT 出站（自建 netavark `snat_ipv6=false` 配合） |
| `subnet` | 前缀 ≤ `/64` | 从路由 `/64` 按 offset 切分 `/112` 子网给容器，容器获得独立公网 IPv6 |

自动探测会区分原生二层前缀与隧道链路前缀（HE 6in4、WireGuard），手工 `/64` 支持 `--ipv6-routed` 标记路由/隧道场景。改动前自动备份 sysctl/网卡配置到 `/var/lib/narwhal-agent/backups/`，支持一键回滚。

## 品牌自定义

- 面板设置中可修改站点名称（顶栏 + 浏览器标题）；
- 站点图标支持上传（PNG/ICO），同时充当浏览器标签 favicon；未配置时回落内置图标；
- API：`POST /api/brand/icon`。

## 更新与发布

- **发布流程**：push tag `v*`（如 `v0.5.3`）→ GitHub Actions 自动跑 Go 测试 + 安装器测试 → 构建 amd64/arm64 → 发布 Release。`main` 分支持续构建为 `dev-<commit>`（仅用于验证，不产生 Release）。
- **Agent 自更新**：默认每 6 小时检查 `tl0502/runman-agent`，发现新版本随机延迟 24–72 小时执行 `--update-only --non-interactive`；更新来源可用 `update_repo` / `update_base_url` / `update_api_url` 指向镜像。
- **仅升级 Agent**（保留网络与业务）：

```bash
bash install.sh zh --update-only
journalctl -u narwhal-agent-update --no-pager   # 自动更新任务日志
```

- 禁用自动更新：`systemctl edit narwhal-agent` 加入 `Environment=RUNMAN_AGENT_AUTO_UPDATE=0` 后 `daemon-reload && restart`。

## 安装内容

| 组件 | 路径 |
|---|---|
| Agent 二进制 | `/opt/narwhal-agent/narwhal-agent` |
| 配置文件 | `/opt/narwhal-agent/config.json` |
| Agent 数据库 | `/opt/narwhal-agent/agent.db` |
| 数据目录 / 备份 | `/var/lib/narwhal-agent`（备份在其 `backups/`） |
| Podman 数据盘 | `/xfs_disk.img` → XFS 挂载至 `/data` |
| Systemd 服务 | `narwhal-agent.service`（rfw 可选：`rfw.service`） |
| 自建 netavark | `/usr/libexec/podman/netavark`（安装器替换） |

**Web 管理面板**：`http://<服务器IP>:8792`

## 关键配置字段（`/opt/narwhal-agent/config.json`）

### 平台与更新

| 字段 | 说明 |
|---|---|
| `token` | 平台对接 Token（首装写入，`--show-token` / `--rotate-token` 管理） |
| `platform_addr` | 平台 gRPC 地址，改动后自动重连，无需重启 |
| `platform_insecure` | 跳过 gRPC 证书校验（仅自签/IP 场景） |
| `update_repo` / `update_base_url` / `update_api_url` | 自更新来源（默认 `tl0502/runman-agent`） |
| `netavark_base` | 自建 netavark 下载基址 |
| `virt_type` | 固定 `podman` |
| `web` / `web_user` / `web_pass_hash` | 面板监听、用户名、bcrypt 密码哈希 |
| `monitor_nic` / `monitor_disk` | 流量/磁盘统计目标 |
| `max_port_forward` / `max_forward_ips` | 每 VM 端口转发数与来源 IP 数上限 |
| `traffic_reset_day` | 每月流量重置日（1-28） |

### 本 fork 新增

| 字段 | 说明 |
|---|---|
| `public_ipv4` | 弹性 IP：入口监听 + 出口 SNAT 共用地址；为空保持旧行为 |
| `site_name` | 面板品牌名（另有站点图标，存配置目录，经面板/API 设置） |
| `container_isolate` | 容器间隔离（默认 true） |
| `host_allow_ports` | 容器可访问的宿主机 TCP 端口白名单 |
| `container_allow_subnets` | 容器可访问的内网网段白名单（CIDR） |
| `container_no_new_privs` / `container_userns` / `container_pids_limit` / `container_drop_caps` / `container_add_caps` | 容器安全加固 |

## 服务管理与排查

```bash
systemctl status narwhal-agent          # Agent 状态
journalctl -u narwhal-agent -f          # 实时日志
systemctl restart narwhal-agent
systemctl status rfw                   # rfw 防火墙
```

常见问题：

- **Agent 启动失败**：`journalctl -u narwhal-agent --no-pager -n 50`
- **rfw 启动失败**：云厂商网卡可能不支持 XDP，在服务参数加 `--xdp-mode skb`（安装器日志检测到挂载失败会自动切换）
- **Podman 数据盘未挂载**：`mount -o defaults,pquota,loop,noatime /xfs_disk.img /data` 后重启 Agent
- **Docker 共存时容器无法出站**：不要清 Docker 规则，`systemctl restart runman-podman-forwarding`；该 oneshot 服务只在 `DOCKER-USER` 链放行 `10.91.0.0/20` 与 `fd91:cafe:cafe:10::/64`
- **容器网络规则异常**：菜单 **3** 先按现有配置重建缺失的 `narwhal-net`（会先删除 `inet netavark` 表避免链残留），再重启 Agent/rfw

## 协议与兼容性

平台对接走 gRPC 双向流（`AgentGateway`），完整协议与最小实现示例见 [PROTOCOL.md](PROTOCOL.md)。

**兼容性红线**：对接 API 口径不能改；新增能力一律以可选字段形式添加，保证旧客户端零影响。Agent 端实现者按 PROTOCOL.md 对接即可，无需关心本 fork 的后端差异。

## 本地开发

```bash
go build ./...          # Windows 下会因 manager/cloudhv 的 syscall.Setsid 失败，Linux-only，CI 覆盖
go test -tags containers_image_openpgp ./...
bash -n install.sh
python3 -m unittest discover -s tests -v
```

Web 静态资源在 `web/static/`，面板 API 在 `web/server.go`。
