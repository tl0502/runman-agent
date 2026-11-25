package patrol

// ActiveConns 是单个 IP 的活跃连接画像：按方向（入/出）× 单向/双向。
//
// 方向的判定口径与 conntrack「原始方向」一致：VM 是源 = 出，VM 是目的 = 入。
// 端口转发是宿主上的用户态代理，容器侧那一跳是「网关 → 容器」，所以经转发的
// 访问会正常计入入向，不需要额外处理。
type ActiveConns struct {
	InBidi  int // 入向已建立（TCP ESTABLISHED / UDP ASSURED）
	InUni   int // 入向单向（TCP SYN_RECV / UDP UNREPLIED）
	OutBidi int // 出向已建立
	OutUni  int // 出向单向（TCP SYN_SENT / UDP UNREPLIED）
}

// SnapshotActiveConns 读一次 /proc/net/nf_conntrack，返回 watch 内每个 IP 的
// 连接画像。未出现在表里的 IP 不返回（调用方按零值处理）。
//
// 一次全表解析服务所有实例：conntrack 可能有几万行，按实例各读一遍会把面板
// 轮询拖垮。watch 为空时直接返回空表，避免解析无用数据。
func SnapshotActiveConns(ips []string) (map[string]ActiveConns, error) {
	if len(ips) == 0 {
		return map[string]ActiveConns{}, nil
	}
	watch := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip != "" {
			watch[ip] = true
		}
	}
	if len(watch) == 0 {
		return map[string]ActiveConns{}, nil
	}
	// scanCap=0 / smtpPorts=nil：这里只要方向计数，不承担滥用画像的开销。
	all, err := countAllConnections(watch, 0, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ActiveConns, len(watch))
	for ip := range watch {
		if st := all[ip]; st != nil {
			out[ip] = ActiveConns{
				InBidi:  st.in,
				InUni:   st.inUni,
				OutBidi: st.out,
				OutUni:  st.outUni,
			}
		}
	}
	return out, nil
}
