/* ============================================================================
   Run · 面板前端
   结构：状态 → 工具 → 视图路由 → 各视图渲染 → 抽屉 → 浮层 → 轮询
   接口约定与原面板保持一致，后端无需为 UI 改造让路。
   ========================================================================== */

const $ = (id) => document.getElementById(id);
const qs = (sel, root = document) => root.querySelector(sel);
const qsa = (sel, root = document) => [...root.querySelectorAll(sel)];

/* ── 工具 ─────────────────────────────────────────────────────────────────── */
// fmtVersion 拼接展示用的版本串：语义化 tag + 构建短 sha。
// 只有 tag 时显示 tag，没有 tag 时退回占位符。
const fmtVersion = (v, withCommit = true) => {
  const tag = (v && v.current) || '—';
  if (!withCommit) return tag;
  const c = (v && v.commit) || '';
  return c ? `${tag} · ${c}` : tag;
};

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function fmtBytes(b, d = 1) {
  b = Number(b) || 0;
  if (!b) return '0 B';
  const k = 1024, u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(b) / Math.log(k)));
  return (b / k ** i).toFixed(i === 0 ? 0 : d) + ' ' + u[i];
}
function fmtBps(bytesPerSec) {
  const b = (Number(bytesPerSec) || 0) * 8;
  if (!b) return '0 bps';
  const k = 1000, u = ['bps', 'Kbps', 'Mbps', 'Gbps'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(b) / Math.log(k)));
  return (b / k ** i).toFixed(i === 0 ? 0 : 1) + ' ' + u[i];
}
function fmtUptime(sec) {
  sec = Number(sec) || 0;
  if (!sec) return '—';
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d} 天 ${h} 小时`;
  if (h) return `${h} 小时 ${m} 分`;
  return `${m} 分`;
}
const gb = (bytes) => (Number(bytes) || 0) / 1024 ** 3;
const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
const num1 = (v) => (Number(v) || 0).toFixed(1);
const nowSec = () => Math.floor(Date.now() / 1000);

function ago(sec) {
  if (!sec) return '从未';
  const d = nowSec() - sec;
  if (d < 60) return `${Math.max(1, d)} 秒前`;
  if (d < 3600) return `${Math.floor(d / 60)} 分钟前`;
  if (d < 86400) return `${Math.floor(d / 3600)} 小时前`;
  return `${Math.floor(d / 86400)} 天前`;
}

/* ── 请求 ─────────────────────────────────────────────────────────────────── */
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) {
    // 会话失效：除登录自身外，一律切到登录页
    if (path !== '/api/session' && path !== '/api/login') {
      showLogin();
      throw new Error('会话已过期，请重新登录');
    }
    throw new Error('用户名或密码错误');
  }
  if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
  if (res.status === 204) return null;
  return res.json().catch(() => null);
}

async function withBusy(btn, fn) {
  if (!btn) return fn();
  const old = btn.innerHTML;
  btn.classList.add('is-loading');
  btn.disabled = true;
  try { return await fn(); }
  finally {
    btn.classList.remove('is-loading');
    btn.disabled = false;
    if (old) btn.innerHTML = old;
  }
}

/* ── Toast ────────────────────────────────────────────────────────────────── */
function toast(msg, kind = 'ok') {
  const box = $('toasts');
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><use href="#${kind === 'bad' ? 'i-warn' : 'i-check'}"/></svg><span>${esc(msg)}</span>`;
  box.appendChild(el);
  setTimeout(() => { el.classList.add('out'); setTimeout(() => el.remove(), 260); }, 2600);
}
const toastErr = (e) => toast(e?.message || String(e), 'bad');

/* ── 状态 ─────────────────────────────────────────────────────────────────── */
const state = {
  view: 'overview',
  vms: [],
  config: null,
  hasToken: false,
  images: [],
  sort: { field: '', order: 'desc' },
  filter: 'all',
  search: '',
  activeVm: null,
  drawerTab: 'monitor',
  pgTimers: [],
  pfCache: [],
  allowPorts: [],
  wgCache: [],
  wgEditId: null,
  settingsCat: 'panel',
  imageItems: [],
  dirty: false,
  siteIcon: 0,
  version: null,
  lastStatus: null,
};

/* ── 外观 ─────────────────────────────────────────────────────────────────── */
function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem('run-theme', theme);
  $('themeBtn').innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#${theme === 'dark' ? 'i-sun' : 'i-moon'}"/></svg>`;
  if (state.activeVm) drawMonitorCharts();
}
$('themeBtn').addEventListener('click', () => {
  applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
});

/* ── 视图路由 ─────────────────────────────────────────────────────────────── */
const VIEWS = ['login', 'overview', 'vms', 'firewall', 'security', 'patrol', 'settings'];

function setView(name, { push = true } = {}) {
  if (!VIEWS.includes(name)) name = 'overview';
  const nextEl = $(`view-${name}`);
  const others = qsa('.view').filter((el) => !el.hidden && el !== nextEl);
  state.view = name;

  // 快速连点时先把上一轮还在淡出的视图收掉，避免同时叠三层
  qsa('.view.is-leaving').forEach((el) => { el.hidden = true; el.classList.remove('is-leaving'); });

  others.forEach((el) => {
    el.classList.remove('is-entering');
    el.classList.add('is-leaving');
    setTimeout(() => { el.hidden = true; el.classList.remove('is-leaving'); }, 240);
  });

  nextEl.hidden = false;
  nextEl.classList.remove('is-entering');
  void nextEl.offsetWidth; // 强制重排，让动画每次都从头播
  nextEl.classList.add('is-entering');
  setTimeout(() => nextEl.classList.remove('is-entering'), 320);

  qsa('.seg').forEach((b) => b.setAttribute('aria-selected', String(b.dataset.view === name)));
  moveSegThumb();
  if (push && location.hash.slice(1) !== name) history.replaceState(null, '', `#${name}`);
  if (name === 'settings') document.documentElement.scrollTop = 0; // 整页流：每次进设置页从顶部开始
  if (name === 'vms') renderVMs();
  if (name === 'firewall') refreshFirewall();
  if (name === 'security') refreshSecurity();
  if (name === 'patrol') { clearPatrolDot(); refreshPatrol(); }
  if (name === 'settings') syncDirty();
  updateSavebar();
  window.scrollTo({ top: 0, behavior: 'instant' });
}

function moveSegThumb() {
  const active = qs('.seg[aria-selected="true"]');
  const thumb = $('segThumb');
  if (!active || !thumb) return;
  // 用 rect 差值定位：容器 padding / border 都算进去，避免 offsetLeft 与 padding 混算。
  const cRect = thumb.parentElement.getBoundingClientRect();
  const aRect = active.getBoundingClientRect();
  const inset = Number.parseFloat(getComputedStyle(thumb.parentElement).paddingLeft) || 0;
  thumb.style.width = `${aRect.width}px`;
  thumb.style.transform = `translateX(${aRect.left - cRect.left - inset}px)`;
}

qsa('.seg').forEach((btn) => btn.addEventListener('click', () => setView(btn.dataset.view)));
qsa('[data-goto]').forEach((el) => el.addEventListener('click', (e) => { e.preventDefault(); setView(el.dataset.goto); }));
window.addEventListener('resize', moveSegThumb);

/* ══════════════════════════════ 概览 ═══════════════════════════════════════ */
async function refreshStatus() {
  try {
    const d = await api('GET', '/api/status');
    if (!d) return;
    state.lastStatus = d;

    const cpu = Number(d.cpu_pct) || 0;
    $('mCpuNum').textContent = num1(cpu);
    $('mCpuBar').style.width = `${clamp(cpu, 0, 100)}%`;
    $('mCpuBar').className = cpu > 85 ? 'tone-red' : cpu > 65 ? 'tone-amber' : '';
    $('mCpuCores').textContent = d.cpus || '—';
    $('mLoad').textContent = [d.load1, d.load5, d.load15].map((x) => (Number(x) || 0).toFixed(2)).join(' / ');

    const ramUsed = (Number(d.ram_used_mb) || 0) / 1024;
    const ramTotal = (Number(d.ram_total_mb) || 0) / 1024;
    $('mMemNum').textContent = ramUsed.toFixed(1);
    $('mMemOf').textContent = `/ ${ramTotal.toFixed(0)} GB`;
    $('mMemBar').style.width = ramTotal ? `${clamp((ramUsed / ramTotal) * 100, 0, 100)}%` : '0%';
    $('mMemMeta').textContent = ramTotal ? `已用 ${Math.round((ramUsed / ramTotal) * 100)}%` : '—';

    const diskUsed = Number(d.disk_used_gb) || 0, diskTotal = Number(d.disk_total_gb) || 0;
    $('mDiskNum').textContent = diskUsed.toFixed(0);
    $('mDiskOf').textContent = `/ ${diskTotal.toFixed(0)} GB`;
    $('mDiskBar').style.width = diskTotal ? `${clamp((diskUsed / diskTotal) * 100, 0, 100)}%` : '0%';
    $('mDiskMeta').textContent = diskTotal ? `已用 ${Math.round((diskUsed / diskTotal) * 100)}%` : '—';

    $('mNetOut').textContent = fmtBps(d.net_out_bps);
    $('mNetIn').textContent = fmtBps(d.net_in_bps);
    renderBandwidth(d);
    $('mNetMeta').textContent = `累计 ↑ ${fmtBytes(d.net_out_total)} · ↓ ${fmtBytes(d.net_in_total)}`;

    $('ovUptime').textContent = fmtUptime(d.uptime);

    const monthIn = Number(d.month_in_total) || 0, monthOut = Number(d.month_out_total) || 0;
    const totalGb = gb(monthIn) + gb(monthOut);
    $('flowTotal').textContent = totalGb.toFixed(1);
    $('flowOut').textContent = fmtBytes(monthOut);
    $('flowIn').textContent = fmtBytes(monthIn);
    const maxFlow = Math.max(monthIn, monthOut, 1);
    $('flowInBar').style.width = `${(monthIn / maxFlow) * 100}%`;
    $('flowOutBar').style.width = `${(monthOut / maxFlow) * 100}%`;
    $('flowInBar').style.background = 'var(--blue)';
    $('flowOutBar').style.background = 'var(--green)';
    renderHostKv();
  } catch { /* 静默：面板可能刚启动 */ }
}

// 带宽显示的是最近一次测速的 min(下行, 上行)：非对称线路按窄的一侧算，
// 与心跳上报的口径一致。没有结果时显示 —（旧版误显示「不限」），失败原因放 tooltip。
function renderBandwidth(d) {
  const btn = $('btnSpeedTest');
  const busy = !!d.speed_testing;
  btn.classList.toggle('is-busy', busy);
  btn.disabled = busy;

  const el = $('mNetBw');
  const sp = d.speed;
  if (busy) {
    el.textContent = '测速中…';
    el.title = '';
    return;
  }
  if (!sp || !sp.bandwidth_mbps) {
    el.textContent = '—';
    el.title = d.speed_error ? `测速失败：${d.speed_error}` : '尚未测速，点右上角图标重测';
    return;
  }
  el.textContent = `${sp.bandwidth_mbps} Mbps`;
  const detail = [`↓ ${sp.down_mbps || '—'}`, `↑ ${sp.up_mbps || '—'}`];
  if (sp.partial) detail.push('仅测通一个方向');
  detail.push(ago(Math.floor(Date.parse(sp.tested_at) / 1000)));
  // 有旧结果时重测失败不会改变显示值，但必须让人知道这次没测成。
  if (d.speed_error) detail.push('最近一次重测失败');
  el.title = detail.join(' · ');
}

function renderHostKv() {
  const c = state.config || {};
  const d = state.lastStatus || {};
  const rows = [
    ['内核', esc(d.kernel || '—')],
    ['监听网卡', esc(c.monitor_nic || '全部')],
    ['监听磁盘', esc(c.monitor_disk || '/')],
    ['唯一 IP 上限', c.max_forward_ips > 0 ? `${c.max_forward_ips} 个 / VM` : '不限'],
    ['端口转发上限', `${c.max_port_forward || 20} 条 / VM`],
  ];
  $('hostKv').innerHTML = rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('');
  // 入口地址由后端统一算好（entry_addr）：host > 公网性质的 public_ipv4 > 自动探测。
  // 不再直接拼 public_ipv4——1:1 NAT 机器上它是内网监听地址，不能当入口展示。
  // location.host 仅作最后兜底（复制按钮不能拿到「自动探测」这种无效文本）。
  const entryAddr = c.entry_addr || c.host || c.detected_ipv4 || location.host;
  $('ovHostAddr').textContent = entryAddr;
  $('ovHost').textContent = entryAddr;
  $('ovOs').textContent = d.os || '—';
  $('flowReset').textContent = c.traffic_reset_day ? `每月 ${c.traffic_reset_day} 日重置` : '—';
}

function renderVmSummary() {
  const vms = state.vms;
  const run = vms.filter((v) => v.status === 2).length;
  const err = vms.filter((v) => v.status === 6).length;
  const stop = vms.length - run - err;

  $('vmRunNum').textContent = run;
  $('vmAllNum').textContent = vms.length;
  $('lgRun').textContent = run;
  $('lgStop').textContent = stop;
  $('lgErr').textContent = err;

  const pct = (n) => (vms.length ? (n / vms.length) * 100 : 0);
  $('vmStack').innerHTML = vms.length
    ? `<i class="tone-run" style="width:${pct(run)}%"></i><i class="tone-stop" style="width:${pct(stop)}%"></i><i class="tone-err" style="width:${pct(err)}%"></i>`
    : '';

  const top = [...vms].sort((a, b) => (b.monthly_traffic_in + b.monthly_traffic_out) - (a.monthly_traffic_in + a.monthly_traffic_out)).slice(0, 3);
  $('vmMiniList').innerHTML = top.length
    ? top.map((v) => `
      <div class="mini-row" data-vm="${esc(v.vm_id)}">
        <span class="badge badge-${v.status === 2 ? 'ok' : v.status === 6 ? 'bad' : 'quiet'}" style="height:6px;width:6px;padding:0;border-radius:50%"></span>
        <span class="name truncate">${esc(v.vm_id)}</span>
        <span class="spacer"></span>
        <span class="val">${fmtBytes((v.monthly_traffic_in || 0) + (v.monthly_traffic_out || 0))}</span>
      </div>`).join('')
    : '<div class="mini-row"><span class="val">还没有虚拟机</span></div>';
  qsa('.mini-row[data-vm]').forEach((row) => row.addEventListener('click', () => openDrawer(row.dataset.vm, 'monitor')));
}

/* ══════════════════════════════ 虚拟机 ═════════════════════════════════════ */
const STATUS_META = {
  1: { label: '创建中', cls: 'badge-warn' },
  2: { label: '运行中', cls: 'badge-ok' },
  3: { label: '已停止', cls: 'badge-quiet' },
  6: { label: '异常', cls: 'badge-bad' },
};
const statusOf = (s) => STATUS_META[s] || { label: '未知', cls: 'badge-quiet' };
const statusBadge = (s) => {
  const m = statusOf(s);
  return `<span class="badge ${m.cls}"><span class="dot"></span>${m.label}</span>`;
};

async function refreshVMs() {
  try {
    const vms = (await api('GET', '/api/vms')) || [];
    state.vms = vms;
    renderVMs();
    renderVmSummary();
  } catch { /* 静默 */ }
}

function filteredVMs() {
  let list = [...state.vms];
  if (state.filter === 'run') list = list.filter((v) => v.status === 2);
  if (state.filter === 'stop') list = list.filter((v) => v.status !== 2);
  const q = state.search.trim().toLowerCase();
  if (q) list = list.filter((v) => v.vm_id.toLowerCase().includes(q) || (v.ips || []).some((ip) => ip.toLowerCase().includes(q)));
  const { field, order } = state.sort;
  if (field) {
    list.sort((a, b) => {
      const diff = Number(b[field] ?? 0) - Number(a[field] ?? 0);
      return order === 'desc' ? diff : -diff;
    });
  }
  return list;
}

function renderVMs() {
  const vms = state.vms;
  const run = vms.filter((v) => v.status === 2).length;
  $('cAll').textContent = vms.length;
  $('cRun').textContent = run;
  $('cStop').textContent = vms.length - run;

  const list = filteredVMs();
  const tbody = $('vmTbody');
  const empty = $('vmEmpty');

  if (!list.length) {
    tbody.innerHTML = '';
    empty.hidden = false;
    qs('.table-wrap', qs('#view-vms')).style.display = 'none';
    return;
  }
  empty.hidden = true;
  qs('.table-wrap', qs('#view-vms')).style.display = '';

  tbody.innerHTML = list.map((vm) => {
    const running = vm.status === 2;
    const ramTotal = vm.ram_total_mb || 0;
    const ramPct = ramTotal ? clamp((vm.ram_used_mb / ramTotal) * 100, 0, 100) : 0;
    const cpu = Number(vm.cpu_pct) || 0;
    const monthTotal = (vm.monthly_traffic_in || 0) + (vm.monthly_traffic_out || 0);
    return `
    <tr class="is-clickable" data-vm="${esc(vm.vm_id)}">
      <td>
        <div class="cell-name">
          <span class="id truncate" title="${esc(vm.vm_id)}">${esc(vm.vm_id)}</span>
          <span class="ip">${(vm.ips && vm.ips.length) ? esc(vm.ips.join(' · ')) : '—'}</span>
        </div>
      </td>
      <td>${statusBadge(vm.status)}</td>
      <td class="num-cell">${num1(cpu)}<span class="dim"> %</span></td>
      <td class="num-cell">
        ${vm.ram_used_mb || 0}<span class="dim"> / ${ramTotal || '?'} MB</span>
      </td>
      <td class="mono-cell">
        <span class="dim">↑</span> ${fmtBps(vm.net_out_bps)}
        <span class="dim" style="margin:0 4px">/</span>
        <span class="dim">↓</span> ${fmtBps(vm.net_in_bps)}
      </td>
      <td class="num-cell">
        <div class="sub" style="font-size:12px">${fmtBytes(monthTotal)}</div>
        <div class="sub" style="font-size:10px">↓ ${fmtBytes(vm.monthly_traffic_in)} · ↑ ${fmtBytes(vm.monthly_traffic_out)}</div>
      </td>
      <td class="small muted">${vm.created_at ? ago(vm.created_at) : '—'}</td>
      <td style="text-align:right">
        <div class="act-group" data-actions="${esc(vm.vm_id)}">
          <button class="act" data-act="console" title="控制台"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-terminal"/></svg></button>
          <button class="act" data-act="monitor" title="监控"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-chart"/></svg></button>
          <button class="act" data-act="ports" title="端口转发"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-swap"/></svg></button>
          <button class="act" data-act="wg" title="WireGuard"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-lock"/></svg></button>
          <button class="act" data-act="power" title="${running ? '停止' : '启动'}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#${running ? 'i-stop' : 'i-play'}"/></svg></button>
          <button class="act" data-act="manage" title="管理"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-gear"/></svg></button>
        </div>
      </td>
    </tr>`;
  }).join('');

  qsa('#vmTbody tr').forEach((tr) => {
    tr.addEventListener('click', (e) => {
      const btn = e.target.closest('[data-act]');
      const id = tr.dataset.vm;
      if (!btn) return openDrawer(id, 'monitor');
      e.stopPropagation();
      const vm = state.vms.find((v) => v.vm_id === id);
      switch (btn.dataset.act) {
        case 'console': return openConsole(id);
        case 'monitor': return openDrawer(id, 'monitor');
        case 'ports': return openDrawer(id, 'ports');
        case 'wg': return openDrawer(id, 'wg');
        case 'manage': return openDrawer(id, 'manage');
        case 'power': return togglePower(vm, btn);
      }
    });
  });
}

async function togglePower(vm, btn) {
  if (!vm) return;
  const running = vm.status === 2;
  try {
    await withBusy(btn, () => api('POST', `/api/vms/${vm.vm_id}/${running ? 'stop' : 'start'}`, running ? { force: false } : undefined));
    toast(running ? '已发送停止指令' : '已发送启动指令');
    await refreshVMs();
    if (state.activeVm === vm.vm_id) renderDrawerHead();
  } catch (e) { toastErr(e); }
}

$('vmSearch').addEventListener('input', (e) => { state.search = e.target.value; renderVMs(); });
qsa('#vmFilter .chip').forEach((chip) => chip.addEventListener('click', () => {
  state.filter = chip.dataset.filter;
  qsa('#vmFilter .chip').forEach((c) => c.setAttribute('aria-pressed', String(c === chip)));
  renderVMs();
}));
qsa('.sort-btn').forEach((btn) => btn.addEventListener('click', () => {
  const f = btn.dataset.sort;
  if (state.sort.field === f) state.sort.order = state.sort.order === 'desc' ? 'asc' : 'desc';
  else { state.sort.field = f; state.sort.order = 'desc'; }
  qsa('.sort-btn').forEach((b) => b.dataset.active = String(b.dataset.sort === state.sort.field));
  renderVMs();
}));
$('vmRefresh').addEventListener('click', (e) => withBusy(e.currentTarget, async () => { await refreshVMs(); toast('已刷新'); }));
$('ovRefresh').addEventListener('click', (e) => withBusy(e.currentTarget, async () => { await refreshStatus(); await refreshVMs(); toast('已刷新'); }));

// 手动测速：后端异步跑约 10 秒，进度与结果都由 2 秒一次的 /api/status 轮询带回来，
// 这里只负责触发与即时反馈。
$('btnSpeedTest').addEventListener('click', async (e) => {
  const btn = e.currentTarget;
  if (btn.disabled) return;
  btn.disabled = true;
  btn.classList.add('is-busy');
  $('mNetBw').textContent = '测速中…';
  try {
    await api('POST', '/api/speedtest');
  } catch (err) {
    toastErr(err);
  }
  await refreshStatus();
});

/* ══════════════════════════════ 抽屉 ═══════════════════════════════════════ */
function openDrawer(vmId, tab = 'monitor') {
  state.activeVm = vmId;
  $('scrim').hidden = false;
  requestAnimationFrame(() => $('scrim').classList.add('show'));
  $('drawer').hidden = false;
  $('drawer').setAttribute('aria-hidden', 'false');
  requestAnimationFrame(() => $('drawer').classList.add('show'));
  renderDrawerHead();
  setDrawerTab(tab);
  document.body.style.overflow = 'hidden';
}

function closeDrawer() {
  $('scrim').classList.remove('show');
  $('drawer').classList.remove('show');
  $('drawer').setAttribute('aria-hidden', 'true');
  document.body.style.overflow = '';
  state.pgTimers.forEach(clearInterval);
  state.pgTimers = [];
  state.activeVm = null;
  setTimeout(() => { $('scrim').hidden = true; $('drawer').hidden = true; }, 320);
}

function renderDrawerHead() {
  const vm = state.vms.find((v) => v.vm_id === state.activeVm);
  if (!vm) return;
  $('dwName').textContent = vm.vm_id;
  $('dwIp').textContent = (vm.ips && vm.ips.length) ? vm.ips.join(' · ') : '无 IP';
  $('dwStatus').innerHTML = statusBadge(vm.status);
  $('dwCpu').textContent = num1(vm.cpu_pct);
  $('dwRam').textContent = `${vm.ram_used_mb || 0} / ${vm.ram_total_mb || 0}`;
  $('dwRamUnit').textContent = 'MB';
  const total = (vm.monthly_traffic_in || 0) + (vm.monthly_traffic_out || 0);
  $('dwTraffic').textContent = fmtBytes(total).split(' ')[0];
  $('dwTrafficUnit').textContent = fmtBytes(total).split(' ')[1] || '';
  const running = vm.status === 2;
  $('mgStart').disabled = running;
  $('mgStop').disabled = !running;
  $('mgRestart').disabled = !running;
}

function setDrawerTab(tab) {
  state.drawerTab = tab;
  state.pgTimers.forEach(clearInterval);
  state.pgTimers = [];
  qsa('#dwTabs .tab').forEach((t) => t.setAttribute('aria-selected', String(t.dataset.dtab === tab)));
  ['monitor', 'ports', 'wg', 'manage'].forEach((t) => { $(`dtab-${t}`).hidden = t !== tab; });

  if (tab === 'monitor') { loadMonitor(); state.pgTimers.push(setInterval(loadMonitor, 3000)); }
  if (tab === 'ports') { refreshPorts(); loadIPLimit(true); state.pgTimers.push(setInterval(() => { refreshPorts(); loadIPLimit(false); }, 3000)); }
  if (tab === 'wg') { refreshWG(); state.pgTimers.push(setInterval(refreshWG, 3000)); }
  if (tab === 'manage') populateImageSelect('mgImage');
}

qsa('#dwTabs .tab').forEach((t) => t.addEventListener('click', () => setDrawerTab(t.dataset.dtab)));
$('dwClose').addEventListener('click', closeDrawer);
$('scrim').addEventListener('click', closeDrawer);
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  // 终端的 Esc = 最小化（会话保持连接），不拆会话
  if (!$('console').hidden) return minimizeConsole();
  if (!$('drawer').hidden) return closeDrawer();
  const open = qsa('.modal').find((m) => !m.hidden);
  if (open) open.hidden = true;
});

/* ── 监控图表 ─────────────────────────────────────────────────────────────── */
async function loadMonitor() {
  if (!state.activeVm) return;
  try {
    const hist = (await api('GET', `/api/monitor/history?vm_id=${encodeURIComponent(state.activeVm)}`)) || [];
    state.monitorHist = hist;
    drawMonitorCharts();
    if (hist.length) {
      const last = hist[hist.length - 1];
      $('lgCpu').textContent = `${num1(last.cpu_pct)}%`;
      $('lgMem').textContent = `${num1(last.ram_pct)}%`;
      $('lgRx').textContent = fmtBps(last.net_in_bps);
      $('lgTx').textContent = fmtBps(last.net_out_bps);
    }
  } catch { /* 静默 */ }
}

function drawMonitorCharts() {
  const hist = state.monitorHist || [];
  const css = getComputedStyle(document.documentElement);
  const ink = css.getPropertyValue('--ink-0').trim() || '#111';
  const blue = css.getPropertyValue('--blue').trim() || '#2e90fa';
  const green = css.getPropertyValue('--green').trim() || '#2fb350';

  drawChart($('chartCpu'), [
    { vals: hist.map((h) => h.cpu_pct), color: ink, fill: ink },
    { vals: hist.map((h) => h.ram_pct), color: blue, fill: blue },
  ], { max: 100, unit: '%' });

  drawChart($('chartNet'), [
    { vals: hist.map((h) => h.net_in_bps), color: blue, fill: blue, bps: true },
    { vals: hist.map((h) => h.net_out_bps), color: green, fill: green, bps: true },
  ], { unit: '', bps: true });
}

function drawChart(svg, series, opts = {}) {
  if (!svg) return;
  const w = Math.max(240, svg.clientWidth || 560);
  const h = Math.max(90, svg.clientHeight || 128);
  const padT = 6, padB = 4;
  const innerH = h - padT - padB;

  const allVals = series.flatMap((s) => s.vals.filter((v) => Number.isFinite(v)));
  let max = opts.max ?? Math.max(1, ...allVals);
  if (opts.bps) max = niceMax(Math.max(1, ...allVals));
  max = Math.max(max, 1);

  const grid = [0, 1, 2, 3, 4].map((i) => {
    const y = padT + (innerH / 4) * i;
    return `<line x1="0" y1="${y}" x2="${w}" y2="${y}" stroke="currentColor" stroke-opacity=".07" stroke-width="1"/>`;
  }).join('');

  const paths = series.map((s, si) => {
    const n = s.vals.length;
    if (!n) return '';
    const pts = s.vals.map((v, i) => {
      const x = n === 1 ? 0 : (i / (n - 1)) * w;
      const y = padT + innerH - (clamp(Number(v) || 0, 0, max) / max) * innerH;
      return [x, y];
    });
    const line = smoothPath(pts);
    const area = `${line} L ${w} ${padT + innerH} L 0 ${padT + innerH} Z`;
    const gid = `grad-${svg.id}-${si}`;
    return `
      <defs>
        <linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${s.fill}" stop-opacity=".18"/>
          <stop offset="100%" stop-color="${s.fill}" stop-opacity="0"/>
        </linearGradient>
      </defs>
      <path d="${area}" fill="url(#${gid})"/>
      <path d="${line}" fill="none" stroke="${s.color}" stroke-width="1.7" stroke-linejoin="round" stroke-linecap="round" opacity=".95"/>`;
  }).join('');

  svg.setAttribute('viewBox', `0 0 ${w} ${h}`);
  svg.innerHTML = grid + paths;

  // 悬停
  if (!svg.dataset.hoverBound) {
    svg.dataset.hoverBound = '1';
    bindChartHover(svg);
  }
}

function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
}

function smoothPath(pts) {
  if (!pts.length) return '';
  if (pts.length === 1) return `M ${pts[0][0]} ${pts[0][1]}`;
  let d = `M ${pts[0][0]} ${pts[0][1]}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const [x0, y0] = pts[i], [x1, y1] = pts[i + 1];
    const cx = (x0 + x1) / 2;
    d += ` C ${cx} ${y0}, ${cx} ${y1}, ${x1} ${y1}`;
  }
  return d;
}

let tipEl = null;
function bindChartHover(svg) {
  svg.addEventListener('mousemove', (e) => {
    const hist = state.monitorHist || [];
    if (!hist.length) return;
    const rect = svg.getBoundingClientRect();
    const ratio = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    const idx = Math.round(ratio * (hist.length - 1));
    const h = hist[idx];
    if (!h) return;
    if (!tipEl) {
      tipEl = document.createElement('div');
      tipEl.className = 'chart-tip';
      document.body.appendChild(tipEl);
    }
    const isNet = svg.id === 'chartNet';
    tipEl.textContent = isNet
      ? `RX ${fmtBps(h.net_in_bps)}\nTX ${fmtBps(h.net_out_bps)}`
      : `CPU ${num1(h.cpu_pct)}%\n内存 ${num1(h.ram_pct)}%`;
    tipEl.style.left = `${e.clientX + 12}px`;
    tipEl.style.top = `${e.clientY - 10}px`;
    tipEl.style.display = 'block';
  });
  svg.addEventListener('mouseleave', () => { if (tipEl) tipEl.style.display = 'none'; });
}

/* ── 端口转发 ─────────────────────────────────────────────────────────────── */
async function refreshPorts() {
  if (!state.activeVm) return;
  try {
    const rules = (await api('GET', `/api/vms/${state.activeVm}/portfwds`)) || [];
    state.pfCache = rules;
    const list = $('pfList');
    $('pfEmpty').hidden = rules.length > 0;
    if (!rules.length) { list.innerHTML = ''; $('pfSummary').textContent = ''; return; }

    const active = rules.reduce((s, r) => s + (r.active_conns || 0), 0);
    const total = rules.reduce((s, r) => s + (r.total_conns || 0), 0);
    $('pfSummary').textContent = `${rules.length} 条 · ${active} 活跃 · ${total} 累计`;

    list.innerHTML = rules.map((r) => {
      const active = r.active_conns || 0;
      const total = r.total_conns || 0;
      const allIPs = r.top_ips || [];
      // 默认只铺前 3 个来源 IP，其余收成 +N，避免 IP 多时把行撑高
      const shown = allIPs.slice(0, 3);
      const restCount = allIPs.length - shown.length;
      const ipBadges = shown.map((ip) => {
        const a = ip.active_count || 0;
        return `<span class="pf-ip${a ? ' is-active' : ''}"><b>${a ? '●' + a : '·'}</b>${esc(ip.ip)}<i>${ip.count}</i></span>`;
      }).join('') + (restCount > 0 ? `<span class="pf-ip more">+${restCount}</span>` : '');

      const famLabel = r.bind_family === 1 ? 'v6' : (r.bind_family === 2 ? 'v4+v6' : '');
      return `
      <div class="pf-row">
        <div class="pf-main">
          <span class="pf-proto">${esc(r.protocol)}</span>
          ${famLabel ? `<span class="pf-fam">${famLabel}</span>` : ''}
          <span class="mono pf-port">:${r.host_port}</span>
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="dim"><use href="#i-arrow-right"/></svg>
          <span class="pf-target">${esc(r.target_addr || ':' + r.guest_port)}</span>
          ${r.description ? `<span class="pf-desc">${esc(r.description)}</span>` : ''}
          <span class="spacer"></span>
          <span class="pf-stat${active ? ' is-ok' : ''}">${active} 活跃</span>
          <span class="pf-stat">${total} 累计</span>
          <button class="act danger" data-del-pf="${esc(r.protocol)}:${r.host_port}" title="删除">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
          </button>
        </div>
        ${allIPs.length ? `<div class="pf-ips">${ipBadges}</div>` : ''}
      </div>`;
    }).join('');

    qsa('[data-del-pf]').forEach((btn) => btn.addEventListener('click', async () => {
      const [proto, port] = btn.dataset.delPf.split(':');
      if (!confirm(`删除 ${proto.toUpperCase()} :${port} 的转发规则？`)) return;
      try {
        await api('DELETE', `/api/vms/${state.activeVm}/portfwds/${proto}/${port}`);
        toast('规则已删除');
        refreshPorts();
      } catch (e) { toastErr(e); }
    }));
  } catch (e) { $('pfErr2').textContent = e.message; $('pfErr2').classList.add('show'); }
}

$('pfOpenModal').addEventListener('click', () => {
  $('modalPF').hidden = false;
  $('pfErr').classList.remove('show');
  setPFMode('single');
  $('pfHost').value = ''; $('pfGuest').value = ''; $('pfBatchText').value = '';
  // 地址族选择只在共享 IPv6（snat 模式）下出现；其余情况固定 IPv4。
  const conf = state.config || {};
  const hasV6 = conf.ipv6_mode === 'snat' && !!(conf.shared_ipv6 || conf.public_ipv6 || conf.ipv6_addr);
  $('pfBindField').hidden = !hasV6;
  $('pfBatchBindField').hidden = !hasV6;
  $('pfBind').value = '0';
  $('pfBatchBind').value = '0';
  setTimeout(() => $('pfHost').focus(), 60);
});

function setPFMode(mode) {
  qsa('.pf-seg-btn').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.pfmode === mode)));
  $('pfSingle').hidden = mode !== 'single';
  $('pfBatch').hidden = mode !== 'batch';
}
qsa('.pf-seg-btn').forEach((b) => b.addEventListener('click', () => setPFMode(b.dataset.pfmode)));

// 批量文本解析：每行 [协议:]宿主:VM，协议缺省为 tcp；也接受 host->guest / host guest。
function parseBatchPorts(text) {
  const out = [];
  for (const raw of String(text || '').split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const m = line.match(/^(?:([a-zA-Z]{3})\s*:\s*)?(\d{1,5})\s*(?:[:>]\s*|\s+)(\d{1,5})$/);
    if (!m) { out.push({ err: line }); continue; }
    const proto = m[1] ? m[1].toLowerCase() : 'tcp';
    if (proto !== 'tcp' && proto !== 'udp') { out.push({ err: line }); continue; }
    out.push({ protocol: proto, host: Number(m[2]), guest: Number(m[3]) });
  }
  return out;
}

async function submitPortFwds(items) {
  for (const it of items) {
    await api('POST', `/api/vms/${state.activeVm}/portfwds`, it);
  }
}

$('pfAdd').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  $('pfErr').classList.remove('show');
  const isBatch = $('pfBatch').hidden === false;
  const items = [];
  if (isBatch) {
    const parsed = parseBatchPorts($('pfBatchText').value);
    const bad = parsed.filter((p) => p.err);
    if (bad.length) { $('pfErr').textContent = `无法解析：${bad.map((b) => b.err).join('、')}`; $('pfErr').classList.add('show'); return; }
    if (!parsed.length) { $('pfErr').textContent = '请填写要映射的端口'; $('pfErr').classList.add('show'); return; }
    const fam = Number($('pfBatchBind').value) || 0;
    parsed.forEach((p) => items.push({ protocol: p.protocol, host_port: p.host, guest_port: p.guest, bind_family: fam }));
  } else {
    const body = { protocol: $('pfProto').value, host_port: Number($('pfHost').value), guest_port: Number($('pfGuest').value), bind_family: Number($('pfBind').value) || 0 };
    if (!body.host_port || !body.guest_port) { $('pfErr').textContent = '端口不能为空'; $('pfErr').classList.add('show'); return; }
    items.push(body);
  }
  try {
    await submitPortFwds(items);
    $('modalPF').hidden = true;
    toast(items.length > 1 ? `已添加 ${items.length} 条转发规则` : '转发规则已添加');
    refreshPorts();
  } catch (err) { $('pfErr').textContent = err.message; $('pfErr').classList.add('show'); }
}));

async function loadIPLimit(fill) {
  if (!state.activeVm) return;
  try {
    const info = await api('GET', `/api/vms/${state.activeVm}/iplimit`);
    if (fill) $('ipLimitValue').value = info.override === null ? '' : info.override;
    $('ipLimitStatus').textContent = `生效 ${info.effective > 0 ? info.effective : '不限'} · 全局 ${info.global > 0 ? info.global : '不限'}`;
  } catch { /* 静默 */ }
}
$('ipLimitSet').addEventListener('click', async () => {
  const v = Number($('ipLimitValue').value);
  if ($('ipLimitValue').value === '') return toast('请填数字，或点「用全局」', 'bad');
  try { await api('PUT', `/api/vms/${state.activeVm}/iplimit`, { max_ips: v, use_global: false }); toast('已设置'); loadIPLimit(false); }
  catch (e) { toastErr(e); }
});
$('ipLimitGlobal').addEventListener('click', async () => {
  try { await api('PUT', `/api/vms/${state.activeVm}/iplimit`, { use_global: true }); $('ipLimitValue').value = ''; toast('已恢复全局'); loadIPLimit(false); }
  catch (e) { toastErr(e); }
});

/* ── WireGuard ────────────────────────────────────────────────────────────── */
const wgField = (id) => $(id).value.trim();

async function refreshWG() {
  if (!state.activeVm) return;
  try {
    const list = (await api('GET', `/api/vms/${state.activeVm}/wg`)) || [];
    state.wgCache = list;
    $('wgEmpty').hidden = list.length > 0;
    if (!list.length) { $('wgList').innerHTML = ''; $('wgSummary').textContent = ''; return; }
    const running = list.filter((b) => b.state === 'running').length;
    const active = list.reduce((s, b) => s + (b.active_conns || 0), 0);
    $('wgSummary').textContent = `${list.length} 条 · ${running} 运行 · ${active} 活跃`;

    $('wgList').innerHTML = list.map((b) => {
      const fresh = b.last_handshake && nowSec() - b.last_handshake < 180;
      const stateCls = b.state === 'running' ? 'badge-ok' : b.state === 'error' ? 'badge-bad' : 'badge-quiet';
      return `
      <div style="padding:12px;border-radius:10px;border:1px solid var(--hairline);margin:4px 0">
        <div class="row" style="gap:8px;flex-wrap:wrap">
          <span class="badge ${stateCls}">${esc(b.state)}</span>
          <span class="mono small" style="font-weight:600">${esc(b.address)}</span>
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="dim"><use href="#i-arrow-right"/></svg>
          <span class="mono small muted">${esc(b.target_ip || 'VM')} · 同端口</span>
          ${b.name ? `<span class="dim small">${esc(b.name)}</span>` : ''}
          <span class="spacer"></span>
          <button class="act" data-wg-toggle="${esc(b.id)}" data-enable="${!b.enabled}" title="${b.enabled ? '停用' : '启用'}">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#${b.enabled ? 'i-pause' : 'i-play'}"/></svg>
          </button>
          <button class="act" data-wg-edit="${esc(b.id)}" title="编辑">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M4 20h4L20 8l-4-4L4 16v4z"/></svg>
          </button>
          <button class="act danger" data-wg-del="${esc(b.id)}" title="删除">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
          </button>
        </div>
        <div class="row mono" style="gap:12px;flex-wrap:wrap;margin-top:8px;font-size:10.5px;color:var(--ink-2)">
          <span>udp :${b.actual_port || b.listen_port || '?'}</span>
          <span>peer ${esc(b.peer_endpoint || b.endpoint || 'passive')}</span>
          <span style="color:${fresh ? 'var(--green-ink)' : 'var(--ink-2)'};font-weight:600">握手 ${ago(b.last_handshake)}</span>
          <span>↓ ${fmtBytes(b.rx_bytes)} / ↑ ${fmtBytes(b.tx_bytes)}</span>
          <span>mtu ${b.mtu}</span>
        </div>
        ${b.error || b.last_dial_err ? `<div class="small" style="margin-top:6px;color:var(--red-ink)">${esc(b.error || b.last_dial_err)}</div>` : ''}
      </div>`;
    }).join('');

    qsa('[data-wg-del]').forEach((btn) => btn.addEventListener('click', async () => {
      if (!confirm('删除这条 WireGuard 绑定？')) return;
      try { await api('DELETE', `/api/vms/${state.activeVm}/wg/${btn.dataset.wgDel}`); toast('已删除'); refreshWG(); }
      catch (e) { toastErr(e); }
    }));
    qsa('[data-wg-toggle]').forEach((btn) => btn.addEventListener('click', async () => {
      try {
        await api('PUT', `/api/vms/${state.activeVm}/wg/${btn.dataset.wgToggle}`, { enabled: btn.dataset.enable === 'true' });
        refreshWG();
      } catch (e) { toastErr(e); }
    }));
    qsa('[data-wg-edit]').forEach((btn) => btn.addEventListener('click', () => editWG(btn.dataset.wgEdit)));
  } catch (e) { $('wgErr').textContent = e.message; $('wgErr').classList.add('show'); }
}

function resetWGForm() {
  state.wgEditId = null;
  ['wgName', 'wgAddress', 'wgPriv', 'wgPeer', 'wgEndpoint', 'wgPort', 'wgPsk', 'wgAllowed', 'wgMtu', 'wgKeepalive'].forEach((id) => { $(id).value = ''; });
  $('wgEnabled').checked = true;
  $('wgFormTitle').textContent = '新增绑定';
  $('wgSubmit').textContent = '添加绑定';
  $('wgCancelEdit').hidden = true;
  $('wgPubHint').style.display = 'none';
  $('wgKeyHint').textContent = '';
  $('wgPriv').placeholder = 'base64, 44 chars';
}

function editWG(id) {
  const b = state.wgCache.find((x) => x.id === id);
  if (!b) return;
  state.wgEditId = id;
  $('wgName').value = b.name || '';
  $('wgAddress').value = b.address || '';
  $('wgPriv').value = '';
  $('wgPriv').placeholder = '留空 = 沿用当前私钥';
  $('wgPeer').value = b.peer_public_key || '';
  $('wgEndpoint').value = b.endpoint || '';
  $('wgPort').value = b.listen_port || '';
  $('wgPsk').value = '';
  $('wgAllowed').value = b.allowed_ips || '';
  $('wgMtu').value = b.mtu || '';
  $('wgKeepalive').value = b.keepalive || '';
  $('wgEnabled').checked = !!b.enabled;
  $('wgFormTitle').textContent = '编辑绑定';
  $('wgSubmit').textContent = '保存改动';
  $('wgCancelEdit').hidden = false;
  $('wgKeyHint').textContent = b.has_psk ? '· PSK 已设置（留空保留）' : '';
  $('wgPubHint').textContent = `本端公钥 ${b.public_key || '?'}`;
  $('wgPubHint').style.display = 'block';
}

$('wgCancelEdit').addEventListener('click', resetWGForm);
$('wgGenKey').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  try {
    const kp = await api('POST', '/api/wg/keypair', {});
    $('wgPriv').value = kp.private_key;
    $('wgPubHint').textContent = `本端公钥 ${kp.public_key} —— 填到对端的 Peer PublicKey`;
    $('wgPubHint').style.display = 'block';
  } catch (e2) { toastErr(e2); }
}));

$('wgSubmit').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  const body = {
    name: wgField('wgName'),
    address: wgField('wgAddress'),
    private_key: wgField('wgPriv'),
    peer_public_key: wgField('wgPeer'),
    endpoint: wgField('wgEndpoint'),
    listen_port: Number($('wgPort').value) || 0,
    preshared_key: wgField('wgPsk'),
    allowed_ips: wgField('wgAllowed'),
    mtu: Number($('wgMtu').value) || 0,
    keepalive: Number($('wgKeepalive').value) || 0,
    enabled: $('wgEnabled').checked,
  };
  $('wgErr').classList.remove('show');
  if (!body.address) { $('wgErr').textContent = '隧道地址必填'; $('wgErr').classList.add('show'); return; }
  if (!state.wgEditId && (!body.private_key || !body.peer_public_key)) {
    $('wgErr').textContent = '私钥与对端公钥必填'; $('wgErr').classList.add('show'); return;
  }
  try {
    if (state.wgEditId) await api('PUT', `/api/vms/${state.activeVm}/wg/${state.wgEditId}`, body);
    else await api('POST', `/api/vms/${state.activeVm}/wg`, body);
    toast(state.wgEditId ? '绑定已更新' : '绑定已添加');
    resetWGForm();
    refreshWG();
  } catch (err) { $('wgErr').textContent = err.message; $('wgErr').classList.add('show'); }
}));

/* ── 管理 ─────────────────────────────────────────────────────────────────── */
$('mgStart').addEventListener('click', (e) => doVmAction(e.currentTarget, 'start'));
$('mgStop').addEventListener('click', (e) => doVmAction(e.currentTarget, 'stop', { force: false }));
$('mgRestart').addEventListener('click', (e) => doVmAction(e.currentTarget, 'restart'));
$('mgConsole').addEventListener('click', () => openConsole(state.activeVm));

async function doVmAction(btn, action, body) {
  try {
    await withBusy(btn, () => api('POST', `/api/vms/${state.activeVm}/${action}`, body));
    toast('操作已下发');
    await refreshVMs();
    renderDrawerHead();
  } catch (e) { toastErr(e); }
}

$('mgPwdBtn').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  const pwd = $('mgPwd').value;
  if (!pwd) return toast('请输入新密码', 'bad');
  try { await api('POST', `/api/vms/${state.activeVm}/reset-password`, { password: pwd }); $('mgPwd').value = ''; toast('密码已重置'); }
  catch (err) { toastErr(err); }
}));

$('mgReinstall').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  const image = $('mgImage').value, pwd = $('mgRePwd').value;
  if (!confirm(`重装会清空 ${state.activeVm} 的全部数据，确认继续？`)) return;
  try {
    await api('POST', `/api/vms/${state.activeVm}/reinstall`, { os_image: image, root_password: pwd });
    toast('重装已开始');
    refreshVMs();
  } catch (err) { toastErr(err); }
}));

$('mgResetTraffic').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  if (!confirm('清零这台 VM 的流量统计？')) return;
  try { await api('POST', `/api/vms/${state.activeVm}/reset-traffic`); toast('流量已重置'); refreshVMs(); }
  catch (err) { toastErr(err); }
}));

$('mgDelete').addEventListener('click', async () => {
  const id = state.activeVm;
  if (!confirm(`删除虚拟机 ${id}？此操作不可恢复。`)) return;
  try {
    await api('DELETE', `/api/vms/${id}`);
    toast('已删除');
    closeDrawer();
    refreshVMs();
  } catch (e) { toastErr(e); }
});

/* ══════════════════════════════ 防火墙 ═════════════════════════════════════ */
async function refreshFirewall() {
  await Promise.all([checkRfw(), refreshRules()]);
}

async function checkRfw() {
  try {
    const d = await api('GET', '/api/rfw/online');
    const badge = $('rfwBadge');
    if (d.online) {
      badge.className = 'badge badge-ok';
      badge.innerHTML = '<span class="dot"></span>运行中';
      $('rfwOnline').hidden = false;
      $('rfwOffline').hidden = true;
      $('rfwIface').textContent = d.status?.iface || '—';
      $('rfwRuleCount').textContent = d.status?.rule_count ?? '—';
      $('rfwApiVer').textContent = d.status?.api_version || '—';
    } else {
      badge.className = 'badge badge-bad';
      badge.innerHTML = '<span class="dot"></span>未运行';
      $('rfwOnline').hidden = true;
      $('rfwOffline').hidden = false;
    }
  } catch {
    const badge = $('rfwBadge');
    badge.className = 'badge badge-quiet';
    badge.innerHTML = '<span class="dot"></span>不可用';
  }
}

async function refreshRules() {
  try {
    const rules = (await api('GET', '/rfw/api/rules')) || [];
    const tbody = $('rfwTbody');
    $('rfwEmpty').hidden = rules.length > 0;
    if (!rules.length) { tbody.innerHTML = ''; return; }
    tbody.innerHTML = rules.map((r) => {
      const port = r.port_start === r.port_end ? r.port_start : `${r.port_start}–${r.port_end}`;
      const ipCell = r.ip_type === 'any'
        ? '<span class="dim small">any</span>'
        : r.ip_type === 'cidr'
          ? `<code class="mono small">${esc(r.ip)}</code>`
          : (r.countries || []).map((c) => `<span class="badge badge-quiet">${esc(c)}</span>`).join(' ');
      return `
      <tr>
        <td class="mono-cell dim">#${r.id}</td>
        <td class="num-cell">${r.priority}</td>
        <td><span class="badge ${r.enabled ? 'badge-ok' : 'badge-quiet'}">${r.enabled ? 'on' : 'off'}</span></td>
        <td><span class="badge ${r.direction === 'in' ? 'badge-blue' : 'badge-warn'}">${r.direction === 'in' ? '入站' : '出站'}</span></td>
        <td><span class="badge badge-quiet badge-mono">${esc(r.protocol)}</span></td>
        <td class="mono-cell">${port}</td>
        <td>${ipCell}</td>
        <td><span class="badge ${r.action === 'block' ? 'badge-bad' : 'badge-ok'}">${esc(r.action)}</span></td>
        <td style="text-align:right">
          <button class="act danger" data-del-rule="${r.id}" title="删除规则">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
          </button>
        </td>
      </tr>`;
    }).join('');

    qsa('[data-del-rule]').forEach((btn) => btn.addEventListener('click', async () => {
      if (!confirm(`删除规则 #${btn.dataset.delRule}？`)) return;
      try {
        const res = await fetch(`/rfw/api/rules/${btn.dataset.delRule}`, { method: 'DELETE' });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        toast('规则已删除');
        refreshRules();
      } catch (e) { toastErr(e); }
    }));
  } catch (e) {
    $('rfwTbody').innerHTML = `<tr><td colspan="9" class="empty" style="padding:28px">读取规则失败：${esc(e.message)}</td></tr>`;
  }
}
$('rfwRefresh').addEventListener('click', (e) => withBusy(e.currentTarget, async () => { await refreshFirewall(); toast('已刷新'); }));

/* ── 安全：登录日志与封禁 ─────────────────────────────────────────────────── */
let banHours = 0; // 0 = 永久
qsa('#banHours .btn').forEach((btn) => btn.addEventListener('click', () => {
  banHours = Number(btn.dataset.h) || 0;
  qsa('#banHours .btn').forEach((b) => b.classList.toggle('active', b === btn));
}));

function banMeta(action) {
  switch (action) {
    case 'login_ok':      return { label: '成功', cls: 'badge-ok' };
    case 'login_fail':    return { label: '失败', cls: 'badge-warn' };
    case 'auto_ban':      return { label: '自动锁定', cls: 'badge-bad' };
    case 'login_blocked': return { label: '被封仍访问', cls: 'badge-bad' };
    case 'manual_ban':    return { label: '手动封禁', cls: 'badge-blue' };
    case 'unban':         return { label: '解封', cls: 'badge-quiet' };
    default:              return { label: action, cls: 'badge-quiet' };
  }
}

function renderBans(bans) {
  const tbody = $('banTbody');
  $('banEmpty').hidden = bans.length > 0;
  // 空态时收起表头（与巡查异常列表同款处理）
  $('banTbody').closest('.table-wrap').style.display = bans.length ? '' : 'none';
  $('banCount').textContent = bans.length;
  if (!bans.length) { tbody.innerHTML = ''; return; }
  tbody.innerHTML = bans.map((b) => {
    // 到期显示剩余时间：ago() 是"距今多久"，对未来的时间戳会算出负数恒显「1 秒前」
    let until;
    if (b.until > 0) {
      const left = b.until - nowSec();
      const abs = new Date(b.until * 1000).toLocaleString();
      const rel = left <= 0 ? '已过期'
        : left < 60 ? `${left} 秒后`
        : left < 3600 ? `${Math.floor(left / 60)} 分钟后`
        : left < 86400 ? `${Math.floor(left / 3600)} 小时后`
        : `${Math.floor(left / 86400)} 天后`;
      until = `${rel}<div class="dim small" title="${abs}">${abs}</div>`;
    } else {
      until = '<span class="badge badge-bad">永久</span>';
    }
    const src = b.source === 'manual'
      ? '<span class="badge badge-blue">手动</span>'
      : '<span class="badge badge-warn">自动</span>';
    return `
    <tr>
      <td class="mono-cell">${esc(b.ip)}</td>
      <td>${src}</td>
      <td class="dim small">${b.reason ? esc(b.reason) : '—'}</td>
      <td>${until}</td>
      <td style="text-align:right">
        <button class="act danger" data-unban="${esc(b.ip)}" title="解封">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-x"/></svg>
        </button>
      </td>
    </tr>`;
  }).join('');
  qsa('[data-unban]').forEach((btn) => btn.addEventListener('click', async () => {
    if (!confirm(`解封 ${btn.dataset.unban}？`)) return;
    try {
      await api('DELETE', `/api/security/ban/${encodeURIComponent(btn.dataset.unban)}`);
      toast('已解封');
      refreshSecurity();
    } catch (e) { toastErr(e); }
  }));
}

// ── 加载骨架：异步面板首次加载时的占位，防止数据到达时卡片突然弹高 ──────────
// 跳过条件：容器已有内容（切页返回时原地等待，不闪烁），或空态已展示过
// （上一轮结果是空的，重进时保持空态不闪骨架）。emptyEl 是同区域的空态元素。
function showSkeleton(host, emptyEl, html) {
  if (!host || host.children.length) return;
  if (emptyEl && !emptyEl.hidden) return;
  if (emptyEl) emptyEl.hidden = true;
  host.innerHTML = html;
}
// 紧凑列表骨架（摘要卡/铃铛小窗）：圆点 + 两行长短不一的条 + 尾部时间条
function skeletonMiniRows(n = 4) {
  const row = (a, b) => `<div class="skel-row"><span class="skel-dot"></span><div class="skel-main"><span class="skel-bar" style="width:${a}%"></span><span class="skel-bar" style="width:${b}%"></span></div><span class="skel-bar skel-tail"></span></div>`;
  return Array.from({ length: n }, (_, i) => row(74 - (i % 3) * 10, 42 + (i % 2) * 14)).join('');
}
// 表格骨架（异常 VM/当前封禁/通知抽屉）：每行每列一根条，宽度循环错开
function skeletonTableRows(cols, n = 4) {
  const w = [34, 18, 14, 26, 40, 22];
  return Array.from({ length: n }, () =>
    '<tr>' + Array.from({ length: cols }, (_, c) => `<td><span class="skel-bar" style="width:${w[c % w.length]}%"></span></td>`).join('') + '</tr>'
  ).join('');
}

async function refreshSecurity() {
  if (state.view !== 'security') return;
  try {
    showSkeleton($('secLogMini'), null, skeletonMiniRows(3));
    showSkeleton($('banTbody'), $('banEmpty'), skeletonTableRows(5));
    const [bans, log] = await Promise.all([
      api('GET', '/api/security/bans'),
      api('GET', '/api/security/log?limit=6'),
    ]);
    renderBans(bans || []);
    renderSecLogMini(log || []);
  } catch { /* 会话失效走全局 401 处理 */ }
}

$('btnBan').addEventListener('click', async (e) => {
  const ip = $('banIp').value.trim();
  if (!ip) return toast('请输入 IP 地址', 'bad');
  await withBusy(e.currentTarget, async () => {
    await api('POST', '/api/security/ban', { ip, hours: banHours, reason: $('banReason').value.trim() });
    toast(`已封禁 ${ip}`);
    $('banIp').value = '';
    $('banReason').value = '';
    refreshSecurity();
  });
});
$('secRefresh').addEventListener('click', (e) => withBusy(e.currentTarget, async () => { await refreshSecurity(); toast('已刷新'); }));

$('rIpType').addEventListener('change', (e) => {
  $('rCidrField').hidden = e.target.value !== 'cidr';
  $('rGeoField').hidden = e.target.value !== 'geoip';
});
$('btnNewRule').addEventListener('click', () => {
  $('formRule').reset();
  $('rCidrField').hidden = true;
  $('rGeoField').hidden = true;
  $('ruleErr').classList.remove('show');
  $('modalRule').hidden = false;
});
$('formRule').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('ruleErr').classList.remove('show');
  const fd = new FormData(e.target);
  const body = {
    priority: Number(fd.get('priority')),
    direction: fd.get('direction'),
    protocol: fd.get('protocol'),
    action: fd.get('action'),
    enabled: fd.get('enabled') === 'true',
    port_start: Number(fd.get('port_start')) || 0,
    port_end: Number(fd.get('port_end')) || Number(fd.get('port_start')) || 0,
    ip_type: fd.get('ip_type'),
    ip: fd.get('ip') || '',
    countries: (fd.get('countries') || '').split(',').map((s) => s.trim()).filter(Boolean),
  };
  try {
    await withBusy($('ruleSubmit'), () => api('POST', '/rfw/api/rules', body));
    $('modalRule').hidden = true;
    toast('规则已创建');
    refreshRules();
    checkRfw();
  } catch (err) { $('ruleErr').textContent = err.message; $('ruleErr').classList.add('show'); }
});

/* ══════════════════════════════ 巡查 ═══════════════════════════════════════ */
const PATROL_SEEN_KEY = 'run-patrol-seen';
const PATROL_PAGE_SIZE = 20;

const patrolState = {
  tab: 'overview',
  cat: 'basic',
  vms: [],
  anomalies: [],
  blacklist: [],
};

function patrolCheckMeta(check) {
  switch (check) {
    case 'bandwidth':     return '带宽';
    case 'connections':   return '连接';
    case 'resources_cpu': return 'CPU';
    case 'resources_mem': return '内存';
    case 'resources_disk':return '磁盘';
    case 'process':       return '进程';
    default:              return check || '—';
  }
}

function anomalyReasonLabel(a) {
  if (a.type === 'stopped') {
    if (a.reason === 'resources') return '资源超限停机';
    if (a.reason === 'process') return '进程黑名单停机';
    if ((a.reason || '').startsWith('abuse_')) return '滥用识别停机';
    return '连接超限停机';
  }
  return a.reason === 'connections' ? '连接数超限' : '带宽超限';
}

function patrolActionMeta(action) {
  switch (action) {
    case 'throttle':     return { label: '限速', cls: 'badge-warn' };
    case 'release':      return { label: '解除', cls: 'badge-quiet' };
    case 'stop_vm':      return { label: '停止', cls: 'badge-bad' };
    case 'restart_vm':   return { label: '重启', cls: 'badge-blue' };
    case 'kill_process': return { label: '杀进程', cls: 'badge-bad' };
    case 'alert':        return { label: '告警', cls: 'badge-warn' };
    case 'recovered':    return { label: '恢复', cls: 'badge-ok' };
    default:             return { label: action || '—', cls: 'badge-quiet' };
  }
}

// 事件严重度：stop/kill 为严重，throttle/restart/alert 为警告，release/recovered 为信息。
function patrolSeverity(action) {
  switch (action) {
    case 'stop_vm':
    case 'kill_process': return { label: '严重', cls: 'badge-bad' };
    case 'throttle':
    case 'restart_vm':
    case 'alert':        return { label: '警告', cls: 'badge-warn' };
    default:             return { label: '信息', cls: 'badge-quiet' };
  }
}

function patrolLastSeen() {
  return Number(localStorage.getItem(PATROL_SEEN_KEY)) || 0;
}
function setPatrolSeen(id) {
  localStorage.setItem(PATROL_SEEN_KEY, String(id || 0));
}

// checkPatrolDot 全局轮询最新事件，有未读就在「巡查」tab 上挂红点。
async function checkPatrolDot() {
  try {
    const d = await api('GET', '/api/patrol/events?page=1&page_size=1');
    const latest = d && d.items && d.items.length ? d.items[0].id : 0;
    if (latest > patrolLastSeen()) {
      $('patrolDot').classList.add('on');
    }
  } catch { /* 会话失效走全局 401 */ }
}

function clearPatrolDot() {
  $('patrolDot').classList.remove('on');
}

// markPatrolSeen 拉取全局最新事件 id 并记为已读，清除红点。
async function markPatrolSeen() {
  try {
    const d = await api('GET', '/api/patrol/events?page=1&page_size=1');
    const latest = d && d.items && d.items.length ? d.items[0].id : 0;
    if (latest) setPatrolSeen(latest);
  } catch { /* 忽略 */ }
  clearPatrolDot();
}

function overallPatrolStatus() {
  const n = new Set(patrolState.anomalies.map((a) => a.vm_id)).size;
  if (n === 0) return { label: '正常', dot: 'dot-ok' };
  return { label: '异常', dot: 'dot-bad' };
}

function renderPatrolOverview() {
  const vms = patrolState.vms;
  const running = vms.filter((v) => v.status === 2).length;
  $('pMonVm').textContent = vms.length;
  $('pMonVmMeta').textContent = `运行中 ${running}`;

  const bad = new Set(patrolState.anomalies.map((a) => a.vm_id)).size;
  $('pEvtNum').textContent = bad;

  const st = overallPatrolStatus();
  $('pStatusVal').innerHTML = `<span class="dot ${st.dot}"></span>${st.label}`;

  renderPatrolAnomalies();
}

function renderPatrolAnomalies() {
  const list = patrolState.anomalies;
  const tbody = $('pVmTbody');
  $('pVmEmpty').hidden = list.length > 0;
  // 空态时收起表头：图标空态 + 表头并存是重复噪音
  $('pVmTbody').closest('.table-wrap').style.display = list.length ? '' : 'none';
  $('pVmCount').textContent = new Set(list.map((a) => a.vm_id)).size;
  if (!list.length) { tbody.innerHTML = ''; return; }
  // 按 VM 合并：被停机优先于限速（限速会到时自动解除，停机需人工启动）
  const byVm = new Map();
  for (const a of list) {
    const cur = byVm.get(a.vm_id);
    if (!cur || (a.type === 'stopped' && cur.type !== 'stopped')) byVm.set(a.vm_id, a);
  }
  tbody.innerHTML = [...byVm.values()].map((a) => {
    const sev = a.type === 'stopped' ? { label: '异常', cls: 'badge-bad' } : { label: '警告', cls: 'badge-warn' };
    const reason = anomalyReasonLabel(a);
    let action;
    if (a.type === 'stopped') {
      action = `<button class="btn btn-ghost btn-sm" data-start="${esc(a.vm_id)}">启动</button>`;
    } else {
      const left = (a.release_at || 0) - nowSec();
      const rel = left > 0 ? `${Math.ceil(left / 60)} 分钟后解除` : '即将解除';
      action = `<span class="dim small">${rel}</span>
        <button class="act danger" data-release="${esc(a.vm_id)}" title="解除限速"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-x"/></svg></button>`;
    }
    return `<tr>
      <td class="mono-cell">${esc(a.vm_id)}</td>
      <td><span class="badge ${sev.cls}">${sev.label}</span><span class="dim small"> ${esc(reason)}</span></td>
      <td style="text-align:right">${action}</td>
    </tr>`;
  }).join('');
  qsa('[data-release]').forEach((btn) => btn.addEventListener('click', async () => {
    if (!confirm(`解除 ${btn.dataset.release} 的限速？`)) return;
    try {
      await api('POST', `/api/patrol/throttles/${encodeURIComponent(btn.dataset.release)}/release`);
      toast('已解除限速');
      refreshPatrol();
    } catch (e) { toastErr(e); }
  }));
  qsa('[data-start]').forEach((btn) => btn.addEventListener('click', async () => {
    if (!confirm(`启动 ${btn.dataset.start}？`)) return;
    try {
      await api('POST', `/api/vms/${encodeURIComponent(btn.dataset.start)}/start`);
      toast('已启动');
      refreshPatrol();
    } catch (e) { toastErr(e); }
  }));
}

async function refreshPatrol() {
  if (state.view !== 'patrol') return;
  try {
    showSkeleton($('patrolMini'), null, skeletonMiniRows(3));
    showSkeleton($('pVmTbody'), $('pVmEmpty'), skeletonTableRows(3));
    const [vms, anomalies, mini] = await Promise.all([
      api('GET', '/api/vms'),
      api('GET', '/api/patrol/anomalies'),
      api('GET', '/api/events/feed?source=patrol&page_size=6'),
    ]);
    patrolState.vms = (vms || []).filter(Boolean);
    patrolState.anomalies = anomalies || [];
    renderPatrolOverview();
    renderPatrolMini((mini && mini.items) || []);
    markPatrolSeen();
  } catch { /* 会话失效走全局 401 */ }
}

/* ══════════════════════════════ 通知（统一事件流） ════════════════════════ */
// 巡查事件 + 安全登录日志的归并视图：来源/严重度筛选 + 分页，
// 数据来自 GET /api/events/feed（两源按时间归并排序）。
const FEED_PAGE_SIZE = 20;

// 面板左列的事件摘要：复用小窗的紧凑行样式。
function miniFeedRow(e) {
  const sevCls = e.sev === 'bad' ? 'dot-bad' : e.sev === 'warn' ? 'dot-warn' : 'dot-ok';
  const what = e.source === 'patrol'
    ? (e.detail || patrolCheckMeta(e.type))
    : [banMeta(e.action).label, e.detail].filter(Boolean).join(' · ');
  const who = e.source === 'patrol' ? '巡查 · ' + (e.actor || '') : '安全 · ' + e.actor;
  const t = new Date(e.ts);
  return `<div class="np-row">
    <span class="np-sev ${sevCls}"></span>
    <div class="np-main">
      <span class="np-what">${esc(what)}</span>
      <span class="np-meta">${esc(who)}</span>
    </div>
    <span class="np-time">${ago(t.getTime() / 1000)}</span>
  </div>`;
}

function renderPatrolMini(items) {
  const box = $('patrolMini');
  if (!items.length) { box.innerHTML = '<div class="np-empty">暂无巡查事件</div>'; return; }
  box.innerHTML = items.map(miniFeedRow).join('');
}

function renderSecLogMini(log) {
  const box = $('secLogMini');
  const rows = (log || []).slice(0, 6);
  if (!rows.length) { box.innerHTML = '<div class="np-empty">暂无登录记录</div>'; return; }
  // /api/security/log 返回原始登录日志，这里归一成 feed 形状复用同一套行渲染。
  const sevOf = { auto_ban: 'bad', login_blocked: 'bad', manual_ban: 'bad', login_fail: 'warn', login_ok: 'info', unban: 'info' };
  box.innerHTML = rows.map((e) => miniFeedRow({
    ts: e.ts, source: 'security', type: e.action, action: e.action,
    sev: sevOf[e.action] || 'info', actor: e.ip, user: e.user || '', detail: e.detail || '',
  })).join('');
}
// 筛选状态按来源各自保留：切 Tab 不互删对方的筛选条件。
const notifState = { source: '', sev: '', check: '', secType: '', time: '', q: '', page: 1, total: 0, items: [] };

const FEED_SEV = {
  bad:  { label: '严重', cls: 'badge-bad' },
  warn: { label: '警告', cls: 'badge-warn' },
  info: { label: '信息', cls: 'badge-quiet' },
};

function renderNotifications() {
  const list = notifState.items;
  const tbody = $('nfTbody');
  $('nfEmpty').hidden = list.length > 0;
  const totalPages = Math.max(1, Math.ceil(notifState.total / FEED_PAGE_SIZE));
  const pager = $('nfPager');
  pager.hidden = notifState.total <= 0;
  $('nfTotal').textContent = `共 ${notifState.total} 条`;
  $('nfPage').textContent = `${notifState.page} / ${totalPages}`;
  $('nfPrev').disabled = notifState.page <= 1;
  $('nfNext').disabled = notifState.page >= totalPages;
  if (!list.length) { tbody.innerHTML = ''; return; }
  tbody.innerHTML = list.map((e) => {
    const sev = FEED_SEV[e.sev] || FEED_SEV.info;
    const src = e.source === 'patrol'
      ? '<span class="badge badge-blue">巡查</span>'
      : '<span class="badge badge-quiet">安全</span>';
    const actor = e.source === 'patrol'
      ? (e.actor ? esc(e.actor) : '—')
      : esc(e.actor) + (e.user ? ` <span class="dim small">${esc(e.user)}</span>` : '');
    const what = e.source === 'patrol'
      ? (e.detail ? esc(e.detail) : esc(patrolCheckMeta(e.type)))
      : [banMeta(e.action).label, e.detail ? esc(e.detail) : ''].filter(Boolean).join(' · ') || '—';
    const t = new Date(e.ts);
    return `<tr>
      <td class="dim small" title="${t.toLocaleString()}">${ago(t.getTime() / 1000)}</td>
      <td>${src}</td>
      <td><span class="badge ${sev.cls}">${sev.label}</span></td>
      <td class="mono-cell">${actor}</td>
      <td class="dim small">${what}</td>
    </tr>`;
  }).join('');
}

async function loadNotifications() {
  const f = notifState;
  showSkeleton($('nfTbody'), $('nfEmpty'), skeletonTableRows(5, 5));
  const qs = new URLSearchParams({ page: f.page, page_size: FEED_PAGE_SIZE });
  if (f.source) qs.set('source', f.source);
  if (f.source !== 'security' && f.sev) qs.set('severity', f.sev);
  if (f.source === 'patrol' && f.check) qs.set('check', f.check);
  if (f.source === 'security' && f.secType) qs.set('sec_type', f.secType);
  if (f.time) qs.set('since', String(Math.floor(Date.now() / 1000) - Number(f.time)));
  if (f.q) qs.set('q', f.q);
  try {
    const d = await api('GET', `/api/events/feed?${qs}`);
    notifState.items = (d && d.items) || [];
    notifState.total = (d && d.total) || 0;
    renderNotifications();
  } catch { /* 会话失效走全局 401 */ }
}

// 子 Tab 切来源：登录=security、巡查=patrol；再次点击已选中的 Tab 回到全部。
// 筛选维度跟着来源走（各 Tab 有各自的筛选组），切换只改可见性不互删取值。
function applyNotifFilterVisibility() {
  const show = (el, on) => { const w = el.closest('.pick'); if (w) w.hidden = !on; };
  show($('nfSev'), notifState.source !== 'security');
  show($('nfCheck'), notifState.source === 'patrol');
  show($('nfSecType'), notifState.source === 'security');
}
function syncNotifTabs() {
  $('nfTabLogin').setAttribute('aria-selected', String(notifState.source === 'security'));
  $('nfTabPatrol').setAttribute('aria-selected', String(notifState.source === 'patrol'));
}
function setNotifSource(src) {
  notifState.source = notifState.source === src ? '' : src;
  syncNotifTabs();
  applyNotifFilterVisibility();
  notifState.page = 1;
  loadNotifications();
}
$('nfTabLogin').addEventListener('click', () => setNotifSource('security'));
$('nfTabPatrol').addEventListener('click', () => setNotifSource('patrol'));
$('nfSev').addEventListener('change', (e) => { notifState.sev = e.target.value; notifState.page = 1; loadNotifications(); });
$('nfCheck').addEventListener('change', (e) => { notifState.check = e.target.value; notifState.page = 1; loadNotifications(); });
$('nfSecType').addEventListener('change', (e) => { notifState.secType = e.target.value; notifState.page = 1; loadNotifications(); });
$('nfTime').addEventListener('change', (e) => { notifState.time = e.target.value; notifState.page = 1; loadNotifications(); });
// 面板摘要的「查看全部」：打开抽屉并预选对应来源 Tab
$('nfOpenLogin').addEventListener('click', () => openNotifDrawer('security'));
$('nfOpenPatrol').addEventListener('click', () => openNotifDrawer('patrol'));
// 搜索：输入防抖后全字段匹配（巡查 detail/vm_id，安全 detail/ip/user）
let nfSearchTimer = 0;
$('nfSearch').addEventListener('input', (e) => {
  clearTimeout(nfSearchTimer);
  nfSearchTimer = setTimeout(() => { notifState.q = e.target.value.trim(); notifState.page = 1; loadNotifications(); }, 250);
});
$('nfPrev').addEventListener('click', () => { if (notifState.page > 1) { notifState.page--; loadNotifications(); } });
$('nfNext').addEventListener('click', () => { notifState.page++; loadNotifications(); });
// ── 铃铛 + 小窗 + 悬浮面板 ──────────────────────────────────────────────────
function openNotifPop() {
  $('notifPop').hidden = false;
  loadNotifPop();
  markNotifSeen(); // 打开即视为看过，红点消除
}
function closeNotifPop() { $('notifPop').hidden = true; }

function openNotifDrawer(source) {
  closeNotifPop();
  // 面板摘要的「查看全部」带来源进来，预选对应 Tab
  if (source) notifState.source = source;
  syncNotifTabs();
  applyNotifFilterVisibility();
  $('nfScrim').hidden = false;
  requestAnimationFrame(() => $('nfScrim').classList.add('show'));
  $('nfDrawer').hidden = false;
  $('nfDrawer').setAttribute('aria-hidden', 'false');
  requestAnimationFrame(() => $('nfDrawer').classList.add('show'));
  document.body.style.overflow = 'hidden';
  loadNotifications();
}
function closeNotifDrawer() {
  $('nfScrim').classList.remove('show');
  $('nfDrawer').classList.remove('show');
  $('nfDrawer').setAttribute('aria-hidden', 'true');
  document.body.style.overflow = '';
  setTimeout(() => { $('nfScrim').hidden = true; $('nfDrawer').hidden = true; }, 320);
}

$('bellBtn').addEventListener('click', (e) => {
  e.stopPropagation();
  if ($('notifPop').hidden) openNotifPop(); else closeNotifPop();
});
// 点击铃铛区域之外关闭小窗
document.addEventListener('click', (e) => {
  if (!$('notifPop').hidden && !e.target.closest('.bell-wrap')) closeNotifPop();
});
// 注意不能直接把 openNotifDrawer 挂给 click：事件对象会被当成 source 参数，
// 请求串变成 source=[object MouseEvent] 被后端 400 拒绝，抽屉就空了。
$('nfExpand').addEventListener('click', () => openNotifDrawer());
$('nfClose').addEventListener('click', closeNotifDrawer);
$('nfScrim').addEventListener('click', closeNotifDrawer);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('nfDrawer').hidden) closeNotifDrawer();
});

// 小窗内容：最近 10 条紧凑列表。
async function loadNotifPop() {
  showSkeleton($('nfPopList'), $('nfPopEmpty'), skeletonMiniRows(4));
  try {
    const d = await api('GET', '/api/events/feed?page=1&page_size=10');
    renderNotifPop((d && d.items) || []);
  } catch { /* 会话失效走全局 401 */ }
}
function renderNotifPop(list) {
  const box = $('nfPopList');
  $('nfPopEmpty').hidden = list.length > 0;
  box.innerHTML = list.map((e) => {
    const sevCls = e.sev === 'bad' ? 'dot-bad' : e.sev === 'warn' ? 'dot-warn' : 'dot-ok';
    const what = e.source === 'patrol'
      ? (e.detail || patrolCheckMeta(e.type))
      : [banMeta(e.action).label, e.detail].filter(Boolean).join(' · ');
    const who = e.source === 'patrol' ? '巡查 · ' + (e.actor || '') : '安全 · ' + e.actor;
    const t = new Date(e.ts);
    return `<div class="np-row">
      <span class="np-sev ${sevCls}"></span>
      <div class="np-main">
        <span class="np-what">${esc(what)}</span>
        <span class="np-meta">${esc(who)}</span>
      </div>
      <span class="np-time">${ago(t.getTime() / 1000)}</span>
    </div>`;
  }).join('');
}

const NOTIF_SEEN_KEY = 'run-notif-seen';
function notifLastSeen() { return Number(localStorage.getItem(NOTIF_SEEN_KEY)) || 0; }

// checkNotifDot 全局轮询最新事件时间，有新通知就让铃铛标红。
async function checkNotifDot() {
  try {
    const d = await api('GET', '/api/events/feed?page=1&page_size=1');
    const latest = d && d.items && d.items.length ? Date.parse(d.items[0].ts) : 0;
    if (latest > notifLastSeen()) $('bellBtn').classList.add('has-new');
  } catch { /* 会话失效走全局 401 */ }
}
// markNotifSeen 记住当前最新事件时间，红点消除。
async function markNotifSeen() {
  try {
    const d = await api('GET', '/api/events/feed?page=1&page_size=1');
    const latest = d && d.items && d.items.length ? Date.parse(d.items[0].ts) : 0;
    localStorage.setItem(NOTIF_SEEN_KEY, String(latest));
    $('bellBtn').classList.remove('has-new');
  } catch { /* 静默 */ }
}

// 设置页为整页流：所有分类常显，侧栏是锚点导航。点击=平滑滚动+高亮；
// 手动滚动时 scrollspy 反向更新高亮。settingsCat 仅用于侧栏高亮同步。
let settingsSpyQuietUntil = 0; // 点击导航后的静默期：smooth 滚动途中不让 spy 抢高亮
function setSettingsCat(cat, { scroll = false } = {}) {
  state.settingsCat = cat;
  qsa('#settingsNav .sn').forEach((b) => b.classList.toggle('active', b.dataset.scat === cat));
  if (scroll) {
    settingsSpyQuietUntil = Date.now() + 900;
    document.querySelector(`#view-settings .scat[data-scat="${CSS.escape(cat)}"]`)
      ?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }
  updateSavebar();
}
function spySettingsScroll() {
  if (state.view !== 'settings' || Date.now() < settingsSpyQuietUntil) return;
  // order 重排后 DOM 顺序 ≠ 视觉顺序，必须按当前几何位置排序后再判定
  const secs = qsa('#view-settings .scat')
    .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top);
  if (!secs.length) return;
  const doc = document.documentElement;
  let best;
  if (doc.scrollTop + doc.clientHeight >= doc.scrollHeight - 4) {
    best = secs[secs.length - 1].dataset.scat; // 滚到底：最后一个区块（矮卡片可能永远过不了判定线）
  } else {
    best = secs[0].dataset.scat; // 顶部兜底：没区块过线时高亮第一个
    for (const s of secs) {
      if (s.getBoundingClientRect().top <= 130) best = s.dataset.scat;
    }
  }
  if (best && best !== state.settingsCat) setSettingsCat(best);
}

function loadPatrolConfig(c) {
  if (!c) return;
  const p = c.patrol || {};
  const b = p.bandwidth || {}, n = p.connections || {}, r = p.resources || {}, pr = p.processes || {}, ab = p.abuse || {}, t = c.telegram || {};
  $('pEnabled').checked = p.enabled == null ? true : !!p.enabled;
  $('pInterval').value = p.interval_sec || 60;
  $('pSelfCheck').value = p.self_check_fail_limit ?? 3;
  $('pbEnabled').checked = b.enabled == null ? true : !!b.enabled;
  $('pbThreshold').value = b.threshold_pct ?? 90;
  $('pbDuration').value = b.duration_min ?? 6;
  $('pbThrottle').value = b.throttle_mbps ?? 10;
  $('pbRelease').value = b.release_min ?? 60;
  $('pcEnabled').checked = n.enabled == null ? true : !!n.enabled;
  $('pcIn').value = n.in_limit ?? 200;
  $('pcOut').value = n.out_limit ?? 300;
  $('pcDuration').value = n.duration_min ?? 4;
  $('pcThrottle').value = n.throttle_mbps ?? 10;
  $('pcRelease').value = n.release_min ?? 60;
  $('prEnabled').checked = r.enabled == null ? true : !!r.enabled;
  $('prCpu').value = r.cpu_pct ?? 90;
  $('prCpuMin').value = r.cpu_min ?? 5;
  $('prMem').value = r.mem_pct ?? 90;
  $('prMemMin').value = r.mem_min ?? 5;
  $('prDisk').value = r.disk_pct ?? 90;
  $('prDiskInterval').value = r.disk_alert_interval_min ?? 360;
  $('prAction').value = r.action || 'restart';
  $('prCooldown').value = r.restart_cooldown_min ?? 60;
  $('ppEnabled').checked = pr.enabled == null ? true : !!pr.enabled;
  $('ppKillStop').value = pr.kill_stop_limit ?? 0;
  patrolState.blacklist = (pr.blacklist || []).slice();
  renderPatrolTags();
  $('paEnabled').checked = !!ab.enabled;
  $('paUdpLimit').value = ab.udp_conn_limit ?? 200;
  $('paUdpAction').value = ab.udp_action || 'throttle';
  $('paScanLimit').value = ab.scan_dst_limit ?? 200;
  $('paScanAction').value = ab.scan_action || 'alert';
  $('paSmtpPorts').value = (ab.smtp_ports || []).join(', ');
  $('paSmtpLimit').value = ab.smtp_limit ?? 20;
  $('paSmtpAction').value = ab.smtp_action || 'alert';
  $('paDuration').value = ab.duration_min ?? 5;
  $('paThrottle').value = ab.throttle_mbps ?? 10;
  $('paRelease').value = ab.release_min ?? 60;
  $('tgEnabled').checked = !!t.enabled;
  // 逐事件推送开关（nil 视为 true）。存量配置 login_enabled=false 的：
  // 登录类三事件映射为全关，首次保存后细粒度字段落盘、语义完全接管。
  const lgOn = t.login_enabled == null || !!t.login_enabled;
  const ev = (v) => lgOn && (v == null || !!v);
  $('tgPatrolNotify').checked = t.patrol_notify == null || !!t.patrol_notify;
  $('tgLoginOkNotify').checked = ev(t.login_ok_notify);
  $('tgAutoBanNotify').checked = ev(t.auto_ban_notify);
  $('tgLoginBlockedNotify').checked = ev(t.login_blocked_notify);
  // BotToken 不回传明文（与主 Token 同基线）：输入框恒为空，留空提交即"不修改"。
  // 是否已配置看响应顶层的 telegram_has_token（telegram 段里的 bot_token 恒为空串）。
  $('tgToken').value = '';
  $('tgToken').placeholder = c.telegram_has_token ? '已配置，留空不修改' : '未配置';
  $('tgChat').value = t.chat_id || '';
}

function collectPatrolConfig() {
  const num = (id) => Math.max(0, Number($(id).value) || 0);
  return {
    patrol: {
      enabled: $('pEnabled').checked,
      interval_sec: num('pInterval'),
      bandwidth: {
        enabled: $('pbEnabled').checked,
        threshold_pct: num('pbThreshold'), duration_min: num('pbDuration'),
        throttle_mbps: num('pbThrottle'), release_min: num('pbRelease'),
      },
      connections: {
        enabled: $('pcEnabled').checked,
        in_limit: num('pcIn'), out_limit: num('pcOut'), duration_min: num('pcDuration'),
        throttle_mbps: num('pcThrottle'), release_min: num('pcRelease'),
      },
      resources: {
        enabled: $('prEnabled').checked,
        cpu_pct: num('prCpu'), cpu_min: num('prCpuMin'),
        mem_pct: num('prMem'), mem_min: num('prMemMin'),
        disk_pct: num('prDisk'), disk_alert_interval_min: num('prDiskInterval'), action: $('prAction').value,
        restart_cooldown_min: num('prCooldown'),
      },
      processes: {
        enabled: $('ppEnabled').checked,
        blacklist: patrolState.blacklist.slice(),
        kill_stop_limit: num('ppKillStop'),
      },
      abuse: {
        enabled: $('paEnabled').checked,
        udp_conn_limit: num('paUdpLimit'),
        udp_action: $('paUdpAction').value,
        scan_dst_limit: num('paScanLimit'),
        scan_action: $('paScanAction').value,
        smtp_ports: $('paSmtpPorts').value.split(/[,，、\s]+/).map((s) => parseInt(s, 10)).filter((v) => Number.isInteger(v) && v >= 1 && v <= 65535),
        smtp_limit: num('paSmtpLimit'),
        smtp_action: $('paSmtpAction').value,
        duration_min: num('paDuration'),
        throttle_mbps: num('paThrottle'),
        release_min: num('paRelease'),
      },
      self_check_fail_limit: num('pSelfCheck'),
    },
    telegram: {
      enabled: $('tgEnabled').checked,
      // login_enabled 由登录类三事件派生（任一开 = true）：细粒度字段已是
      // 完整事实源，该字段仅为兼容旧语义保留。
      login_enabled: $('tgLoginOkNotify').checked || $('tgAutoBanNotify').checked || $('tgLoginBlockedNotify').checked,
      patrol_notify: $('tgPatrolNotify').checked,
      login_ok_notify: $('tgLoginOkNotify').checked,
      auto_ban_notify: $('tgAutoBanNotify').checked,
      login_blocked_notify: $('tgLoginBlockedNotify').checked,
      bot_token: $('tgToken').value.trim(),
      chat_id: $('tgChat').value.trim(),
    },
  };
}

// 巡查配置的快照/脏检查已并入 confSnapshot（o.patrolTelegram 段），统一由 syncDirty 维护。

// 黑名单 tag 编辑器：渲染 chip 列表，支持添加/删除。
function renderPatrolTags() {
  const box = $('ppTagList');
  if (!box) return;
  box.innerHTML = patrolState.blacklist.map((k, i) => `
    <span class="tag">${esc(k)}<span class="tag-x" data-tag="${i}" title="移除"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><use href="#i-x"/></svg></span></span>
  `).join('');
  qsa('[data-tag]', box).forEach((x) => x.addEventListener('click', () => {
    patrolState.blacklist.splice(Number(x.dataset.tag), 1);
    renderPatrolTags();
    syncDirty();
  }));
}

function addPatrolTag() {
  const input = $('ppInput');
  const raw = (input.value || '').trim();
  if (!raw) return;
  // 支持批量导入：按逗号/换行/分号/制表符（含中文标点）拆成多个关键词
  const items = raw.split(/[\n,，、;；\t]+/).map((s) => s.trim()).filter(Boolean);
  for (const v of items) {
    if (patrolState.blacklist.some((k) => k.toLowerCase() === v.toLowerCase())) continue;
    patrolState.blacklist.push(v);
  }
  input.value = '';
  renderPatrolTags();
  syncDirty();
}

qsa('#settingsNav .sn').forEach((b) => b.addEventListener('click', () => setSettingsCat(b.dataset.scat, { scroll: true })));
let settingsSpyTimer = 0;
document.addEventListener('scroll', () => {
  if (state.view !== 'settings') return;
  clearTimeout(settingsSpyTimer);
  settingsSpyTimer = setTimeout(spySettingsScroll, 100);
}, { passive: true, capture: true });
$('patrolForm').addEventListener('input', syncDirty);
$('patrolForm').addEventListener('change', syncDirty);
$('ppInput').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); addPatrolTag(); }
});
$('patrolRefresh').addEventListener('click', (e) => withBusy(e.currentTarget, async () => { await refreshPatrol(); toast('已刷新'); }));

/* ══════════════════════════════ 设置 ═══════════════════════════════════════ */
const CONF_FIELDS = ['siteName', 'token', 'platformAddr', 'monitorNIC', 'monitorDisk', 'maxPortForward',
  'maxForwardIPs', 'trafficResetDay', 'entryHost', 'updateRepo', 'updateBase', 'updateApi', 'netavarkBase',
  'publicIpv4', 'publicIpv6', 'webUser', 'webPass', 'containerAllowSubnets'];

// 独立模式下「平台凭据」相关控件保留可见但不可编辑——用户能直接看到重新接入
// 需要填什么，不用先开开关才知道。开关本身就是这一组字段的生效前提。
function applyPlatformEnabledUI() {
  const on = $('platformEnabled').checked;
  $('token').disabled = !on;
  $('platformAddr').disabled = !on;
  $('platformInsecure').disabled = !on;
  // 清除按钮跟随「服务端是否存着 Token」，不跟随开关——独立模式下也要能清遗留凭据。
  $('clearToken').disabled = !state.hasToken;
}

function confSnapshot() {
  const o = {};
  CONF_FIELDS.forEach((id) => { o[id] = $(id).value; });
  o.hostAllowPorts = state.allowPorts.join(',');
  o.platformEnabled = $('platformEnabled').checked;
  o.platformInsecure = $('platformInsecure').checked;
  o.containerIsolate = $('containerIsolate').checked;
  o.trustProxy = $('trustProxy').checked;
  o.allowInsecureDownload = $('allowInsecureDownload').checked;
  o.allowInternalDownload = $('allowInternalDownload').checked;
  o.trafficResetMode = $('trafficResetMode').value;
  // 巡查+通知渠道已并入主配置：单一快照覆盖全部设置字段。
  // collectPatrolConfig 返回的对象键序固定，序列化稳定。
  o.patrolTelegram = collectPatrolConfig();
  return JSON.stringify(o);
}

function syncDirty() {
  const dirty = state.snapshot !== undefined && confSnapshot() !== state.snapshot;
  state.dirty = dirty;
  updateSavebar();
}

// updateSavebar 统一控制全局 SaveBar：巡查配置已并入主配置，单一 dirty 即可判断。
function updateSavebar() {
  $('savebar')?.classList.toggle('show', !!state.dirty);
}

function loadConfigIntoForm(c) {
  $('siteName').value = c.site_name || 'Run';
  // 服务端只回传是否已配置（has_token），不回传凭据本身：输入框恒为空，
  // 留空提交即"不修改"，只有真正输入新值才会覆盖。
  $('token').value = '';
  $('token').placeholder = c.has_token ? '已配置，留空不修改' : '未配置';
  // 「清除」清的是服务端已保存的凭据，与平台开关无关：独立模式下也要能清掉
  // 遗留 Token，否则只能先打开开关才能清。
  state.hasToken = !!c.has_token;
  $('monitorNIC').value = c.monitor_nic || '';
  $('monitorDisk').value = c.monitor_disk || '';
  $('maxPortForward').value = c.max_port_forward || 20;
  $('maxForwardIPs').value = c.max_forward_ips || 0;
  $('trafficResetDay').value = c.traffic_reset_day || 1;
  $('trafficResetMode').value = c.traffic_reset_mode === 'rental' ? 'rental' : 'monthly';
  syncTrafficResetUI();
  $('entryHost').value = c.host || '';
  $('webUser').value = c.web_user || '';
  $('webPass').value = '';
  $('platformAddr').value = c.platform_addr || '';
  $('platformEnabled').checked = c.platform_enabled !== false;
  applyPlatformEnabledUI();
  $('updateRepo').value = c.update_repo || '';
  $('updateBase').value = c.update_base_url || '';
  $('updateApi').value = c.update_api_url || '';
  $('netavarkBase').value = c.netavark_base || '';
  $('allowInsecureDownload').checked = !!c.allow_insecure_download;
  $('allowInternalDownload').checked = !!c.allow_internal_download;
  $('platformInsecure').checked = !!c.platform_insecure;
  $('platformInsecureWarn').hidden = !c.platform_insecure;
  $('containerIsolate').checked = c.container_isolate !== false;
  $('trustProxy').checked = !!c.trust_proxy;
  state.allowPorts = (c.host_allow_ports || []).map(String);
  renderPortTags();
  $('containerAllowSubnets').value = (c.container_allow_subnets || []).join(', ');
  // 巡查+通知字段与主配置同源（GET /api/config 现在带 patrol/telegram 段），
  // 必须在快照拍照之前填充，否则初次加载会误报 dirty。
  loadPatrolConfig(c);
  state.snapshot = confSnapshot();
  syncDirty();
  loadNetworkAddresses(c.public_ipv4 || '', c.public_ipv6 || '');
  // 共享 IPv6：snat 模式显示地址选择；未启用时给出启用入口。
  renderIPv6Share(c);
}

// renderIPv6Share 控制共享 IPv6 的两种形态：
// 已启用（snat）→ 可换地址；未启用 → 提供一键启用入口。
// podman 的 IPv6 是网络级属性，启用必须重建容器网络，故实际操作放在按钮里二次确认。
function renderIPv6Share(c) {
  const edit = $('publicIpv6Field');
  const enable = $('ipv6EnableField');
  if (!edit || !enable) return;
  const isSnat = c.ipv6_mode === 'snat';
  edit.hidden = !isSnat;
  enable.hidden = isSnat || c.ipv6_mode === 'subnet' || c.virt_type !== 'podman';
  if (!enable.hidden) void fillSharedV6Options();
}

async function fillSharedV6Options() {
  const sel = $('sharedV6Addr');
  if (!sel) return;
  try {
    const data = await api('GET', '/api/network/addresses');
    const v6 = (data && data.ipv6) || [];
    sel.innerHTML = '<option value="">选择本机 IPv6 地址</option>';
    v6.forEach((ip) => {
      const o = document.createElement('option');
      o.value = ip;
      o.textContent = ip;
      sel.appendChild(o);
    });
    if (v6.length === 1) sel.value = v6[0];
  } catch (e) {
    // 拉不到地址就留空，用户刷新页面可重试
  }
}

// 流量重置模式切换：rental（创建 + 30 天）时隐藏「重置日」输入。
function syncTrafficResetUI() {
  const el = $('trafficResetDayField');
  if (el) el.style.display = $('trafficResetMode').value === 'rental' ? 'none' : '';
}

// loadNetworkAddresses 拉取宿主机可用的 IPv4 填充下拉框。
// 当前配置值若不在列表里（IP 尚未绑到网卡），补一项保留，避免保存时被清空。
async function loadNetworkAddresses(selected, selected6) {
  const sel = $('publicIpv4');
  if (!sel) return;
  let addrs = [];
  let addrs6 = [];
  let primary = '';
  try {
    const data = await api('GET', '/api/network/addresses');
    addrs = (data && data.ipv4) || [];
    addrs6 = (data && data.ipv6) || [];
    primary = (data && data.primary) || '';
  } catch (e) { /* 拉不到就只留空选项 */ }

  sel.innerHTML = '<option value="">未指定（默认）</option>';
  addrs.forEach((ip) => {
    const opt = document.createElement('option');
    opt.value = ip;
    opt.textContent = ip === primary ? `${ip}（主）` : ip;
    sel.appendChild(opt);
  });
  if (selected && !addrs.includes(selected)) {
    const opt = document.createElement('option');
    opt.value = selected;
    opt.textContent = `${selected}（当前配置）`;
    sel.appendChild(opt);
  }
  sel.value = selected || '';

  const sel6 = $('publicIpv6');
  if (sel6) {
    sel6.innerHTML = '<option value="">未指定（用检测值）</option>';
    addrs6.forEach((ip) => {
      const opt = document.createElement('option');
      opt.value = ip;
      opt.textContent = ip;
      sel6.appendChild(opt);
    });
    if (selected6 && !addrs6.includes(selected6)) {
      const opt = document.createElement('option');
      opt.value = selected6;
      opt.textContent = `${selected6}（当前配置）`;
      sel6.appendChild(opt);
    }
    sel6.value = selected6 || '';
  }

  state.snapshot = confSnapshot();
  syncDirty();
}

// 容器资源上限：限制所有 podman 容器合计的 CPU/内存（machine.slice 预算）。
async function loadPodmanLimits() {
  const card = $('podmanLimitsCard');
  if (!card) return;
  let d;
  try {
    d = await api('GET', '/api/podman/limits');
  } catch (e) {
    card.hidden = true;
    return;
  }
  if (!d || !d.supported) {
    card.hidden = true;
    return;
  }
  card.hidden = false;
  $('plMemory').value = d.memory_max || '';
  $('plMemoryHigh').value = d.memory_high || '';
  $('plSwap').value = d.swap_max || '';
  $('plCPU').value = d.cpu_quota ? String(Number(d.cpu_quota) / 100) : '';
  $('plEff').textContent = `当前生效：内存 ${d.eff_memory || '不限'} · 软 ${d.eff_mem_high || '不限'} · Swap ${d.eff_swap || '不限'} · CPU ${d.eff_cpu || '不限'}`;
}

async function savePodmanLimits(memoryMax, memoryHigh, swapMax, cpuCores) {
  let cpuQuota = '';
  if (cpuCores !== '') {
    const n = Number(cpuCores);
    if (!Number.isFinite(n) || n <= 0) throw new Error('CPU 核数无效');
    cpuQuota = String(Math.round(n * 100));
  }
  const body = { memory_max: memoryMax, memory_high: memoryHigh, swap_max: swapMax, cpu_quota: cpuQuota };
  // 四项全空 = 显式取消全部上限，后端要求带 unlimited 才肯清空 drop-in
  // （防止字段名写错时静默把整个集群的上限放开）。
  if (!memoryMax && !memoryHigh && !swapMax && !cpuQuota) body.unlimited = true;
  await api('POST', '/api/podman/limits', body);
}

$('plApply').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  $('plErr').classList.remove('show');
  try {
    await savePodmanLimits($('plMemory').value.trim(), $('plMemoryHigh').value.trim(), $('plSwap').value.trim(), $('plCPU').value.trim());
    toast('资源上限已生效');
    await loadPodmanLimits();
  } catch (err) {
    $('plErr').textContent = err.message;
    $('plErr').classList.add('show');
  }
}));

$('plClear').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  $('plErr').classList.remove('show');
  try {
    await savePodmanLimits('', '', '', '');
    toast('已取消资源上限');
    await loadPodmanLimits();
  } catch (err) {
    $('plErr').textContent = err.message;
    $('plErr').classList.add('show');
  }
}));

// 宿主机端口白名单 tag 编辑器：与进程黑名单一致的可增删 chip 列表。
// 支持粘贴逗号/空格/换行/中文标点分隔的多个端口批量导入；
// 非 1-65535 的非法项直接忽略（旧版靠后端 ParsePortList 静默丢弃，
// 中文逗号会让整串解析失败，所以入口就拆分干净）。
function renderPortTags() {
  const box = $('portTagList');
  if (!box) return;
  box.innerHTML = state.allowPorts.map((p, i) => `
    <span class="tag">${esc(p)}<span class="tag-x" data-port="${i}" title="移除"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><use href="#i-x"/></svg></span></span>
  `).join('');
  qsa('[data-port]', box).forEach((x) => x.addEventListener('click', () => {
    state.allowPorts.splice(Number(x.dataset.port), 1);
    renderPortTags();
    syncDirty();
  }));
}

function addPortTags() {
  const input = $('portInput');
  const raw = (input.value || '').trim();
  if (!raw) return;
  const items = raw.split(/[\s,，、;；]+/).map((s) => s.trim()).filter(Boolean);
  for (const v of items) {
    const n = Number(v);
    if (!Number.isInteger(n) || n < 1 || n > 65535) continue;
    const s = String(n);
    if (!state.allowPorts.includes(s)) state.allowPorts.push(s);
  }
  state.allowPorts.sort((a, b) => Number(a) - Number(b));
  input.value = '';
  renderPortTags();
  syncDirty();
}

CONF_FIELDS.forEach((id) => $(id)?.addEventListener('input', syncDirty));
$('platformInsecure').addEventListener('change', () => { $('platformInsecureWarn').hidden = !$('platformInsecure').checked; syncDirty(); });

// 启用共享 IPv6：podman 的网络 IPv6 是网络级属性，启用必须重建容器网络。
// 先确认影响（容器数、连接中断），再交给后端一次性完成迁移 + 写配置。
$('btnEnableSharedV6')?.addEventListener('click', () => {
  const sel = $('sharedV6Addr');
  const addr = sel ? sel.value : '';
  if (!addr) { toast('请先选择要使用的 IPv6 地址', 'bad'); return; }
  const ok = window.confirm(
    '将重建容器网络以启用共享 IPv6：\n\n' +
    '· 所有容器的网络会中断数秒（进程不重启）\n' +
    '· 已建立的连接会断开，客户端需要重连\n\n确认继续？'
  );
  if (!ok) return;
  withBusy($('btnEnableSharedV6'), async () => {
    try {
      const res = await api('POST', '/api/ipv6/share', { enabled: true, addr });
      toast(`已启用共享 IPv6：${res.containers} 个容器，中断 ${(res.downtime_ms / 1000).toFixed(1)} 秒`);
      loadConfigIntoForm(await api('GET', '/api/config'));
    } catch (e) {
      toast('启用失败：' + (e.message || e), 'bad');
      // The network/config can be saved while egress application is pending.
      try { loadConfigIntoForm(await api('GET', '/api/config')); } catch (_) { /* retain the error */ }
    }
  });
});
$('containerIsolate').addEventListener('change', syncDirty);
$('trustProxy').addEventListener('change', syncDirty);
$('trafficResetMode').addEventListener('change', () => { syncTrafficResetUI(); syncDirty(); });
$('portInput').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); addPortTags(); }
});
$('portInput').addEventListener('blur', addPortTags);
$('platformEnabled').addEventListener('change', () => { applyPlatformEnabledUI(); syncDirty(); });
// 清除 Token 是立即生效的后端操作（不是待在表单里保存的字段），
// 因此完成后要把快照基线一起刷新，否则会留下一个假的「未保存」提示。
$('clearToken').addEventListener('click', (e) => clearPlatformToken(e.currentTarget).catch(toastErr));

// Telegram 通道连通性测试：同步发送、结果就地显示。不受 enabled 开关限制——
// 配了凭据就能测，用于区分「开关开着但没收到」与「根本发不出去」。
$('tgTest').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  const res = $('tgTestResult');
  try {
    await api('POST', '/api/notify/test');
    res.textContent = '✅ 已发送，请查收 Telegram';
    res.hidden = false;
  } catch (err) {
    res.textContent = '❌ ' + (err?.message || '发送失败');
    res.hidden = false;
  }
}));

async function clearPlatformToken(btn) {
  if (!confirm('清除 Token 并切换为独立模式？')) return;
  await withBusy(btn, async () => {
    await api('DELETE', '/api/platform/token');
    $('token').value = '';
    $('token').placeholder = '未配置';
    $('platformEnabled').checked = false;
    state.hasToken = false;
    applyPlatformEnabledUI();
    if (state.config) state.config.has_token = false;
    state.snapshot = confSnapshot();
    syncDirty();
    toast('已清除 Token，切换为独立模式');
    updateConnection();
  });
  // withBusy 结束时会把按钮无条件恢复为可用，这里按最新状态再矫正一次。
  applyPlatformEnabledUI();
}

async function saveConfig(btn) {
  await withBusy(btn, async () => {
    const body = {
      site_name: $('siteName').value.trim() || 'Run',
      token: $('token').value,
      monitor_nic: $('monitorNIC').value,
      monitor_disk: $('monitorDisk').value,
      max_port_forward: Number($('maxPortForward').value) || 20,
      max_forward_ips: Math.max(0, Number($('maxForwardIPs').value) || 0),
      traffic_reset_day: Number($('trafficResetDay').value) || 1,
      traffic_reset_mode: $('trafficResetMode').value,
      host: $('entryHost').value.trim(),
      web_user: $('webUser').value,
      web_pass: $('webPass').value,
      platform_addr: $('platformAddr').value.trim(),
      platform_enabled: $('platformEnabled').checked,
      update_repo: $('updateRepo').value.trim(),
      update_base_url: $('updateBase').value.trim(),
      update_api_url: $('updateApi').value.trim(),
      netavark_base: $('netavarkBase').value.trim(),
      allow_insecure_download: $('allowInsecureDownload').checked,
      allow_internal_download: $('allowInternalDownload').checked,
      public_ipv4: $('publicIpv4').value,
      public_ipv6: $('publicIpv6').value,
      platform_insecure: $('platformInsecure').checked,
      container_isolate: $('containerIsolate').checked,
      trust_proxy: $('trustProxy').checked,
      host_allow_ports: state.allowPorts.join(','),
      container_allow_subnets: $('containerAllowSubnets').value.trim(),
    };
    // 巡查+通知渠道并入主配置单次提交：服务端对 patrol 段整体替换（normalize+validate 后写入）。
    const pc = collectPatrolConfig();
    body.patrol = pc.patrol;
    body.telegram = pc.telegram;
    try {
      await api('POST', '/api/config', body);
      // 后端把空 Token 视为"未修改"。真提交了新值就落盘并立即从页面抹掉明文，
      // 只留一句占位提示——回填会让凭据重新暴露在 DOM 里，等于没改。
      if (body.token) {
        state.hasToken = true;
        $('token').value = '';
        $('token').placeholder = '已配置，留空不修改';
      }
      // Telegram Bot Token 同为脱敏基线：本次提交了新值就立即从页面抹掉明文
      // （与主 Token 同款处理）——不清空的话明文会一直留在 DOM 里，
      // 且后续每次保存都会把旧明文重发一遍。
      if (body.telegram && body.telegram.bot_token) {
        $('tgToken').value = '';
        $('tgToken').placeholder = '已配置，留空不修改';
      }
      delete body.token;
      body.has_token = state.hasToken;
      state.config = { ...(state.config || {}), ...body };
      $('webPass').value = '';
      state.snapshot = confSnapshot();
      syncDirty();
      applyBrand(body.site_name);
      renderHostKv();
      applyPlatformEnabledUI();
      // 平台开关是热生效的（后端 10 秒内断开/重连），保存后立刻刷新一次胶囊，
      // 不让用户盯着「连接中」等下一轮轮询。
      updateConnection();
      toast('配置已保存');
    } catch (e) { toastErr(e); }
  });
}

// 巡查配置已并入 /api/config 单次提交：保存/放弃都只有一套入口。
$('btnSaveConf').addEventListener('click', (e) => saveConfig(e.currentTarget));
$('savebarSave').addEventListener('click', (e) => saveConfig(e.currentTarget));
$('savebarReset').addEventListener('click', () => {
  if (state.dirty) $('btnReloadConf').click(); // 重拉 /api/config → loadConfigIntoForm 重填全部字段并清 dirty
});
$('btnReloadConf').addEventListener('click', async (e) => withBusy(e.currentTarget, async () => {
  try {
    const c = await api('GET', '/api/config');
    state.config = c;
    loadConfigIntoForm(c);
    toast('已放弃改动');
  } catch (err) { toastErr(err); }
}));

function applyBrand(name, iconVersion) {
  const n = (name || 'Run').slice(0, 24);
  $('brandName').textContent = n;
  $('loginTitle').textContent = n;
  document.title = n;
  if (iconVersion !== undefined) renderBrandIcon(iconVersion);
}

// 自定义图标按修改时间做缓存键：换图后 URL 变化，浏览器不会拿旧图。
function renderBrandIcon(version) {
  state.siteIcon = Number(version) || 0;
  const img = $('brandImg');
  const svg = $('brandSvg');
  if (!img || !svg) return;
  // SVG 元素没有 hidden 这个 IDL 属性（那是 HTMLElement 上的），
  // 直接赋值 svg.hidden = true 只会在 DOM 对象上挂一个无用字段，
  // 元素依然占位、把 img 顶到顶栏之外。必须走 attribute。
  const show = state.siteIcon > 0;
  if (show) {
    img.src = `/api/brand/icon?v=${state.siteIcon}`;
  } else {
    img.removeAttribute('src');
  }
  img.toggleAttribute('hidden', !show);
  svg.toggleAttribute('hidden', show);
  // 登录页的品牌标识同步
  const loginImg = $('loginBrandImg');
  const loginSvg = $('loginBrandMark')?.querySelector('svg');
  if (loginImg && loginSvg) {
    if (show) {
      loginImg.src = img.src;
      // 图标文件被删/损坏时回落内置标记（与顶栏 brandImg 的 error 兜底一致）
      loginImg.onerror = () => {
        loginImg.onerror = null;
        loginImg.toggleAttribute('hidden', true);
        loginSvg.toggleAttribute('hidden', false);
      };
    }
    loginImg.toggleAttribute('hidden', !show);
    loginSvg.toggleAttribute('hidden', show);
  }
  applyFavicon(state.siteIcon);
  renderIconPreview();
}

// 浏览器标签图标跟随站点图标。改 href（带版本）可以绕开 ico 的强缓存。
function applyFavicon(version) {
  const link = $('favicon');
  if (!link) return;
  link.removeAttribute('type');
  link.href = version > 0 ? `/api/brand/icon?v=${version}` : '/favicon.ico';
}

function renderIconPreview() {
  const slot = $('iconPreview');
  if (!slot) return;
  // 克隆内置标记当预览：去掉 id 与 hidden，避免预览框跟着一起被隐藏。
  const fallback = $('brandSvg').outerHTML
    .replace(/\sid="brandSvg"/, '')
    .replace(/\shidden(="")?/, '');
  slot.innerHTML = state.siteIcon > 0
    ? `<img src="/api/brand/icon?v=${state.siteIcon}" alt="">`
    : fallback;
}

$('iconPick').addEventListener('click', () => $('iconFile').click());
$('iconFile').addEventListener('change', async (e) => {
  const file = e.target.files && e.target.files[0];
  e.target.value = '';
  if (!file) return;
  if (file.size > 256 * 1024) return toast('图标需小于 256 KB', 'bad');
  const data = await new Promise((res, rej) => {
    const fr = new FileReader();
    fr.onload = () => res(fr.result);
    fr.onerror = rej;
    fr.readAsDataURL(file);
  });
  try {
    const r = await api('POST', '/api/brand/icon', { data });
    renderBrandIcon(r?.site_icon || Math.floor(Date.now() / 1000));
    toast('图标已更新');
  } catch (err) { toastErr(err); }
});
$('iconReset').addEventListener('click', async () => {
  try {
    await api('DELETE', '/api/brand/icon');
    renderBrandIcon(0);
    toast('已恢复默认图标');
  } catch (err) { toastErr(err); }
});
// 图片加载失败（图标文件被删/损坏）时回落内置 SVG —— 同样必须走 attribute。
$('brandImg').addEventListener('error', () => {
  $('brandImg').hidden = true;
  $('brandSvg').removeAttribute('hidden');
});

/* ── 版本 ─────────────────────────────────────────────────────────────────── */
$('versionPill').addEventListener('click', openVersion);
$('btnVersion').addEventListener('click', openVersion);

function openVersion() {
  $('modalVersion').hidden = false;
  $('verErr').classList.remove('show');
  $('verUpdate').hidden = true;
}

$('verCheck').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  $('verErr').classList.remove('show');
  try {
    const d = await api('GET', '/api/update-check');
    $('verCurrent').textContent = fmtVersion({ ...state.version, current: d.current });
    if (d.error) { $('verErr').textContent = '检查失败：' + d.error; $('verErr').classList.add('show'); return; }
    if (d.has_update) {
      $('verLatest').textContent = d.latest;
      $('verUpdate').hidden = false;
      $('verState').className = 'badge badge-warn';
      $('verState').innerHTML = '<span class="dot"></span>有新版本';
    } else {
      $('verState').className = 'badge badge-ok';
      $('verState').innerHTML = '<span class="dot"></span>已是最新';
      toast('已是最新版本');
    }
  } catch (err) { $('verErr').textContent = err.message; $('verErr').classList.add('show'); }
}));

$('verUpgrade').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  // 先比对版本：版本号相同也会重新下载安装（可用来拉取 CI 重新构建的产物），
  // 但必须说清楚，否则用户会以为"点了没反应"。
  let head = '升级后服务会自动重启，面板会短暂失联。\n\n';
  try {
    const d = await api('GET', '/api/update-check');
    if (d && !d.error) {
      head = d.has_update
        ? `发现新版本 ${d.latest}（当前 ${d.current}）。\n升级后服务会自动重启，面板会短暂失联。\n\n`
        : `当前已是最新版本 ${d.current}。\n继续会按同一版本重新下载并覆盖安装，`
          + '仅用于修复损坏的安装。\n\n';
    }
  } catch (err) { /* 检查失败不阻塞升级 */ }

  if (!confirm(head + '确认继续？')) return;
  try {
    await api('POST', '/api/upgrade');
    $('verUpgradeNote').hidden = false;
    $('verUpgradeNote').textContent = '升级已开始，面板会短暂失联，稍后刷新即可。';
    toast('升级已开始');
  } catch (err) { $('verErr').textContent = err.message; $('verErr').classList.add('show'); }
}));

/* ── 创建虚拟机 ───────────────────────────────────────────────────────────── */
$('btnCreate').addEventListener('click', openCreate);

function openCreate() {
  $('createErr').classList.remove('show');
  populateImageSelect('cImage');
  $('modalCreate').hidden = false;
}

$('formCreate').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('createErr').classList.remove('show');
  const body = {
    vm_id: $('cVmid').value.trim() || undefined,
    cpu: Number($('cCpu').value),
    ram_mb: Number($('cRam').value),
    disk_gb: Number($('cDisk').value),
    bandwidth_mbps: Number($('cBw').value) || 0,
    os_image: $('cImage').value,
    root_password: $('cPwd').value,
  };
  try {
    const res = await withBusy($('createSubmit'), () => api('POST', '/api/vms', body));
    $('modalCreate').hidden = true;
    $('formCreate').reset();
    toast(`虚拟机 ${res?.vm_id || ''} 已创建`);
    await refreshVMs();
  } catch (err) { $('createErr').textContent = err.message; $('createErr').classList.add('show'); }
});

function populateImageSelect(selectId) {
  const sel = $(selectId);
  if (!sel) return;
  sel.innerHTML = state.images.map((img) => `<option value="${esc(img.id)}">${esc(img.name)}</option>`).join('');
}

/* ── 镜像配置（内置 + 自定义统一清单：上架开关 / 排序 / 删除）───────────────── */
$('btnImages').addEventListener('click', openImagesConfig);

async function openImagesConfig() {
  try {
    await loadImageConfig();
    $('modalImages').hidden = false;
    $('ciAddForm').hidden = true;
    $('ciRef').value = '';
  } catch (e) { toastErr(e); }
}

async function loadImageConfig() {
  const res = await api('GET', '/api/images/config');
  state.imageItems = res.items || [];
  renderImageItems();
}

// 从镜像引用推导显示名：docker.io/library/nginx:1.25 -> nginx:1.25
function deriveImageName(ref) {
  const noReg = ref.replace(/^[^/]+\.[^/]+\//, '');
  const parts = noReg.split('/');
  return parts[parts.length - 1] || ref;
}

function renderImageItems() {
  const box = $('imagesList');
  if (!state.imageItems.length) {
    box.innerHTML = '<div class="empty">还没有可用镜像</div>';
    return;
  }
  box.innerHTML = state.imageItems.map((it, idx) => {
    const custom = it.kind === 'custom';
    const notReady = custom && it.status !== 'ready';
    const kindBadge = custom
      ? '<span class="badge badge-blue">自定义</span>'
      : '<span class="badge badge-quiet">内置</span>';
    const statusBadge = !custom || it.status === 'ready' ? ''
      : it.status === 'pulling'
        ? '<span class="badge badge-warn">PULLING</span>'
        : '<span class="badge badge-bad">ERROR</span>';
    return `
    <div class="list-row">
      <label class="check" style="flex:1;min-width:0">
        <input type="checkbox" data-img-chk="${idx}" ${it.enabled ? 'checked' : ''} ${notReady ? 'disabled' : ''}>
        <svg class="check-box" viewBox="0 0 12 12" aria-hidden="true"><rect class="cb-bg" x="1" y="1" width="10" height="10" rx="2.5"/><path class="cb-mark" d="M2.8 6.4 L5 8.5 L9.4 3.7"/></svg>
        <span class="truncate">${esc(it.name)}</span>${kindBadge}${statusBadge}
      </label>
      <button class="act" data-img-up="${idx}" ${idx === 0 ? 'disabled' : ''} title="上移">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m6 14 6-6 6 6"/></svg>
      </button>
      <button class="act" data-img-down="${idx}" ${idx === state.imageItems.length - 1 ? 'disabled' : ''} title="下移">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m6 10 6 6 6-6"/></svg>
      </button>
      ${custom ? `
      ${it.status === 'error' ? `<button class="btn btn-quiet btn-sm" data-img-retry="${idx}">重试</button>` : ''}
      <button class="act danger" data-img-del="${idx}" title="删除">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
      </button>` : ''}
    </div>`;
  }).join('');

  qsa('[data-img-chk]').forEach((chk) => chk.addEventListener('change', () => {
    const it = state.imageItems[Number(chk.dataset.imgChk)];
    if (it) it.enabled = chk.checked;
  }));
  qsa('[data-img-up]').forEach((btn) => btn.addEventListener('click', () => moveImageItem(Number(btn.dataset.imgUp), -1)));
  qsa('[data-img-down]').forEach((btn) => btn.addEventListener('click', () => moveImageItem(Number(btn.dataset.imgDown), 1)));
  qsa('[data-img-del]').forEach((btn) => btn.addEventListener('click', async () => {
    const it = state.imageItems[Number(btn.dataset.imgDel)];
    if (!it || !confirm(`删除自定义镜像「${it.name}」？\n\n删除后记录与镜像引用一并移除，需要时重新添加拉取。`)) return;
    try {
      await api('DELETE', '/api/images/custom?id=' + encodeURIComponent(it.id));
      toast('已删除');
      await loadImageConfig();
      state.images = ((await api('GET', '/api/images')) || []).filter(Boolean);
    } catch (e) { toastErr(e); }
  }));
  qsa('[data-img-retry]').forEach((btn) => btn.addEventListener('click', async () => {
    const it = state.imageItems[Number(btn.dataset.imgRetry)];
    if (!it) return;
    try {
      await api('POST', '/api/images/custom', { id: it.id, name: it.name });
      toast('已重新加入拉取队列');
      await loadImageConfig();
    } catch (e) { toastErr(e); }
  }));

  // 拉取中的镜像每 3 秒刷新状态（弹窗打开时才轮询）
  if (state.imageItems.some((i) => i.status === 'pulling') && !$('modalImages').hidden) {
    setTimeout(() => { if (!$('modalImages').hidden) loadImageConfig().catch(() => {}); }, 3000);
  }
}

function moveImageItem(idx, dir) {
  const t = idx + dir;
  if (t < 0 || t >= state.imageItems.length) return;
  [state.imageItems[idx], state.imageItems[t]] = [state.imageItems[t], state.imageItems[idx]];
  renderImageItems();
}

$('imagesSave').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  try {
    await api('POST', '/api/images/config', {
      items: state.imageItems.map(({ id, enabled }) => ({ id, enabled })),
    });
    state.images = ((await api('GET', '/api/images')) || []).filter(Boolean);
    $('modalImages').hidden = true;
    toast('镜像配置已保存');
  } catch (err) { toastErr(err); }
}));

/* 添加自定义镜像：内联展开表单，显示名从镜像地址自动推导 */
$('ciAddToggle').addEventListener('click', () => {
  $('ciAddForm').hidden = false;
  $('ciRef').focus();
});
$('ciCancel').addEventListener('click', () => {
  $('ciAddForm').hidden = true;
  $('ciRef').value = '';
  $('ciName').value = '';
});
$('ciRef').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addCustomImage(); } });
$('ciName').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addCustomImage(); } });
$('ciAdd').addEventListener('click', addCustomImage);

async function addCustomImage() {
  const id = $('ciRef').value.trim();
  if (!id) return toast('请填镜像地址', 'bad');
  const name = $('ciName').value.trim() || deriveImageName(id);
  try {
    await api('POST', '/api/images/custom', { id, name });
    $('ciRef').value = '';
    $('ciName').value = '';
    $('ciAddForm').hidden = true;
    toast('已加入拉取队列');
    await loadImageConfig();
  } catch (e) { toastErr(e); }
}

/* ── 浮层通用关闭 ─────────────────────────────────────────────────────────── */
qsa('[data-close]').forEach((el) => el.addEventListener('click', () => { $(el.dataset.close).hidden = true; }));

/* ── 控制台 ───────────────────────────────────────────────────────────────── */
/* ══ 终端（多标签）═════════════════════════════════════════════════════════
   每个 VM 一个会话：独立 xterm 实例 + 独立 WebSocket（后端每个连接就是一个
   独立 PTY，天然支持并发）。切换标签不拆会话；标签上的 × 关单个会话；
   红灯 / 停靠条的关闭钮 = 关窗口并结束全部会话；最小化走原版单胶囊停靠条
   （圆点汇总连接态，标题=最近活跃 vmId，多会话时 +N）。断线的标签保留置灰，
   缓冲区里仍能看到 [已断开] 输出。 */
const termSessions = new Map(); // vmId -> { term, ws, fit, observer, pane }
let termActive = null; // 当前显示的会话
let termMaximized = false;
let termMinimized = false;

const termConnected = (s) => !!s && !!s.ws && s.ws.readyState <= WebSocket.OPEN;

// 拆单个会话：先把 socket 的回调摘掉再 close，避免 close 触发的 onclose
// 落到别的会话上（表现就是开控制台时闪一行「已断开」）。
function teardownSession(vmId) {
  const s = termSessions.get(vmId);
  if (!s) return;
  if (s.ws) {
    const old = s.ws;
    s.ws = null;
    old.onopen = old.onmessage = old.onclose = old.onerror = null;
    try { old.close(); } catch {}
  }
  if (s.observer) s.observer.disconnect();
  try { s.term.dispose(); } catch {}
  s.pane.remove();
  termSessions.delete(vmId);
}

function closeConsoleWindow() {
  for (const vmId of [...termSessions.keys()]) teardownSession(vmId);
  termActive = null;
  termMinimized = false;
  termMaximized = false;
  $('consoleWindow').classList.remove('maximized');
  $('cwMax').classList.remove('maximized');
  $('cwMax').title = '最大化';
  $('consoleDock').hidden = true;
  $('console').hidden = true;
  document.body.style.overflow = '';
}

function renderTermTabs() {
  const bar = $('termTabs');
  bar.innerHTML = '';
  for (const [vmId, s] of termSessions) {
    const b = document.createElement('button');
    b.type = 'button';
    b.dataset.vm = vmId;
    b.className = 'ttab' + (vmId === termActive ? ' active' : '') + (termConnected(s) ? '' : ' dead');
    b.title = termConnected(s) ? vmId : `${vmId}（已断开）`;
    b.innerHTML = `<span class="tname">${esc(vmId)}</span><span class="ttab-x" data-close-vm="${esc(vmId)}" title="关闭此终端"><svg viewBox="0 0 10 10" aria-hidden="true"><path d="M2.4 2.4l5.2 5.2M7.6 2.4l-5.2 5.2"/></svg></span>`;
    bar.appendChild(b);
  }
}

// 停靠条（原版单胶囊）：圆点汇总连接状态，标题显示最近活跃的 vmId；
// 多会话时追加「+N」表示还有 N 个在后台保持连接，悬停可见完整清单。
function renderDock() {
  const dock = $('consoleDock');
  if (!termMinimized || !termSessions.size) { dock.hidden = true; return; }
  const activeId = termSessions.has(termActive) ? termActive : termSessions.keys().next().value;
  $('dockDot').className = 'dot ' + ([...termSessions.values()].every(termConnected) ? 'dot-ok' : 'dot-bad');
  const title = $('dockTitle');
  title.textContent = activeId;
  title.title = [...termSessions.entries()].map(([id, s]) => (termConnected(s) ? id : `${id}（已断开）`)).join('、');
  // 会话计数独立于截断的标题，长 id 下也能看到还有几个在后台保持连接
  const count = $('dockCount');
  count.textContent = termSessions.size > 1 ? `+${termSessions.size - 1}` : '';
  count.hidden = termSessions.size <= 1;
  dock.hidden = false;
}

function activateSession(vmId) {
  const s = termSessions.get(vmId);
  if (!s) return;
  for (const [id, o] of termSessions) o.pane.hidden = id !== vmId;
  termActive = vmId;
  renderTermTabs();
  // 隐藏过的 pane 尺寸为 0，显示后需要重新 fit 再聚焦
  setTimeout(() => { try { s.fit.fit(); s.term.focus(); } catch {} }, 60);
}

function openConsole(vmId) {
  if (!termSessions.has(vmId)) createSession(vmId);
  termMinimized = false;
  $('consoleDock').hidden = true;
  $('console').hidden = false;
  document.body.style.overflow = 'hidden';
  activateSession(vmId);
}

function createSession(vmId) {
  const pane = document.createElement('div');
  pane.className = 'term-pane';
  pane.hidden = true;
  $('termHost').appendChild(pane);

  const theme = { background: '#0b0c0e', foreground: '#e6e7ea', cursor: '#e6e7ea', selectionBackground: 'rgba(255,255,255,.18)' };
  const t = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: getComputedStyle(document.body).getPropertyValue('--mono') || 'monospace',
    theme,
    convertEol: true,
    scrollback: 4000,
  });
  const fit = new FitAddon.FitAddon();
  t.loadAddon(fit);
  t.open(pane);
  const observer = new ResizeObserver(() => { if (!pane.hidden) try { fit.fit(); } catch {} });
  observer.observe(pane);

  const s = { term: t, ws: null, fit, observer, pane };
  termSessions.set(vmId, s);
  connectWS(vmId, s);
  return s;
}

function connectWS(vmId, s) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const sock = new WebSocket(`${proto}://${location.host}/api/vms/${encodeURIComponent(vmId)}/tty`);
  sock.binaryType = 'arraybuffer';
  s.ws = sock;

  sock.onopen = () => {
    if (s.ws !== sock) return;
    sock.send(JSON.stringify({ type: 'resize', cols: s.term.cols, rows: s.term.rows }));
  };
  sock.onmessage = (e) => {
    if (s.ws !== sock) return;
    s.term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data);
  };
  sock.onclose = () => {
    if (s.ws !== sock) return; // 会话已被主动关闭：不再提示
    s.ws = null;
    s.term.write('\r\n\x1b[38;5;245m[已断开]\x1b[0m\r\n');
    renderTermTabs();
    renderDock();
  };
  sock.onerror = () => {
    if (s.ws !== sock) return;
    s.term.write('\r\n\x1b[31m[连接错误]\x1b[0m\r\n');
  };

  s.term.onData((data) => {
    if (s.ws?.readyState === WebSocket.OPEN) s.ws.send(new TextEncoder().encode(data));
  });
  s.term.onResize(({ cols, rows }) => {
    if (s.ws?.readyState === WebSocket.OPEN) s.ws.send(JSON.stringify({ type: 'resize', cols, rows }));
  });
}

// 关单个会话（标签 × / 停靠 chip ×）；关的是当前显示的则切到相邻会话
function closeSession(vmId) {
  const wasActive = termActive === vmId;
  teardownSession(vmId);
  if (!termSessions.size) { closeConsoleWindow(); return; }
  if (wasActive) activateSession(termSessions.keys().next().value);
  renderTermTabs();
  renderDock();
}

// 黄点：最小化到右下角停靠条，所有会话保持连接（不 dispose 终端）
function minimizeConsole() {
  if ($('console').hidden || !termSessions.size) return;
  termMinimized = true;
  $('console').hidden = true;
  renderDock();
  document.body.style.overflow = '';
}

function restoreConsoleTo(vmId) {
  termMinimized = false;
  $('consoleDock').hidden = true;
  $('console').hidden = false;
  document.body.style.overflow = 'hidden';
  activateSession(vmId);
}

function toggleConsoleMax() {
  termMaximized = !termMaximized;
  const win = $('consoleWindow');
  win.classList.toggle('maximized', termMaximized);
  $('cwMax').classList.toggle('maximized', termMaximized);
  $('cwMax').title = termMaximized ? '还原' : '最大化';
  qs('.console', document).style.padding = termMaximized ? '0' : '';
  const s = termSessions.get(termActive);
  setTimeout(() => { try { s?.fit.fit(); } catch {} }, 60);
}

$('cwClose').addEventListener('click', closeConsoleWindow);
$('cwMin').addEventListener('click', minimizeConsole);
$('cwMax').addEventListener('click', toggleConsoleMax);
$('termTabs').addEventListener('click', (e) => {
  const x = e.target.closest('[data-close-vm]');
  if (x) { closeSession(x.dataset.closeVm); return; }
  const tab = e.target.closest('.ttab');
  if (tab) activateSession(tab.dataset.vm);
});
$('cwRestore').addEventListener('click', () => {
  restoreConsoleTo(termSessions.has(termActive) ? termActive : termSessions.keys().next().value);
});
$('cwDockClose').addEventListener('click', closeConsoleWindow);
// 点窗口外遮罩 = 最小化（会话保持），不是关闭
qsa('[data-min-console]').forEach((el) => el.addEventListener('click', minimizeConsole));

/* ══════════════════════════════ 启动 ═══════════════════════════════════════ */
async function boot() {
  applyTheme(localStorage.getItem('run-theme') || 'light');
  enhanceSelects();

  // 品牌是公开端点，登录页也要显示真实名称/图标；与会话探测并行，
  // 首帧（HTML 静态态）始终是登录页，由这里的结果决定去留。
  const [brand, authed] = await Promise.all([
    api('GET', '/api/brand').catch(() => null),
    api('GET', '/api/session').then(() => true).catch(() => false),
  ]);
  if (brand) applyBrand(brand.site_name, brand.site_icon);

  if (authed) {
    init();
  } else {
    showLogin();
  }
}

function showLogin() {
  // 幂等：会话失效后轮询（2s/5s）会反复触发 401→showLogin，
  // 只有首次进入登录页才抢焦点，避免用户输密码时被反复拽回用户名框。
  const first = !document.body.classList.contains('logged-out');
  document.body.classList.add('logged-out');
  setView('login', { push: false });
  if (first) $('loginUser')?.focus();
}

/* ── 登录 / 登出 ───────────────────────────────────────────────────────────── */
$('formLogin').addEventListener('submit', async (e) => {
  e.preventDefault();
  const err = $('loginErr');
  err.hidden = true;
  try {
    await withBusy($('loginSubmit'), () => api('POST', '/api/login', {
      user: $('loginUser').value.trim(),
      password: $('loginPass').value,
    }));
    location.reload(); // 走完整初始化路径，避免重复轮询
  } catch (ex) {
    err.textContent = ex.message;
    err.hidden = false;
  }
});

$('logoutBtn').addEventListener('click', async () => {
  try { await api('POST', '/api/logout'); } catch { /* 已失效也照常回登录页 */ }
  location.reload();
});

async function init() {
  document.body.classList.remove('logged-out'); // HTML 初始为未登录挡板态
  applyTheme(localStorage.getItem('run-theme') || 'light');
  enhanceSelects();
  // 已登录用户不应停留在登录视图（如手动输入/收藏了 #login）
  const initial = (location.hash || '#overview').slice(1);
  setSettingsCat('panel');
  setView(initial === 'login' ? 'overview' : initial, { push: false });

  try {
    const [sys, conf, imgs, version] = await Promise.all([
      api('GET', '/api/system/info').catch(() => ({ nics: [], disks: [] })),
      api('GET', '/api/config').catch(() => ({})),
      api('GET', '/api/images').catch(() => []),
      api('GET', '/api/version').catch(() => ({ current: '—' })),
    ]);

    state.config = conf || {};
    state.images = (imgs || []).filter(Boolean);
    applyBrand(state.config.site_name, state.config.site_icon);
    $('virtPill').textContent = `VIRT ${(state.config.virt_type || '?').toUpperCase()}`;
    state.version = version || null;
    $('versionPill').textContent = fmtVersion(version, false);
    $('aboutVersion').textContent = fmtVersion(version);
    $('aboutVirt').textContent = (state.config.virt_type || '').toUpperCase() || '—';
    $('aboutRepo').textContent = state.config.update_repo || 'tl0502/runman-agent';

    $('monitorNIC').innerHTML = '<option value="">全部网卡</option>' + (sys.nics || []).map((n) => `<option value="${esc(n)}">${esc(n)}${n === sys.primary_nic ? ' · 主网卡' : ''}</option>`).join('');
    $('monitorDisk').innerHTML = (sys.disks || []).map((d) => `<option value="${esc(d)}">${esc(d)}${d === (sys.primary_disk || '/') ? ' · 系统盘' : ''}</option>`).join('');

    loadConfigIntoForm(state.config);
    loadPodmanLimits();
    renderHostKv();
    updateConnection();
    setInterval(updateConnection, 4000);
    updateHostFWStatus();
    setInterval(updateHostFWStatus, 10000);
  } catch { /* 静默 */ }

  await Promise.all([refreshStatus(), refreshVMs()]);
  moveSegThumb();
  setInterval(refreshStatus, 2000);
  setInterval(refreshVMs, 5000);
  setInterval(moveSegThumb, 1000);

  if (state.view === 'firewall') refreshFirewall();
  setInterval(() => { if (state.view === 'firewall') checkRfw(); }, 10000);
  setInterval(() => { if (state.view === 'security') refreshSecurity(); }, 10000);
  checkPatrolDot();
  setInterval(checkPatrolDot, 10000);
  checkNotifDot();
  setInterval(checkNotifDot, 10000);
}

async function updateConnection() {
  const pill = $('connPill');
  try {
    const d = await api('GET', '/api/connection');
    switch (d.mode) {
      case 'standalone':
        pill.className = 'pill pill-quiet';
        pill.innerHTML = '<span class="dot"></span><span id="connText">独立模式</span>';
        pill.title = '未接入平台，仅本地面板管理';
        break;
      case 'connected':
        pill.className = 'pill pill-ok';
        pill.innerHTML = '<span class="dot"></span><span id="connText">已连接</span>';
        pill.title = '平台连接正常';
        break;
      case 'no_token':
        pill.className = 'pill pill-warn';
        pill.innerHTML = '<span class="dot"></span><span id="connText">未配置</span>';
        pill.title = '已启用平台接入，但未设置 Token';
        break;
      default:
        pill.className = 'pill pill-warn';
        pill.innerHTML = '<span class="dot"></span><span id="connText">连接中</span>';
        pill.title = d.error || '正在连接平台';
    }
  } catch {
    pill.className = 'pill pill-quiet';
    pill.innerHTML = '<span class="dot"></span><span id="connText">未知</span>';
  }
}

// updateHostFWStatus 轮询容器网络隔离规则的生效状态。
// 隔离是安全边界，失效必须可见——这里只在设置页「容器网络」卡片给出提示。
async function updateHostFWStatus() {
  const el = $('hostfwStatus');
  if (!el) return;
  try {
    const d = await api('GET', '/api/hostfw/status');
    if (d.healthy) {
      el.hidden = true;
    } else {
      el.hidden = false;
      el.textContent = d.error || '容器网络隔离规则未生效';
    }
  } catch {
    el.hidden = true;
  }
}

$('connPill').addEventListener('click', () => setView('settings'));
$('ovCopyHost').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText($('ovHostAddr').textContent); toast('已复制'); }
  catch { toast('复制失败', 'bad'); }
});

/* ── 自绘下拉 ───────────────────────────────────────────────────────────────
   原生 <select> 的展开列表由浏览器绘制，圆角、间距、hover 都无法跟随本套设计，
   深色主题下尤其突兀。这里保留原生元素（取值、表单校验、change 事件照旧），
   仅在它上面盖一层按钮 + 浮层。 */

let openPick = null;

// enhanceSelect 把单个原生 select 换成自绘控件，重复调用无副作用。
function enhanceSelect(el) {
  if (!el || el.dataset.pickReady === '1') return;

  const wrap = document.createElement('div');
  wrap.className = 'pick';

  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'input pick-btn';
  btn.setAttribute('aria-haspopup', 'listbox');
  btn.setAttribute('aria-expanded', 'false');
  btn.innerHTML = '<span></span><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" '
    + 'stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>';

  const menu = document.createElement('div');
  menu.className = 'pick-menu';
  menu.setAttribute('role', 'listbox');
  menu.hidden = true;

  // select 夹在 button 与 menu 之间，保持在原父节点的同一位置，避免布局跳变。
  el.parentNode.insertBefore(wrap, el);
  wrap.appendChild(el);
  wrap.appendChild(btn);
  wrap.appendChild(menu);
  el.dataset.pickReady = '1';

  const label = btn.querySelector('span');
  let hoverIdx = -1;

  const syncLabel = () => {
    const opt = el.options[el.selectedIndex];
    label.textContent = opt ? opt.textContent : '';
    btn.disabled = el.disabled;
    wrap.classList.toggle('is-disabled', !!el.disabled);
    btn.title = opt ? opt.textContent : '';
  };

  const renderMenu = () => {
    hoverIdx = el.selectedIndex;
    menu.innerHTML = Array.from(el.options).map((o, i) => {
      const on = i === el.selectedIndex;
      return `<button type="button" class="pick-item${on ? ' is-on' : ''}${o.disabled ? ' is-off' : ''}"`
        + ` role="option" aria-selected="${on}" data-idx="${i}">`
        + `<span>${esc(o.textContent)}</span>`
        + (on ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" '
          + 'stroke-linecap="round" stroke-linejoin="round"><path d="M20 6L9 17l-5-5"/></svg>' : '')
        + '</button>';
    }).join('');
    const cur = menu.querySelector('.is-on');
    if (cur) cur.classList.add('is-hover');
  };

  const place = () => {
    const r = btn.getBoundingClientRect();
    menu.style.width = `${r.width}px`;
    menu.style.left = `${r.left}px`;
    menu.style.top = '0px';
    const mh = menu.offsetHeight;
    const below = window.innerHeight - r.bottom;
    // 下方放不下且上方更宽敞时向上展开
    let top = (below < mh + 10 && r.top > below)
      ? r.top - mh - 6
      : r.bottom + 6;
    // 两个方向都放不下时夹在视口内，避免浮层跑到屏幕外
    top = Math.min(Math.max(8, top), Math.max(8, window.innerHeight - mh - 8));
    const left = Math.min(Math.max(8, r.left), Math.max(8, window.innerWidth - r.width - 8));
    menu.style.top = `${top}px`;
    menu.style.left = `${left}px`;
  };

  // 把菜单项滚进浮层自身的可视区。只动 menu.scrollTop，绝不能用
  // scrollIntoView——它会连带滚动窗口，而窗口滚动会立即触发浮层收起。
  const scrollItemIntoView = (item) => {
    if (!item) return;
    const top = item.offsetTop, bottom = top + item.offsetHeight;
    if (top < menu.scrollTop + 1) menu.scrollTop = top - 1;
    else if (bottom > menu.scrollTop + menu.clientHeight - 1) menu.scrollTop = bottom - menu.clientHeight + 1;
  };

  const open = () => {
    if (el.disabled) return;
    // 同一时刻只允许一个浮层展开，先收起上一个。
    if (openPick) openPick();
    renderMenu();
    // 浮层挂到 body 再定位：卡片上的 backdrop-filter 会把 fixed 的
    // 包含块从视口改成卡片自身，视口坐标会被错误地按卡片偏移解释。
    if (menu.parentNode !== document.body) document.body.appendChild(menu);
    menu.hidden = false;
    wrap.classList.add('is-open');
    btn.setAttribute('aria-expanded', 'true');
    place();
    openPick = close;
    scrollItemIntoView(menu.querySelector('.is-on'));
  };

  const close = () => {
    menu.hidden = true;
    // 移回原位，select 所在容器被销毁时浮层也一并回收，不留孤儿节点。
    if (menu.parentNode === document.body && wrap.isConnected) wrap.appendChild(menu);
    wrap.classList.remove('is-open');
    btn.setAttribute('aria-expanded', 'false');
    if (openPick === close) openPick = null;
  };

  const choose = (idx) => {
    const opt = el.options[idx];
    if (!opt || opt.disabled) return;
    el.selectedIndex = idx;
    // 原生 change 事件：调用方靠它做后续动作，必须照常派发。
    el.dispatchEvent(new Event('change', { bubbles: true }));
    syncLabel();
    close();
    btn.focus();
  };

  btn.addEventListener('click', () => (menu.hidden ? open() : close()));

  btn.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp' || e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      open();
    }
  });

  menu.addEventListener('click', (e) => {
    const item = e.target.closest('.pick-item');
    if (item) choose(Number(item.dataset.idx));
  });

  menu.addEventListener('mousemove', (e) => {
    const item = e.target.closest('.pick-item');
    if (!item) return;
    hoverIdx = Number(item.dataset.idx);
    menu.querySelectorAll('.pick-item').forEach((n, i) => n.classList.toggle('is-hover', i === hoverIdx));
  });

  // 打开状态下接管键盘：方向键移动、回车确认、Esc 关闭
  document.addEventListener('keydown', (e) => {
    if (menu.hidden) return;
    if (e.key === 'Escape') { e.preventDefault(); close(); btn.focus(); return; }
    if (e.key === 'Tab') { close(); return; }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter') return;
    e.preventDefault();
    if (e.key === 'Enter') { choose(hoverIdx); return; }
    const step = e.key === 'ArrowDown' ? 1 : -1;
    let i = hoverIdx;
    for (let n = 0; n < el.options.length; n++) {
      i = (i + step + el.options.length) % el.options.length;
      if (!el.options[i].disabled) break;
    }
    hoverIdx = i;
    menu.querySelectorAll('.pick-item').forEach((n, idx) => n.classList.toggle('is-hover', idx === i));
    scrollItemIntoView(menu.querySelectorAll('.pick-item')[i]);
  });

  // 窗口滚动/resize 时浮层位置会失效，直接收起，比跟随重排更省事也更稳。
  // 注意捕获阶段会听到浮层内部的滚动，那是我们自己设 scrollTop，要放行。
  window.addEventListener('scroll', (e) => {
    if (menu.hidden) return;
    if (e.target === menu) return;
    close();
  }, true);
  window.addEventListener('resize', () => { if (!menu.hidden) close(); });

  // 程序化赋值（el.value = ...）不会触发 change，包一层 setter 保证显示同步。
  try {
    const desc = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value');
    if (desc && desc.get && desc.set) {
      Object.defineProperty(el, 'value', {
        configurable: true,
        get() { return desc.get.call(el); },
        set(v) { desc.set.call(el, v); syncLabel(); },
      });
    }
  } catch { /* 拿不到描述符就退回 change + Observer */ }

  // option 增删、disabled 变化都重新渲染
  new MutationObserver(() => { syncLabel(); if (!menu.hidden) { renderMenu(); place(); } })
    .observe(el, { childList: true, subtree: true, attributes: true, attributeFilter: ['disabled', 'required'] });

  el.addEventListener('change', syncLabel);
  syncLabel();
}

function enhanceSelects(root = document) {
  root.querySelectorAll('select.input').forEach(enhanceSelect);
}

// 点击浮层与按钮之外的地方收起。
// 浮层展开时挂在 body 上（躲开卡片的 backdrop-filter 包含块），
// 所以判定“内部”时要把 .pick-menu 一起算进去。
document.addEventListener('mousedown', (e) => {
  if (openPick && !e.target.closest('.pick, .pick-menu')) openPick();
});

boot();
