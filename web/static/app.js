/* ============================================================================
   Run · 面板前端
   结构：状态 → 工具 → 视图路由 → 各视图渲染 → 抽屉 → 浮层 → 轮询
   接口约定与原面板保持一致，后端无需为 UI 改造让路。
   ========================================================================== */

const $ = (id) => document.getElementById(id);
const qs = (sel, root = document) => root.querySelector(sel);
const qsa = (sel, root = document) => [...root.querySelectorAll(sel)];

/* ── 工具 ─────────────────────────────────────────────────────────────────── */
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
  images: [],
  sort: { field: '', order: 'desc' },
  filter: 'all',
  search: '',
  activeVm: null,
  drawerTab: 'monitor',
  pgTimers: [],
  pfCache: [],
  wgCache: [],
  wgEditId: null,
  imagesAll: [],
  imagesActive: [],
  customImages: [],
  dirty: false,
  siteIcon: 0,
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
const VIEWS = ['overview', 'vms', 'firewall', 'settings'];

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
  if (name === 'vms') renderVMs();
  if (name === 'firewall') refreshFirewall();
  if (name === 'settings') syncDirty();
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
    $('mNetBw').textContent = d.bandwidth_mbps ? `${d.bandwidth_mbps} Mbps` : '不限';
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
  $('ovHostAddr').textContent = c.host || '自动探测';
  $('ovHost').textContent = c.host || location.host;
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
  if (!$('console').hidden) return closeConsole();
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
      const ips = (r.top_ips || []).slice(0, 3).map((ip) => `<span class="badge badge-quiet badge-mono">${esc(ip.ip)} · ${ip.count}</span>`).join('');
      return `
      <div class="list-row">
        <span class="badge badge-quiet" style="text-transform:uppercase">${esc(r.protocol)}</span>
        <span class="mono small">:${r.host_port}</span>
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="dim"><use href="#i-arrow-right"/></svg>
        <span class="mono small muted truncate">${esc(r.target_addr || ':' + r.guest_port)}</span>
        <span class="spacer"></span>
        ${ips}
        <span class="badge ${r.active_conns ? 'badge-ok' : 'badge-quiet'}">${r.active_conns || 0} 活跃</span>
        <button class="act danger" data-del-pf="${esc(r.protocol)}:${r.host_port}" title="删除">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
        </button>
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

$('pfAdd').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  $('pfErr').classList.remove('show');
  const body = { protocol: $('pfProto').value, host_port: Number($('pfHost').value), guest_port: Number($('pfGuest').value) };
  if (!body.host_port || !body.guest_port) { $('pfErr').textContent = '端口不能为空'; $('pfErr').classList.add('show'); return; }
  try {
    await api('POST', `/api/vms/${state.activeVm}/portfwds`, body);
    $('pfHost').value = ''; $('pfGuest').value = '';
    toast('转发规则已添加');
    refreshPorts();
  } catch (err) { $('pfErr').textContent = err.message; $('pfErr').classList.add('show'); }
}));

async function loadIPLimit(fill) {
  if (!state.activeVm) return;
  try {
    const info = await api('GET', `/api/vms/${state.activeVm}/iplimit`);
    if (fill) $('ipLimitValue').value = info.override === null ? '' : info.override;
    $('ipLimitStatus').textContent = `生效 ${info.effective > 0 ? info.effective : '∞'} · 全局 ${info.global > 0 ? info.global : '∞'}`;
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
$('mgConsole').addEventListener('click', () => { const id = state.activeVm; closeDrawer(); openConsole(id); });

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
  } catch (e) { $('mgErr').textContent = e.message; $('mgErr').classList.add('show'); }
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

/* ══════════════════════════════ 设置 ═══════════════════════════════════════ */
const CONF_FIELDS = ['siteName', 'token', 'platformAddr', 'monitorNIC', 'monitorDisk', 'maxPortForward',
  'maxForwardIPs', 'trafficResetDay', 'entryHost', 'updateRepo', 'updateBase', 'updateApi', 'netavarkBase',
  'publicIpv4', 'webUser', 'webPass'];

function confSnapshot() {
  const o = {};
  CONF_FIELDS.forEach((id) => { o[id] = $(id).value; });
  o.platformInsecure = $('platformInsecure').checked;
  return JSON.stringify(o);
}

function syncDirty() {
  const dirty = state.snapshot !== undefined && confSnapshot() !== state.snapshot;
  state.dirty = dirty;
  $('savebar')?.classList.toggle('show', dirty);
}

function loadConfigIntoForm(c) {
  $('siteName').value = c.site_name || 'Run';
  $('token').value = c.token || '';
  $('monitorNIC').value = c.monitor_nic || '';
  $('monitorDisk').value = c.monitor_disk || '';
  $('maxPortForward').value = c.max_port_forward || 20;
  $('maxForwardIPs').value = c.max_forward_ips || 0;
  $('trafficResetDay').value = c.traffic_reset_day || 1;
  $('entryHost').value = c.host || '';
  $('webUser').value = c.web_user || '';
  $('webPass').value = '';
  $('platformAddr').value = c.platform_addr || '';
  $('updateRepo').value = c.update_repo || '';
  $('updateBase').value = c.update_base_url || '';
  $('updateApi').value = c.update_api_url || '';
  $('netavarkBase').value = c.netavark_base || '';
  $('platformInsecure').checked = !!c.platform_insecure;
  state.snapshot = confSnapshot();
  syncDirty();
  loadNetworkAddresses(c.public_ipv4 || '');
}

// loadNetworkAddresses 拉取宿主机可用的 IPv4 填充下拉框。
// 当前配置值若不在列表里（IP 尚未绑到网卡），补一项保留，避免保存时被清空。
async function loadNetworkAddresses(selected) {
  const sel = $('publicIpv4');
  if (!sel) return;
  let addrs = [];
  let primary = '';
  try {
    const data = await api('GET', '/api/network/addresses');
    addrs = (data && data.ipv4) || [];
    primary = (data && data.primary) || '';
  } catch (e) { /* 拉不到就只留空选项 */ }

  sel.innerHTML = '<option value="">未指定（所有 IP 均可访问）</option>';
  addrs.forEach((ip) => {
    const opt = document.createElement('option');
    opt.value = ip;
    opt.textContent = ip === primary ? `${ip}（主 IP）` : ip;
    sel.appendChild(opt);
  });
  if (selected && !addrs.includes(selected)) {
    const opt = document.createElement('option');
    opt.value = selected;
    opt.textContent = `${selected}（当前配置）`;
    sel.appendChild(opt);
  }
  sel.value = selected || '';
  state.snapshot = confSnapshot();
  syncDirty();
}

CONF_FIELDS.forEach((id) => $(id)?.addEventListener('input', syncDirty));
$('platformInsecure').addEventListener('change', syncDirty);

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
      host: $('entryHost').value.trim(),
      web_user: $('webUser').value,
      web_pass: $('webPass').value,
      platform_addr: $('platformAddr').value.trim(),
      update_repo: $('updateRepo').value.trim(),
      update_base_url: $('updateBase').value.trim(),
      update_api_url: $('updateApi').value.trim(),
      netavark_base: $('netavarkBase').value.trim(),
      public_ipv4: $('publicIpv4').value,
      platform_insecure: $('platformInsecure').checked,
    };
    try {
      await api('POST', '/api/config', body);
      state.config = { ...(state.config || {}), ...body };
      $('webPass').value = '';
      state.snapshot = confSnapshot();
      syncDirty();
      applyBrand(body.site_name);
      renderHostKv();
      toast('配置已保存');
    } catch (e) { toastErr(e); }
  });
}

$('btnSaveConf').addEventListener('click', (e) => saveConfig(e.currentTarget));
$('savebarSave').addEventListener('click', (e) => saveConfig(e.currentTarget));
$('savebarReset').addEventListener('click', () => $('btnReloadConf').click());
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
    $('verCurrent').textContent = d.current || '—';
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
  if (!confirm('升级后服务会自动重启，确认继续？')) return;
  try {
    await api('POST', '/api/upgrade');
    $('verUpgradeNote').hidden = false;
    $('verUpgradeNote').textContent = '升级已开始，面板会短暂失联。';
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

/* ── 镜像配置 ─────────────────────────────────────────────────────────────── */
$('btnImages').addEventListener('click', openImagesConfig);

async function openImagesConfig() {
  try {
    const res = await api('GET', '/api/images/config');
    state.imagesAll = res.all || ['debian', 'alpine'];
    state.imagesActive = res.config || ['debian', 'alpine'];
    state.tempList = [...state.imagesActive, ...state.imagesAll.filter((i) => !state.imagesActive.includes(i))];
    renderImagesList();
    $('modalImages').hidden = false;

    const isPodman = (state.config?.virt_type || '') === 'podman';
    $('customImagesWrap').hidden = !isPodman;
    if (isPodman) refreshCustomImages();
  } catch (e) { toastErr(e); }
}

function renderImagesList() {
  $('imagesList').innerHTML = state.tempList.map((item, idx) => {
    const on = state.imagesActive.includes(item);
    return `
    <div class="list-row">
      <label class="check" style="flex:1">
        <input type="checkbox" data-img="${esc(item)}" ${on ? 'checked' : ''}>
        <span style="text-transform:capitalize">${esc(item)}</span>
      </label>
      <button class="act" data-img-up="${idx}" ${idx === 0 ? 'disabled' : ''} title="上移">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m6 14 6-6 6 6"/></svg>
      </button>
      <button class="act" data-img-down="${idx}" ${idx === state.tempList.length - 1 ? 'disabled' : ''} title="下移">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m6 10 6 6 6-6"/></svg>
      </button>
    </div>`;
  }).join('');

  qsa('[data-img]').forEach((chk) => chk.addEventListener('change', () => collectImagesActive()));
  qsa('[data-img-up]').forEach((btn) => btn.addEventListener('click', () => moveImage(Number(btn.dataset.imgUp), -1)));
  qsa('[data-img-down]').forEach((btn) => btn.addEventListener('click', () => moveImage(Number(btn.dataset.imgDown), 1)));
}

function collectImagesActive() {
  state.imagesActive = qsa('[data-img]').filter((c) => c.checked).map((c) => c.dataset.img);
}

function moveImage(idx, dir) {
  const t = idx + dir;
  if (t < 0 || t >= state.tempList.length) return;
  collectImagesActive();
  [state.tempList[idx], state.tempList[t]] = [state.tempList[t], state.tempList[idx]];
  renderImagesList();
}

$('imagesSave').addEventListener('click', (e) => withBusy(e.currentTarget, async () => {
  collectImagesActive();
  try {
    await api('POST', '/api/images/config', state.imagesActive);
    state.images = ((await api('GET', '/api/images')) || []).filter(Boolean);
    $('modalImages').hidden = true;
    toast('镜像配置已保存');
  } catch (err) { toastErr(err); }
}));

async function refreshCustomImages() {
  try {
    const list = (await api('GET', '/api/images/custom')) || [];
    state.customImages = list;
    const box = $('customImagesList');
    if (!list.length) {
      box.innerHTML = '<div class="small dim" style="padding:8px 2px">还没有自定义镜像</div>';
    } else {
      box.innerHTML = list.map((img, idx) => {
        const badge = img.status === 'ready' ? '<span class="badge badge-ok">READY</span>'
          : img.status === 'pulling' ? '<span class="badge badge-warn">PULLING</span>'
            : '<span class="badge badge-bad">ERROR</span>';
        return `
        <div class="row" style="padding:9px 10px;border-radius:10px;gap:10px;border:1px solid var(--hairline)">
          <div style="min-width:0;flex:1">
            <div class="row" style="gap:8px"><span class="small" style="font-weight:600">${esc(img.name)}</span>${badge}</div>
            <div class="mono dim truncate" style="font-size:10.5px">${esc(img.id)}</div>
          </div>
          ${img.status === 'error' ? `<button class="btn btn-quiet btn-sm" data-ci-retry="${idx}">重试</button>` : ''}
          <button class="act danger" data-ci-del="${idx}" title="删除">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><use href="#i-trash"/></svg>
          </button>
        </div>`;
      }).join('');

      qsa('[data-ci-del]').forEach((btn) => btn.addEventListener('click', async () => {
        const img = state.customImages[Number(btn.dataset.ciDel)];
        if (!img || !confirm(`删除自定义镜像「${img.name}」？`)) return;
        try { await api('DELETE', '/api/images/custom?id=' + encodeURIComponent(img.id)); refreshCustomImages(); }
        catch (e) { toastErr(e); }
      }));
      qsa('[data-ci-retry]').forEach((btn) => btn.addEventListener('click', async () => {
        const img = state.customImages[Number(btn.dataset.ciRetry)];
        if (!img) return;
        try { await api('POST', '/api/images/custom', { id: img.id, name: img.name }); refreshCustomImages(); }
        catch (e) { toastErr(e); }
      }));
    }
    if (list.some((i) => i.status === 'pulling') && !$('modalImages').hidden) setTimeout(refreshCustomImages, 3000);
  } catch { /* 静默 */ }
}

$('ciAdd').addEventListener('click', async () => {
  const id = $('ciRef').value.trim();
  if (!id) return toast('请填镜像地址', 'bad');
  try {
    await api('POST', '/api/images/custom', { id, name: $('ciName').value.trim() });
    $('ciRef').value = ''; $('ciName').value = '';
    toast('已加入拉取队列');
    refreshCustomImages();
  } catch (e) { toastErr(e); }
});

/* ── 浮层通用关闭 ─────────────────────────────────────────────────────────── */
qsa('[data-close]').forEach((el) => el.addEventListener('click', () => { $(el.dataset.close).hidden = true; }));

/* ── 控制台 ───────────────────────────────────────────────────────────────── */
let term = null, termWs = null, fitAddon = null, termObserver = null, termMaximized = false;

function openConsole(vmId) {
  $('consoleTitle').textContent = `console — ${vmId}`;
  $('console').hidden = false;
  document.body.style.overflow = 'hidden';

  if (termWs) { termWs.close(); termWs = null; }
  if (term) { term.dispose(); term = null; }
  if (termObserver) { termObserver.disconnect(); termObserver = null; }

  const host = $('termHost');
  host.innerHTML = '';
  const theme = getComputedStyle(document.documentElement);
  term = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: getComputedStyle(document.body).getPropertyValue('--mono') || 'monospace',
    theme: { background: '#0b0c0e', foreground: '#e6e7ea', cursor: '#e6e7ea', selectionBackground: 'rgba(255,255,255,.18)' },
    convertEol: true,
    scrollback: 4000,
  });
  fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);
  term.open(host);

  termObserver = new ResizeObserver(() => { try { fitAddon.fit(); } catch {} });
  termObserver.observe($('consoleWindow'));

  setTimeout(() => { try { fitAddon.fit(); } catch {} connectWS(vmId); }, 90);
}

function connectWS(vmId) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  termWs = new WebSocket(`${proto}://${location.host}/api/vms/${encodeURIComponent(vmId)}/tty`);
  termWs.binaryType = 'arraybuffer';

  termWs.onopen = () => {
    termWs.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  };
  termWs.onmessage = (e) => {
    if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
    else term.write(e.data);
  };
  termWs.onclose = () => term?.write('\r\n\x1b[38;5;245m[已断开]\x1b[0m\r\n');
  termWs.onerror = () => term?.write('\r\n\x1b[31m[连接错误]\x1b[0m\r\n');

  term.onData((data) => {
    if (termWs?.readyState === WebSocket.OPEN) termWs.send(new TextEncoder().encode(data));
  });
  term.onResize(({ cols, rows }) => {
    if (termWs?.readyState === WebSocket.OPEN) termWs.send(JSON.stringify({ type: 'resize', cols, rows }));
  });
}

function closeConsole() {
  if (termWs) { termWs.close(); termWs = null; }
  if (term) { term.dispose(); term = null; }
  if (termObserver) { termObserver.disconnect(); termObserver = null; }
  termMaximized = false;
  $('consoleWindow').classList.remove('maximized');
  $('console').hidden = true;
  document.body.style.overflow = '';
}

$('cwClose').addEventListener('click', closeConsole);
$('cwMin').addEventListener('click', closeConsole);
$('cwMax').addEventListener('click', toggleConsoleMax);
$('cwFull').addEventListener('click', toggleConsoleMax);

function toggleConsoleMax() {
  termMaximized = !termMaximized;
  const win = $('consoleWindow');
  win.classList.toggle('maximized', termMaximized);
  qs('.console', document).style.padding = termMaximized ? '0' : '';
  setTimeout(() => { try { fitAddon.fit(); } catch {} }, 60);
}

/* ══════════════════════════════ 启动 ═══════════════════════════════════════ */
async function init() {
  applyTheme(localStorage.getItem('run-theme') || 'light');
  setView((location.hash || '#overview').slice(1), { push: false });

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
    $('versionPill').textContent = version?.current || 'v—';
    $('aboutVersion').textContent = version?.current || '—';
    $('aboutVirt').textContent = (state.config.virt_type || '').toUpperCase() || '—';
    $('aboutRepo').textContent = state.config.update_repo || 'GitHub 默认';

    $('monitorNIC').innerHTML = '<option value="">全部网卡</option>' + (sys.nics || []).map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    $('monitorDisk').innerHTML = (sys.disks || []).map((d) => `<option value="${esc(d)}">${esc(d)}</option>`).join('');

    loadConfigIntoForm(state.config);
    renderHostKv();
    updateConnection();
    setInterval(updateConnection, 4000);
  } catch { /* 静默 */ }

  await Promise.all([refreshStatus(), refreshVMs()]);
  moveSegThumb();
  setInterval(refreshStatus, 2000);
  setInterval(refreshVMs, 5000);
  setInterval(moveSegThumb, 1000);

  if (state.view === 'firewall') refreshFirewall();
  setInterval(() => { if (state.view === 'firewall') checkRfw(); }, 10000);
}

async function updateConnection() {
  const pill = $('connPill');
  const text = $('connText');
  const hasToken = !!state.config?.token;
  if (!hasToken) {
    pill.className = 'pill pill-warn';
    pill.innerHTML = '<span class="dot"></span><span id="connText">未配置</span>';
    return;
  }
  try {
    const d = await api('GET', '/api/connection');
    if (d.connected) {
      pill.className = 'pill pill-ok';
      pill.innerHTML = '<span class="dot"></span><span id="connText">已连接</span>';
      pill.title = '平台连接正常';
    } else {
      pill.className = 'pill pill-warn';
      pill.innerHTML = '<span class="dot"></span><span id="connText">连接中</span>';
      pill.title = d.error || '正在连接平台';
    }
  } catch {
    pill.className = 'pill pill-quiet';
    pill.innerHTML = '<span class="dot"></span><span id="connText">未知</span>';
  }
}

$('connPill').addEventListener('click', () => setView('settings'));
$('ovCopyHost').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText($('ovHostAddr').textContent); toast('已复制'); }
  catch { toast('复制失败', 'bad'); }
});

init();
