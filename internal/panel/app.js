'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'l3api.key', LS_THEME = 'l3api.theme';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => { if (theme === 'auto') applyTheme(); });
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
  // 图表颜色取自 CSS 变量与明暗态，主题翻转后必须重绘，否则深色底上留浅色网格。
  if (view === 'usage') loadUsage();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
// API_BASE 面板 API 前缀：从当前页面 URL 推导（/panel/ 或 /l3api/panel/ 等
// 子路径部署场景自动适配），不再硬编码绝对路径 /panel/api/。
// location.pathname 形如 /panel/ 或 /l3api/panel/（或 /panel），
// 去掉尾部非路径字符后补 api/ 即为正确前缀。
const API_BASE = location.pathname.replace(/\/?$/, '/') + 'api/';
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch(API_BASE + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', probe: '健康检验', usage: '用量', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', keys: 'API Keys', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models') { renderModelBrandFilters(); if (!$('mdBody').children.length) loadModels(); }
  if (v === 'config') loadConfig();
  if (v === 'keys') loadKeys();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'taskscenter') { loadSchoolStatus(true); pollQueueOnce(); }
  if (v === 'probe') initProbeView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* ── 健康检验（账号 × 模型 真实调用探测）──────────────────────────────
   后端 POST /panel/api/probe（?uid= 单个），直连上游、绕过池记账。
   行状态机：idle → probing → ok/fail/skipped；全部检验并发发起、逐行刷新。 */
let probeRows = null; // 最近一次结果（{accounts, results}），视图重进时保留
let probeBusy = false;
let probeBrandFilter = 'all'; // 'all' | 品牌名（与账号池页同口径）

function initProbeView() {
  loadProbeSched();
  loadProbeLast();
  if (probeRows && probeRows.accounts) { renderProbeRows(null); return; }
  renderProbeRows(null);
  loadProbeTargets().then(accounts => {
    probeRows = { accounts, results: (probeRows && probeRows.results) || [] };
    renderProbeRows(null);
  }).catch(e => toast('账号列表加载失败：' + (e.message || e), 'err'));
}

/* ── 定时体检设置卡（probe 视图顶部）──────────────────────────────────
   读写 schedule.probe_enabled / probe_hours（与配置页同一字段，POST config
   部分对象深合并）。下次时点在前端按小时列表现算（排程时区固定 CST=UTC+8）。 */
async function loadProbeSched() {
  try {
    const d = await api('config');
    const sch = (d.config && d.config.schedule) || {};
    $('probeSchedEnabled').checked = sch.probe_enabled !== false;
    const hours = Array.isArray(sch.probe_hours) && sch.probe_hours.length ? sch.probe_hours : [9];
    $('probeSchedHours').value = hours.join(', ');
    renderProbeNext($('probeSchedEnabled').checked, hours);
  } catch (e) { $('probeSchedNext').textContent = '设置读取失败：' + (e.message || e); }
}

function renderProbeNext(enabled, hours) {
  const el = $('probeSchedNext');
  if (!enabled) { el.textContent = '定时体检已关闭'; return; }
  const clean = (hours || []).map(Number).filter(h => h >= 0 && h <= 23);
  if (!clean.length) { el.textContent = ''; return; }
  // 与后端 nextFire 同口径：CST 当日未过的最近整点，否则明日最早整点。
  const now = new Date(Date.now() + 8 * 3600 * 1000); // CST 墙钟（用 UTC 字段读）
  const sorted = clean.slice().sort((a, b) => a - b);
  const curH = now.getUTCHours();
  const nxt = sorted.find(h => h > curH);
  el.textContent = nxt !== undefined
    ? '下次定时体检：今天 ' + String(nxt).padStart(2, '0') + ':00'
    : '下次定时体检：明天 ' + String(sorted[0]).padStart(2, '0') + ':00';
}

$('btnProbeSchedSave').onclick = async () => {
  const raw = $('probeSchedHours').value.trim();
  const hours = raw ? raw.split(/[,，\s]+/).filter(Boolean).map(Number) : [];
  if (!hours.length || hours.some(h => !Number.isInteger(h) || h < 0 || h > 23)) {
    toast('体检时点需为 0-23 的整数小时（逗号分隔）', 'err');
    return;
  }
  const btn = $('btnProbeSchedSave');
  btn.disabled = true;
  try {
    await api('config', {
      method: 'POST',
      body: JSON.stringify({ schedule: { probe_enabled: $('probeSchedEnabled').checked, probe_hours: hours } })
    });
    toast('定时体检设置已保存并生效', 'ok');
    renderProbeNext($('probeSchedEnabled').checked, hours);
    cfgLoaded = null; // 配置页下次进入时重新拉取，避免旧值覆盖
  } catch (e) { toast('保存失败：' + (e.message || e), 'err'); }
  finally { btn.disabled = false; }
};

async function loadProbeLast() {
  const line = $('probeLastLine'), detail = $('probeLastDetail');
  try {
    const d = await api('probe/last');
    if (!d.ran_at) { line.textContent = '上次定时体检：尚未执行'; detail.innerHTML = ''; return; }
    const bad = (d.results || []).filter(r => !r.ok);
    const when = new Date(d.ran_at).toLocaleString('zh-CN', { hour12: false });
    line.innerHTML = '上次定时体检：<b>' + esc(when) + '</b> · 正常 <b>' + (d.ok || 0) +
      '</b> / 共 ' + (d.total || 0) + (bad.length ? ' · <span style="color:var(--bad)">异常 ' + bad.length + '</span>' : '');
    detail.innerHTML = bad.length
      ? '<div class="hint" style="margin-top:4px;color:var(--bad)">' +
        bad.map(r => esc((r.nickname || r.uid.slice(0, 8)) + '（' + r.brand + '）：' + (r.error || '未知错误'))).join('<br>') + '</div>'
      : '';
  } catch (e) { line.textContent = '上次定时体检：读取失败'; }
}

async function loadProbeTargets() {
  const d = await api('overview');
  return (d.accounts || []);
}

function renderProbeRows(accounts, results) {
  const host = $('probeSections');
  if (!host) return;
  const rows = accounts || (probeRows ? probeRows.accounts : null);
  // 品牌筛选按钮组（按当前实际存在的品牌渲染；与账号池页同一交互）。
  const fb = $('probeFilterBtns');
  if (fb && rows) {
    const present = [...new Set(rows.map(brandOf))];
    if (!present.includes(probeBrandFilter)) probeBrandFilter = 'all';
    const opts = ['all'].concat(BRAND_ORDER.filter(b => present.includes(b)));
    fb.innerHTML = opts.map(b =>
      '<button class="xs' + (probeBrandFilter === b ? ' primary' : '') + '" data-pbf="' + b + '">' +
      (b === 'all' ? '全部' : (BRAND_LABEL[b] || b)) + '</button>').join(' ');
  }
  if (!rows || !rows.length) {
    host.innerHTML = '<div class="empty">暂无账号</div>';
    return;
  }
  let list = rows;
  if (probeBrandFilter !== 'all') list = list.filter(s => brandOf(s) === probeBrandFilter);
  const resMap = {};
  (results || (probeRows && probeRows.results) || []).forEach(r => { resMap[r.uid] = r; });
  const groups = groupByBrand(list);
  host.innerHTML = groups.map(g => {
    const items = g.items;
    let ok = 0, bad = 0, untested = 0;
    for (const a of items) {
      const r = resMap[a.uid];
      if (!r || r._probing) untested++;
      else if (r.ok) ok++;
      else if (r.skipped) untested++;
      else bad++;
    }
    const head = '<section class="acc-section">' +
      '<header class="acc-section-hd">' + brandBadge(g.brand) +
        '<span class="acc-section-stats">正常 <b>' + ok + '</b> · 异常 <b>' + bad + '</b> · 未检验 <b>' + untested + '</b></span>' +
        '<span class="grow"></span></header>' +
      '<div class="tbl-wrap"><table class="acc"><thead><tr><th></th><th>账号</th><th>探测模型</th><th>结果</th><th>延迟</th><th>回答/错误</th><th style="text-align:right">操作</th></tr></thead><tbody>';
    const body = items.map(a => probeRowHtml(a, resMap[a.uid])).join('');
    return head + body + '</tbody></table></div></section>';
  }).join('');
}

function probeRowHtml(a, r) {
  const short = a.uid.length > 16 ? a.uid.slice(0, 16) + '…' : a.uid;
  let stCell = '<span style="color:var(--ink-3)">未检验</span>';
  let detail = '<span style="color:var(--ink-3)">—</span>';
  let lat = '—';
  if (r && r._probing) {
    stCell = '<span class="tag">检验中…</span>';
  } else if (r) {
    if (r.ok) {
      stCell = '<span class="tag ok">正常</span>';
      detail = '<span style="color:var(--ink-2)">' + esc(r.reply || '(空应答)') + '</span>';
    } else if (r.skipped) {
      stCell = '<span class="tag">跳过</span>';
      detail = '<span style="color:var(--ink-3)">' + esc(r.error || '') + '</span>';
    } else {
      stCell = '<span class="tag bad">异常</span>';
      detail = '<span style="color:var(--bad)">' + esc(r.error || '未知错误') + '</span>';
    }
    if (r.latency_ms) lat = r.latency_ms + 'ms';
  }
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"><div class="nm">' + (a.nickname ? esc(a.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + '</div><div class="id">' + esc(short) + '</div></td>' +
    '<td class="num" style="font-size:12px">' + esc((r && r.model) || probeModelHint(a)) + '</td>' +
    '<td>' + stCell + '</td>' +
    '<td class="num">' + lat + '</td>' +
    '<td style="max-width:340px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + detail + '</td>' +
    '<td><div class="acts"><button class="xs ghost" data-probe-u="' + esc(a.uid) + '">检验</button></div></td></tr>';
}

function probeModelHint(a) {
  if (a.brand === 'qoder') return 'auto';
  if (a.brand === 'trae') return 'glm-5.3-flashx';
  return 'glm-5.2';
}

async function runProbe(uid) {
  if (probeBusy) return;
  probeBusy = true;
  const btn = $('btnProbeAll');
  if (btn) { btn.disabled = true; btn.textContent = '检验中…'; }
  try {
    if (!probeRows || !probeRows.accounts) {
      const accounts = await loadProbeTargets();
      probeRows = { accounts, results: [] };
    }
    if (uid) {
      // 单账号：行内转圈
      const ex = (probeRows.results || []).find(r => r.uid === uid);
      if (ex) ex._probing = true;
      else (probeRows.results = probeRows.results || []).push({ uid, _probing: true });
      renderProbeRows(null);
      const d = await api('probe?uid=' + encodeURIComponent(uid), { method: 'POST' });
      const r = (d.results || [])[0];
      probeRows.results = (probeRows.results || []).filter(x => x.uid !== uid).concat(r ? [r] : []);
    } else {
      probeRows.results = probeRows.accounts.map(a => ({ uid: a.uid, _probing: true }));
      renderProbeRows(null);
      const d = await api('probe', { method: 'POST' });
      probeRows.results = d.results || [];
      const bad = (d.results || []).filter(r => !r.ok && !r.skipped).length;
      toast('检验完成：' + (d.ok || 0) + '/' + (d.total || 0) + ' 正常' + (bad ? '，' + bad + ' 个异常' : ''), bad ? 'err' : 'ok');
    }
    renderProbeRows(null);
  } catch (e) {
    toast('检验失败：' + (e.message || e), 'err');
    if (probeRows) { probeRows.results = (probeRows.results || []).map(r => { delete r._probing; return r; }); renderProbeRows(null); }
  } finally {
    probeBusy = false;
    if (btn) { btn.disabled = false; btn.textContent = '检验全部账号'; }
  }
}

document.addEventListener('click', e => {
  const cp = e.target.closest('.copy-call');
  if (cp) {
    copyText(cp.dataset.copy).then(
      () => toast('已复制调用名：' + cp.dataset.copy, 'ok'),
      () => toast('复制失败', 'err'));
    return;
  }
  const b = e.target.closest('[data-probe-u]');
  if (b) { runProbe(b.dataset.probeU); return; }
  const fb = e.target.closest('button[data-pbf]');
  if (fb) { probeBrandFilter = fb.dataset.pbf; renderProbeRows(null); return; }
  if (e.target.id === 'btnProbeAll') runProbe(null);
});


/* ── 品牌分组（账号池 / 积分构成 / 任务中心 / 用量按账号 共用）────────────
   上游品牌：codebuddy（腾讯）/ qoder（阿里）/ trae（字节）。同一品牌的账号
   归到一个区块，各自带独立统计头与批量操作按钮——「把不同的池子分开」。
   能力门控 BRAND_CAPS 与调度器 supports() 能力矩阵一致：qoder 余额 + 权益
   监测（签到按钮实际打的是 campaigns 检测，见 upstream/qoder_checkin.go）、
   trae 签到 + 余额（签到走 ug/checkin_credits，余额走 pay/ide_user_ent_usage）。 */
const BRAND_ORDER = ['codebuddy', 'codebuddy-global', 'qoder', 'trae'];
const MODEL_BRAND_FILTERS = ['all', 'codebuddy', 'codebuddy-global', 'qoder', 'trae'];
let modelBrandFilter = 'all';
const BRAND_LABEL = { codebuddy: 'CodeBuddy 国内版', 'codebuddy-global': 'CodeBuddy 国际版', qoder: 'Qoder', trae: 'Trae' };
const BRAND_CAPS = {
  codebuddy: ['checkin', 'travel', 'activity', 'keepalive'],
  'codebuddy-global': [],
  qoder: ['checkin', 'balance'],
  trae: ['checkin', 'balance'],
};
const BRAND_BTN = {
  checkin: '签到', travel: '旅行巡检', activity: '活跃上报', keepalive: '保活', balance: '刷新余额',
};
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');

// displayBrand 展示用品牌键：codebuddy 按 realm 拆 国内版/国际版（v1.12 用户要求
// 账号池分区，v1.13 起任务中心/队列/用量同口径）；其余品牌无 realm 概念原样返回。
// 服务端的 brand 字段不变（pool/scheduler 仍认 'codebuddy'），这里只是展示层分组键。
// 上游 codebuddy 国内与国际版**同名**，只靠 realm 区分，故必须同时看 brand+realm。
function displayBrand(brand, realm) {
  const b = brand || 'codebuddy';
  return (b === 'codebuddy' && realm === 'global') ? 'codebuddy-global' : b;
}
// brandOf 从账号状态对象取展示品牌键（对象带 brand+realm）。
function brandOf(s) {
  return displayBrand(s && s.brand, s && s.realm);
}
// baseBrand 展示键 → 服务端真实品牌（批量操作/能力门控用）
function baseBrand(b) { return b === 'codebuddy-global' ? 'codebuddy' : b; }
// uid → 品牌映射（来自 overview 账号快照；用量/队列等只带 uid 的视图靠它反查）。
// 存**展示键**（含 codebuddy-global），这样只带 uid 的视图也能正确分区。
let BRAND_OF_UID = {};
function rebuildBrandMap() {
  BRAND_OF_UID = {};
  for (const s of (overviewData?.accounts || [])) BRAND_OF_UID[s.uid] = brandOf(s);
}
function brandOfUID(uid, fallbackBrand) {
  return BRAND_OF_UID[uid] || fallbackBrand || 'codebuddy';
}
// 冷却秒数（熔断/连败降权/积分冷却/限流冷却取最大）
function coolOf(s) {
  const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
  const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
  return Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
}
// 按品牌把账号数组切成有序分组（只列实际出现的品牌，未知品牌排最后）
function groupByBrand(list) {
  const map = {};
  for (const s of list) (map[brandOf(s)] ||= []).push(s);
  const seen = new Set(list.map(brandOf));
  const order = BRAND_ORDER.filter(b => seen.has(b));
  for (const b of seen) if (!order.includes(b)) order.push(b);
  return order.map(b => ({ brand: b, items: map[b] }));
}
// 品牌徽标 HTML（codebuddy 也展示，避免与旧「缺省不展示」逻辑混用——这里分区了，必须标出来源）
function brandBadge(b) {
  return '<span class="brand-badge ' + esc(b) + '" title="' + esc(BRAND_LABEL[b] || b) + '">' +
    esc(BRAND_LABEL[b] || b) + '</span>';
}

/* ── 账号池 ───────────────────────────────────────────────────────── */
let brandFilter = 'all'; // 'all' | 'codebuddy' | 'codebuddy-global' | 'qoder' | 'trae'

// ── 营销/折扣活动时段 ────────────────────────────────────────────────
// 时段此前硬编码 23:00–08:00（实测上游真实口径是 23:00–7:50，且不同活动各不相同）。
// 现以后端 promotion（解析自 /v3/config modelPromotions，带准确 schedule）为准，
// 本地不再猜时段——只负责判定与渲染。

// promoOf 取模型生效活动的展示态：null 表示无活动。
// active 由**后端**按 CST 现算下发（后端每次请求都算，不受 /v1/models 1h 缓存影响）。
function promoOf(m) {
  return m.promotion || null;
}
// promoFree 该活动是否为「免费」（折后 0x / factor 0）。
function promoFree(p) {
  if (!p) return false;
  const c = (p.credit || '').trim();
  return (typeof p.factor === 'number' && p.factor === 0) || c === '0x' || c === '0' || c === 'x0.00';
}
// freeState 模型当前免费状态：'always' 全时段免费 | 'win' 活动生效中 | 'win-off' 活动未生效 | '' 无活动。
// 判定优先用结构化 promotion.active；无活动数据时回落徽章文字（兜底窗口在后端）。
function freeState(m) {
  const c = (m.credits || '').trim();
  if (/^x?0(\.0+)?($|\s)/.test(c)) return 'always'; // 倍率 x0.00 = 全时段免费（必须有结尾/空格锚定——无锚定时 x0.05 的前缀 x0 会误判，2026-10-12 修）
  const p = promoOf(m);
  if (p && promoFree(p)) return p.active ? 'always' : 'win-off';
  if (p) return p.active ? 'win' : 'win-off';
  const b = m.badge || '';
  if (b.includes('限时免费')) return 'always';
  return '';
}

// fmtNum 千分位（积分展示用）
function fmtNum(n) { return (n || 0).toLocaleString('en-US'); }

// 品牌卡片墙：每品牌一张卡（账号数/状态分解/余额条），点击筛选账号列表。
// 「全部品牌」不是品牌而是汇总——单独渲染成通栏总览条（#poolSummary），不进卡片墙。
// 数据全部来自 overview.accounts 前端聚合（与分块统计头同口径 coolOf()）。
function renderBrandCards(all) {
  const host = $('brandCards');
  if (!host) return;
  const agg = b => {
    const items = b === 'all' ? all : all.filter(s => brandOf(s) === b);
    let avail = 0, cooling = 0, disabled = 0, expired = 0, rem = 0, tot = 0;
    for (const s of items) {
      // 口径与账号行状态列一致：禁用 > Session 过期 > 冷却 > 正常
      if (s.disabled) disabled++;
      else if (s.session_dead) expired++;
      else if (coolOf(s) > 0) cooling++;
      else avail++;
      rem += s.credits || 0; tot += s.credits_total || 0;
    }
    return { n: items.length, avail, cooling, disabled, expired, rem, tot };
  };
  // 通栏总览条：左大数字 / 中状态分段 / 右积分仪表
  const sumHost = $('poolSummary');
  if (sumHost) {
    const a = agg('all');
    const brandCount = new Set(all.map(brandOf)).size;
    const pct = a.tot > 0 ? Math.min(100, Math.round(a.rem / a.tot * 100)) : (a.n ? 100 : 0);
    const stat = (cls, label, v) =>
      '<span class="ps-stat"><span class="k"><i class="' + cls + '"></i>' + label + '</span><span class="v">' + v + '</span></span>';
    sumHost.innerHTML = '<button class="psum' + (brandFilter === 'all' ? ' on' : '') + '" data-bf="all">' +
      '<span class="ps-id">' +
        '<span class="ps-logo">Σ</span>' +
        '<span><span class="ps-name">总统计</span>' +
        '<div class="ps-num">' + brandCount + '<em>个品牌</em><span class="ps-sep">·</span>' + a.n + '<em>个账号</em></div></span>' +
      '</span>' +
      '<span class="ps-stats">' +
        stat('ok', '正常', a.avail) + stat('warn', '冷却', a.cooling) +
        stat('dead', '过期', a.expired) + stat('bad', '禁用', a.disabled) +
      '</span>' +
      '<span class="ps-cred">' +
        '<span class="ps-cred-row"><span class="ps-cred-t">' + (a.tot > 0
          ? '积分 ' + fmtNum(a.rem) + '<i>/' + fmtNum(a.tot) + '</i>'
          : '积分 ' + fmtNum(a.rem)) + '</span>' +
        '<span class="ps-pct">' + (a.tot > 0 ? pct + '%' : '—') + '</span></span>' +
        '<span class="ps-bar"><i style="width:' + pct + '%"></i></span>' +
      '</span></button>';
  }
  const card = (b, name) => {
    const a = agg(b);
    const pct = a.tot > 0 ? Math.min(100, Math.round(a.rem / a.tot * 100)) : (a.n ? 100 : 0);
    const letter = name[0] || '?';
    return '<button class="bcard ' + esc(b) + (brandFilter === b ? ' on' : '') +
      '" data-bf="' + b + '">' +
      '<span class="bc-logo ' + esc(b) + '">' + esc(letter) + '</span>' +
      '<span class="bc-main">' +
        '<span class="bc-name">' + esc(name) + '</span>' +
        '<div class="bc-num">' + a.n + '<em>个账号</em></div>' +
        '<span class="bc-sub">' +
          '<span><i class="ok"></i>正常 <b>' + a.avail + '</b></span>' +
          '<span><i class="warn"></i>冷却 <b>' + a.cooling + '</b></span>' +
          '<span><i class="dead"></i>过期 <b>' + a.expired + '</b></span>' +
          '<span><i class="bad"></i>禁用 <b>' + a.disabled + '</b></span>' +
        '</span>' +
        '<span class="bc-cred">' +
          '<span class="bc-cred-t">' + (a.tot > 0
            ? '积分 ' + fmtNum(a.rem) + '<i>/' + fmtNum(a.tot) + '</i>'
            : '积分 ' + fmtNum(a.rem)) + '</span>' +
          '<span class="bc-pct">' + (a.tot > 0 ? pct + '%' : '—') + '</span>' +
        '</span>' +
        '<span class="bc-bar"><i style="width:' + pct + '%"></i></span>' +
      '</span></button>';
  };
  const present = [...new Set(all.map(brandOf))];
  const brands = BRAND_ORDER.filter(b => present.includes(b));
  host.classList.toggle('bcards-has-sel', brandFilter !== 'all');
  host.innerHTML = brands.map(b => card(b, BRAND_LABEL[b] || b)).join('');
}

// scrollToAccounts 列表滚动到账号区（仅点击切品牌时调一次；render 里每 5s
// 定时刷新也走这里的话，用户看总览条时会被反复拽回去）。
function scrollToAccounts() {
  const body = $('accBody');
  if (body) body.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function renderAccounts(list) {
  // 品牌卡片墙（v1.12）：全量聚合渲染——筛选时卡片不消失，靠选中态表达。
  renderBrandCards(list);
  if (brandFilter !== 'all') list = list.filter(s => brandOf(s) === brandFilter);
  const host = $('accBody');
  if (!list.length) {
    host.innerHTML = '<div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  const rowHTML = s => {
    const cool = coolOf(s);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad"><span class="pdot bad"></span>已禁用</span>'; }
    else if (s.session_dead) {
      // session 失效（401）：未达禁用阈值也醒目标出——账号实际已不可用，需重新登录
      cls = 'off';
      tag = '<span class="tag bad"><span class="pdot bad"></span>Session 过期</span>';
    }
    else if (cool > 0) {
      cls = 'cool';
      const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
      const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn"><span class="pdot warn"></span>' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok"><span class="pdot ok"></span>可用</span>';
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
    const pct = s.credits_total > 0
      ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
      : Math.round((s.credits || 0) / maxCred * 100);
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip = s.credits_total > 0 ? '剩余 ' + s.credits + ' / 总额 ' + s.credits_total + '（' + pct + '%）' : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" data-row-uid="' + esc(s.uid) + '" title="点击查看账号详情">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm nm-flex"><span class="nmtxt">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + '</span></div><div class="id">' + esc(short) + '</div>' +
        (s.note ? '<div class="hint" style="font-size:11px;color:var(--ink-3);margin-top:2px" title="' + esc(s.note) + '">备注：' + esc(s.note) + '</div>' : '') + '</td>' +
      '<td>' + tag + note + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td><div class="acts">' +
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '">签到</button>' +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">余额</button>' +
        (s.brand === 'qoder' || s.brand === 'trae'
          ? ''
          : '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost" data-a="note" data-u="' + esc(s.uid) + '" data-note="' + esc(s.note || '') + '">备注</button>' +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</div></td></tr>';
  };
  // 按品牌分块：每块带独立统计头 + 按能力门控的批量操作按钮。
  const sections = groupByBrand(list).map(g => {
    const items = g.items;
    let avail = 0, cooling = 0, disabled = 0, expired = 0, rem = 0, tot = 0;
    for (const s of items) {
      if (s.disabled) disabled++;
      else if (s.session_dead) expired++;
      else if (coolOf(s) > 0) cooling++;
      else avail++;
      rem += s.credits || 0; tot += s.credits_total || 0;
    }
    const caps = BRAND_CAPS[g.brand] || [];
    const btns = caps.map(c =>
      '<button class="xs" data-brand-all="' + c + '" data-brand="' + esc(g.brand) + '">' + BRAND_BTN[c] + '</button>'
    ).join('');
    const brandNote = (g.brand === 'trae' || g.brand === 'qoder')
      ? '<span class="note" style="color:var(--ink-3);font-size:11px">（独立品牌路由，无运营任务体系）</span>' : '';
    return '<section class="acc-section">' +
      '<header class="acc-section-hd">' + brandBadge(g.brand) + brandNote +
        '<span class="acc-section-stats">可用 <b>' + avail + '</b> · 冷却 <b>' + cooling + '</b> · 过期 <b>' + expired + '</b> · 禁用 <b>' + disabled + '</b> · 积分 <b>' + rem + (tot > 0 ? '/' + tot : '') + '</b></span>' +
        '<span class="grow"></span>' + btns +
      '</header>' +
      '<div class="tbl-wrap"><table class="acc"><thead><tr>' +
        '<th class="mark" aria-hidden="true"></th><th>账号</th><th>状态</th><th>积分</th><th>成功 / 失败</th><th>在途</th><th>用量</th><th>最近成功</th><th class="c-acts"></th>' +
      '</tr></thead><tbody>' + items.map(rowHTML).join('') + '</tbody></table></div>' +
    '</section>';
  }).join('');
  host.innerHTML = sections;
}

/* ── 账号详情弹窗（v1.12）─────────────────────────────────────────── */
let accDlgUID = null;

function openAcctDlg(uid) {
  const a = (overviewData?.accounts || []).find(x => x.uid === uid);
  if (!a) return;
  accDlgUID = uid;
  renderAcctDlg(a);
  $('accVeil').classList.add('on');
}
function closeAcctDlg() { $('accVeil').classList.remove('on'); accDlgUID = null; }

function renderAcctDlg(a) {
  const b = brandOf(a);
  const cool = coolOf(a);
  const stTag = a.disabled
    ? '<span class="tag bad"><span class="pdot bad"></span>已禁用</span>'
    : a.session_dead
      ? '<span class="tag bad"><span class="pdot bad"></span>Session 过期</span>'
      : cool > 0
      ? '<span class="tag warn"><span class="pdot warn"></span>冷却中 · ' + dur(cool) + '</span>'
      : '<span class="tag ok"><span class="pdot ok"></span>可用</span>';
  $('accDlgHd').innerHTML =
    '<span class="bc-logo ' + esc(b) + '">' + esc((BRAND_LABEL[b] || b)[0] || '?') + '</span>' +
    '<span><span class="nm">' + esc(a.nickname || '未命名') + '</span> ' + brandBadge(b) +
    (a.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + ' ' + stTag +
    '<div class="id">' + esc(a.uid) + '</div></span>';

  const tu = a.token_usage || {};
  const pct = a.credits_total > 0 ? Math.min(100, Math.round((a.credits || 0) / a.credits_total * 100)) : 0;
  const kv = (k, v) => '<dt>' + k + '</dt><dd>' + v + '</dd>';
  let body = '<div class="adlg-grid">' +
    '<div class="acell"><div class="v">' + (a.credits == null ? '—' : a.credits) + '</div><div class="k">剩余积分' + (a.credits_total > 0 ? ' / ' + a.credits_total + '（' + pct + '%）' : '') + '</div></div>' +
    '<div class="acell"><div class="v">' + (a.success_count || 0) + ' / <span style="color:var(--bad)">' + (a.err_total || 0) + '</span></div><div class="k">成功 / 失败（连败 ' + (a.consecutive_fails || 0) + '）</div></div>' +
    '<div class="acell"><div class="v">' + (a.in_flight || 0) + '</div><div class="k">当前在途</div></div>' +
    '<div class="acell"><div class="v">' + (tu.request_count || 0) + '</div><div class="k">累计请求</div></div>' +
    '<div class="acell"><div class="v">' + formatTokenCount(tu.total_tokens) + '</div><div class="k">累计 tokens</div></div>' +
    '<div class="acell"><div class="v">' + formatLatency(tu.last_latency_ms) + '</div><div class="k">最近延迟 · ' + formatRate(tu.last_tokens_per_second) + '</div></div>' +
    '</div>' +
    '<div class="adlg-sec">状态明细</div><dl class="adlg-kv">' +
    kv('最近成功', ago(a.last_success)) +
    (a.session_dead && !a.disabled ? kv('Session 状态', '<span style="color:var(--bad,#e5534b)">已过期（401）——签到/余额/对话均不可用，请重新添加或登录该账号</span>') : '') +
    kv('冷却剩余', cool > 0 ? dur(cool) + (a.cool_kind ? '（' + esc(a.cool_kind) + '）' : '') : '—') +
    kv('熔断连败', (a.breaker_fails || 0) + ' 次') +
    (a.reason ? kv('冷却原因', esc(a.reason)) : '') +
    (a.disabled_reason ? kv('禁用原因', esc(a.disabled_reason)) : '') +
    (a.bot_name ? kv('智能体', esc(a.bot_name)) : '') +
    '</dl>';
  if (a.last_err) {
    body += '<div class="adlg-sec">最近错误</div><div class="adlg-err">' + esc(a.last_err) + '</div>';
  }
  const costs = (a.model_costs || []).filter(c => c.model);
  if (costs.length) {
    body += '<div class="adlg-sec">实测单价（credits/1K）</div><dl class="adlg-kv">' +
      costs.map(c => kv(esc(c.model), c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('') + '</dl>';
  }
  // 免费额度尽台账（free_guard）：免费促销窗口内实测扣费的模型 + 复位点。
  const fex = (a.free_exhausted_models || []).filter(f => f.model);
  if (fex.length) {
    const remain = iso => {
      const s = Math.round((new Date(iso) - Date.now()) / 1000);
      return s > 0 ? dur(s) + ' 后重置' : '即将重置';
    };
    body += '<div class="adlg-sec">免费额度已用尽（守护中）</div><dl class="adlg-kv">' +
      fex.map(f => kv(esc(f.model), '<span style="color:var(--warn,#d29922)">' + remain(f.until) + '</span>')).join('') + '</dl>';
  }
  body += '<div class="adlg-sec">备注</div><div class="adlg-note">' +
    '<input id="accDlgNote" maxlength="100" placeholder="留空 = 清除备注" value="' + esc(a.note || '') + '">' +
    '<button class="xs" id="accDlgNoteSave">保存备注</button></div>';
  $('accDlgBody').innerHTML = body;

  // 操作按钮：与表格行同一套 data-a 动作（事件统一走下面的 accVeil 委托）。
  const frozen = a.disabled || cool > 0;
  $('accDlgActs').innerHTML =
    '<button class="xs ghost" data-a="checkin" data-u="' + esc(a.uid) + '">签到</button>' +
    '<button class="xs ghost" data-a="balance" data-u="' + esc(a.uid) + '">查余额</button>' +
    (b === 'codebuddy' ? '<button class="xs ghost" data-a="tasks" data-u="' + esc(a.uid) + '">任务</button>' : '') +
    (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(a.uid) + '">解冻</button>'
            : '<button class="xs ghost" data-a="disable" data-u="' + esc(a.uid) + '">禁用</button>') +
    '<button class="xs ghost danger" data-a="remove" data-u="' + esc(a.uid) + '">移除</button>';
}

// 弹窗内动作：复用表格同一套 API；完成后刷新（loadOverview 会重渲染开着的弹窗）。
$('accVeil').addEventListener('click', async ev => {
  if (ev.target === $('accVeil')) { closeAcctDlg(); return; } // 点遮罩关闭
  if (ev.target.closest('#accDlgClose')) { closeAcctDlg(); return; }
  if (ev.target.closest('#accDlgNoteSave')) {
    const v = ev.target.closest('.adlg-note').querySelector('input').value.trim();
    try {
      await api('accounts/' + encodeURIComponent(accDlgUID) + '/note', { method: 'POST', body: JSON.stringify({ note: v }) });
      toast(v ? '备注已保存' : '备注已清除', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    loadOverview(true);
    return;
  }
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
      closeAcctDlg();
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
      closeAcctDlg();
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});
document.addEventListener('keydown', ev => {
  if (ev.key === 'Escape' && $('accVeil').classList.contains('on')) closeAcctDlg();
});

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    rebuildBrandMap();
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    // 今日消耗（v1.16.9）：后端按今日小时桶累加上游 usage.credit 实测扣费。
    // 整数就显示整数，有小数保留一位（积分额度本身是整数，小数来自按 token 折算）。
    const td = Number(d.today_credits || 0);
    $('sCreditsToday').textContent = Number.isInteger(td) ? fmtNum(td) : td.toFixed(1);
    $('sToday').textContent = fmtNum(d.today_requests || 0) + ' · ' + fmtTok(d.today_tokens || 0);
    $('sInFlight').textContent = d.in_flight || 0;
    $('sSticky').textContent = d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
    // 详情弹窗开着时同步刷新内容（数据已变化）。
    if (accDlgUID) {
      const a = (d.accounts || []).find(x => x.uid === accDlgUID);
      if (a) renderAcctDlg(a); else closeAcctDlg();
    }
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', ev => {
  if (ev.target.closest('button')) return; // 操作按钮不进详情
  const tr = ev.target.closest('tr[data-row-uid]');
  if (tr) openAcctDlg(tr.dataset.rowUid);
});

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'note') {
      const v = prompt('备注（留空清除，最长 100 字）：', b.dataset.note || '');
      if (v === null) { b.disabled = false; loadOverview(true); return; }
      await api('accounts/' + encodeURIComponent(u) + '/note', { method: 'POST', body: JSON.stringify({ note: v }) });
      toast(v ? '备注已保存' : '备注已清除', 'ok');
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

// 区块级批量操作：每个品牌区块头部按钮按品牌触发（?brand= 透传到调度器）。
// 能力门控 BRAND_CAPS 已决定哪些按钮出现在哪个品牌区块。
$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-brand-all]');
  if (!b) return;
  const brand = b.dataset.brand, act = b.dataset.brandAll;
  const endpoint = { checkin: 'checkin_all', travel: 'travel_all', activity: 'activity_all', keepalive: 'keepalive_all', balance: 'balance_all' }[act];
  if (!endpoint) return;
  b.disabled = true;
  try {
    await api(endpoint + '?brand=' + encodeURIComponent(brand), { method: 'POST' });
    toast((BRAND_LABEL[brand] || brand) + '：' + BRAND_BTN[act] + '已开始，结果见日志', 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

function renderModelBrandFilters() {
  const el = $('modelBrandFilters');
  if (!el) return;
  el.innerHTML = MODEL_BRAND_FILTERS.map(b =>
    '<button class="xs' + (b === modelBrandFilter ? ' primary' : '') + '" data-mbf="' + b + '">' +
    (b === 'all' ? '全部' : BRAND_LABEL[b] || b) + '</button>'
  ).join('');
  el.querySelectorAll('button').forEach(btn => {
    btn.onclick = () => { modelBrandFilter = btn.dataset.mbf; renderModelBrandFilters(); loadModels(); };
  });
}
async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    let list = d.models || [];
    // 品牌筛选：all 显示全部，否则只显示选中品牌
    if (modelBrandFilter !== 'all') {
      list = list.filter(m => m.brand === modelBrandFilter);
    }
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>'; return; }
    const probes = pr.probes || {};
    const probeKeys = Object.keys(probes);
    const probeOf = id => probes[id] || probes[probeKeys.find(k => k.endsWith(':' + id))];
    tb.innerHTML = list.map(m => {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
      const caps = [];
      if (m.is_default) caps.push('<span class="tag mute">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
      if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
      if (m.badge) caps.push('<span class="tag bad"' + (promoOf(m) && promoOf(m).text ? ' title="' + esc(promoOf(m).text) + '"' : '') + '>' + esc(m.badge) + '</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      const bdg = m.brand ? brandBadge(m.brand) : '';
      // 主展示 = 真实调用名（call_id，含品牌/域前缀，客户端原样填进 model 字段）；
      // 点击复制。裸上游 id 与展示名退为次级信息。
      const callId = m.call_id || m.id;
      const sub = m.name && m.name !== m.id ? esc(m.name) + ' · ' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '>' + bdg +
        '<div class="nm"><a href="javascript:void 0" class="copy-call" data-copy="' + esc(callId) + '" title="点击复制调用名" style="font-family:var(--mono);color:var(--ink);text-decoration:none;border-bottom:1px dashed var(--ink-3)">' + esc(callId) + '</a></div>' +
        '<div class="id">' + sub + '上游 ID: ' + esc(m.id) + '</div>' + capHtml + '</td>' +
        '<td class="num">' + (() => {
          const fs = freeState(m);
          const p = promoOf(m);
          // 准确时段（后端从 modelPromotions.schedule 解析，如 23:00–07:50 / 10-12 至 10-25）；
          // 无时段（错峰使用这类）时只显示徽章，原文说明悬浮在徽章上。
          const win = p ? (p.window || '') : '';
          const note = (txt, color) => '<div class="note" style="color:' + color + ';font-size:11px">' + txt + '</div>';
          if (fs === 'always') {
            // 全时段免费（x0.00 或活动生效中）：有窗口则标注窗口，否则只标免费。
            return '<span class="tag ok">免费</span>' +
              (win && p && !promoFree(p) ? '' : (win ? note(esc(win), 'var(--ink-3)') : ''));
          }
          if (fs === 'win') {
            // 折扣活动生效中：绿标 + 折后倍率 + 窗口。
            // 无窗口（错峰使用：真含义只在原文里）→ 徽章 + 原价 + 提示看徽章说明。
            if (!win) {
              const lb2 = (p && p.label) || m.badge || '优惠';
              return esc(m.credits || '—') + note(esc(lb2) + ' · 见徽章说明', 'var(--ok)');
            }
            const cr = p && p.credit ? esc(p.credit) : esc(m.credits || '—');
            return '<span class="tag ok">' + cr + '</span>' + note(esc(win) + ' 生效中', 'var(--ok)');
          }
          if (fs === 'win-off') {
            // 活动未生效：原倍率 + 提示准确生效时段（不再硬编码 23:00–8:00）。
            const lbl = (p && p.label) || m.badge || '优惠';
            const tail = win ? (promoFree(p) ? ' 免费' : (p && p.credit ? ' ' + esc(p.credit) : '')) : '';
            return esc(m.credits || '—') + (win ? note(esc(lbl) + tail + '（' + esc(win) + '）', 'var(--warn)') : '');
          }
          return m.credits ? esc(m.credits) : '—';
        })() + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag mute">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    }).join('');
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  try {
    const d = await api('logs');
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(logEntryHTML).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}

/* logEntryHTML 渲染一条日志（都是**单行**，v1.16.15 起）。
   - 对话行（后端已解析成 row）：时间 · 完整模型名 · 状态 · 指标，一行铺开；
     不用两行卡片——一屏 4~5 条的密度看日志得翻很久。行内那个与 TS 重复的
     时刻**不再展示**（「每行两个时间」困惑的根因）。
   - 任务/系统行：等宽文本，保留终端质感与级别着色。
   - row 缺失（老格式/解析失败）：退回原始文本，信息不丢。 */
function logEntryHTML(e) {
  // 时间只显示到秒：后端 TS 带纳秒，不显式指定时分秒会带毫秒尾巴。
  const t = e.ts
    ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
    : '';
  const chBadge = logCh === 'all'
    ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || esc(e.ch)) + '</i>'
    : '';

  if (e.ch === 'chat' && e.row) {
    const r = e.row;
    const ok = r.status >= 200 && r.status < 300;
    // 慢请求以 8s 为线（实测 TTFB 常见 7–8s，再慢就是上游异常），标黄不标红。
    const slow = ok && r.total_ms >= 8000;
    const cls = ok ? (slow ? ' slow' : '') : ' err';
    const statusCls = ok ? (slow ? 'warn' : 'ok') : 'bad';
    const kv = (label, val, extra) =>
      '<span class="kv' + (extra ? ' ' + extra : '') + '">' + label + '<b>' + esc(val) + '</b></span>';
    let metrics = kv('', r.mode === 'stream' ? '流式' : '同步', 'mute');
    metrics += kv('', 'HTTP ' + r.status, statusCls);
    if (r.ttfb_ms > 0) metrics += kv('首字 ', fmtMs(r.ttfb_ms), r.ttfb_ms >= 8000 ? 'warn' : '');
    if (r.tokens >= 0) {
      metrics += kv('产出 ', fmtTok(r.tokens) + ' tok');
      if (r.tok_s > 0) metrics += kv('', r.tok_s.toFixed(1) + ' tok/s');
    } else {
      metrics += '<span class="kv mute">无 usage</span>';
    }
    metrics += kv('耗时 ', (r.total_ms / 1000).toFixed(1) + 's', slow ? 'warn' : '');
    return '<span class="lg-chat' + cls + '">' + chBadge +
      '<span class="ts">' + esc(t) + '</span>' +
      '<span class="mdl" title="' + esc(r.model) + '">' + esc(r.model) + '</span>' +
      '<span class="bd">' + metrics + '</span>' +
      '<span class="seq">#' + r.seq + '</span>' +
      '</span>';
  }

  const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
  return '<span class="lg-line' + lvl + '">' + chBadge + esc(t + ' ' + e.text) + '</span>';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  model_sync_enabled: ['schedule', 'model_sync_enabled'], model_sync_time: ['schedule', 'model_sync_time'],
  probe_enabled: ['schedule', 'probe_enabled'], probe_hours: ['schedule', 'probe_hours'],
  docs_sheet_enabled: ['docs_sheet', 'enabled'], docs_sheet_file_id: ['docs_sheet', 'file_id'],
  docs_sheet_sheet_id: ['docs_sheet', 'sheet_id'], docs_sheet_token: ['docs_sheet', 'token'],
  max_body_mb: ['server', 'max_body_mb'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'], credit_floor: ['pool', 'credit_floor'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  notify_enabled: ['notify', 'enabled'],
  notify_cooldown: ['notify', 'cooldown'],
  notify_balance_low_credits: ['notify', 'balance_low_credits'],
};
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    // 通知渠道：结构化行编辑器回填（无渠道 = 空列表，不推送）。
    const ch = dig(cfgLoaded, ['notify', 'channels']);
    renderChannels(Array.isArray(ch) ? ch : []);
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const channels = collectChannels();
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const payload = collectConfig();
    put(payload, ['notify', 'channels'], channels);
    const r = await api('config', { method: 'POST', body: JSON.stringify(payload) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function addBrand() {
  return (document.querySelector('input[name="addBrand"]:checked') || {}).value || 'codebuddy';
}
function syncAddBrandUI() {
  const b = addBrand();
  const qoder = b === 'qoder', trae = b === 'trae';
  $('addRealmRow').hidden = qoder || trae;
  $('addHintCB').hidden = qoder || trae;
  $('addPatRow').hidden = !qoder;
  $('addTraeRow').hidden = !trae;
  $('btnStartLogin').hidden = qoder || trae;
  $('btnImportQoder').hidden = !qoder;
  $('btnImportTrae').hidden = !trae;
}
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到选域态：选域可见、加载/就绪/完成/错误全收，起始按钮亮起。
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  $('btnImportQoder').disabled = false; $('btnImportTrae').disabled = false;
  $('addPat').value = '';
  $('addTraeCookie').value = ''; $('addTraeDevice').value = ''; $('addTraeUid').value = '';
  syncAddBrandUI();
  stopPoll();
}
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function importQoder() {
  const pat = ($('addPat').value || '').trim();
  if (!pat) { $('addErr').hidden = false; $('addErr').textContent = '请粘贴 Qoder PAT'; return; }
  $('btnImportQoder').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('accounts/import', { method: 'POST', body: JSON.stringify({ brand: 'qoder', pat }) }).then(r => {
    $('addLoad').hidden = true;
    $('addDone').hidden = false;
    $('addDone').textContent = '已导入 Qoder ' + (r.nickname || r.uid) +
      (r.credits >= 0 ? ' · 额度 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') +
      '，账号已载入池中';
    setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnImportQoder').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function importTrae() {
  const cookie = ($('addTraeCookie').value || '').trim();
  const device_id = ($('addTraeDevice').value || '').trim();
  const uid = ($('addTraeUid').value || '').trim();
  if (!cookie) { $('addErr').hidden = false; $('addErr').textContent = '请粘贴 work.trae.cn 的 cookie'; return; }
  // device_id 留空即可：网关按 uid 自动派生（x-device-id 只作记账 key，实测不校验真伪）。
  $('btnImportTrae').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('accounts/import', { method: 'POST', body: JSON.stringify({ brand: 'trae', cookie, device_id, uid }) }).then(r => {
    $('addLoad').hidden = true;
    $('addDone').hidden = false;
    $('addDone').textContent = '已导入 Trae ' + (r.nickname || r.uid) + '，账号已载入池中';
    setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnImportTrae').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('brandCards').addEventListener('click', ev => {
  const b = ev.target.closest('[data-bf]');
  if (!b) return;
  // 再点当前选中卡片 = 取消筛选（回到全部）。
  brandFilter = (brandFilter === b.dataset.bf) ? 'all' : b.dataset.bf;
  if (brandFilter !== 'all') scrollToAccounts();
  if (overviewData) renderAccounts(overviewData.accounts || []);
});
// 总览条：点击回到「全部品牌」
$('poolSummary').addEventListener('click', ev => {
  if (!ev.target.closest('[data-bf]')) return;
  brandFilter = 'all';
  if (overviewData) renderAccounts(overviewData.accounts || []);
});
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnImportQoder').onclick = importQoder;
$('btnImportTrae').onclick = importTrae;
document.querySelectorAll('input[name="addBrand"]').forEach(el => el.addEventListener('change', syncAddBrandUI));
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => copyText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') pollQueueOnce();
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td><div class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
const SCHOOL_META = [
  ['share_invite', '分享'],
  ['desktop_chat_1_time', '桌面'],
  ['chat_3_times', '对话×3'],
  ['expert_use', '专家'],
  ['task_student_verify', '认证'],
];
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
// 上游错误常带整段 HTML（apisix 401 页），剥标签压空白再截断，全文留 title。
function errShort(s) {
  const t = String(s || '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();
  return t.length > 96 ? t.slice(0, 96) + '…' : t;
}
async function loadSchoolStatus(quiet) {
  const st = $('schoolState'), list = $('schoolList');
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('school/status');
    const arr = d.accounts || [];
    if (!arr.length) {
      st.hidden = false; st.className = 'state'; st.textContent = '暂无可用账号';
      list.innerHTML = ''; return;
    }
    // 活动期判定：任一账号 in_period=true 即活动仍在进行。全部 false（活动已
    // 结束）时不再铺任务格子——那些「未做」是无法补做的历史终态，铺开只会误导。
    const inPeriod = arr.some(v => v.in_period);
    $('schoolPeriodHint').textContent = inPeriod ? '09-13 ~ 09-24 · 每日重置' : '09-13 ~ 09-24 · 活动已结束';
    $('btnSchoolRunAll').style.display = inPeriod ? '' : 'none'; // 活动结束后闭环是纯空跑，藏掉入口
    let allDone = 0, active = 0; // active = 真正参与开学季的账号数（排除 global 域）
    const head = '<div class="shead"><div class="who">账号</div><div class="stasks">' +
      SCHOOL_META.map(([, name]) => '<span>' + esc(name) + '</span>').join('') +
      '</div><div class="luck">剩余抽奖</div></div>';
    // 无任务的品牌整段收成一行说明，不再渲染 5 列「—」空表。
    const compactSec = (g, note) =>
      '<div class="sbrand"><div class="sbrand-hd">' + brandBadge(g.brand) +
      '<span class="note">' + g.items.length + ' 个账号 · ' + esc(note) + '</span><span class="grow"></span></div></div>';
    list.innerHTML = groupByBrand(arr).map(g => {
      const isGlobalSec = g.brand === 'codebuddy-global';
      if (isGlobalSec) return compactSec(g, '国际版不参与开学季');
      if (g.items.every(v => v.unsupported)) return compactSec(g, '该品牌无开学季活动');
      if (!inPeriod) return compactSec(g, '活动已结束（历史任务状态不再展示）');
      const rows = g.items.map(v => {
        const by = {};
        (v.tasks || []).forEach(t => by[t.task_code] = t);
        const cells = SCHOOL_META.map(([code]) => {
          const t = by[code];
          const html = code === 'task_student_verify'
            ? '<span class="stask todo"><span class="mark">—</span>不做</span>'
            : staskHTML(t);
          return '<span title="' + esc(SCHOOL_TITLES[code] || code) + '">' + html + '</span>';
        }).join('');
        const done = SCHOOL_META.filter(([code]) => code !== 'task_student_verify' && by[code] && by[code].status === 'claimed').length;
        // global 域账号无开学季活动（后端 v.error 已注明），不计入「今日完成」分母。
        if (v.realm !== 'global') { active += 1; if (done === 4) allDone += 1; }
        return '<div class="srow">' +
          '<div class="who"><div class="nm" title="' + esc(v.nickname || '') + '">' + esc(v.nickname || '未命名') + '</div><div class="id">' + esc(v.uid) + '</div></div>' +
          '<div class="stasks">' + cells + '</div>' +
          '<div class="luck" title="剩余抽奖次数">' + LUCK_SVG + (v.chances == null ? '—' : v.chances) + '</div>' +
          (v.error ? '<div class="err" title="' + esc(String(v.error).replace(/<[^>]*>/g, ' ')) + '">' + esc(errShort(v.error)) + '</div>' : '') +
          '</div>';
      }).join('');
      return '<div class="sbrand"><div class="sbrand-hd">' + brandBadge(g.brand) +
        '<span class="note">' + g.items.length + ' 个账号' + '</span><span class="grow"></span></div>' +
        '<div class="sbody">' + head + rows + '</div></div>';
    }).join('');
    $('schoolSummary').textContent = !inPeriod
      ? '活动已结束'
      : (active > 0 && allDone === active)
        ? '今日全部完成 🎉'
        : allDone + '/' + active + ' 个账号今日全部完成';
    st.hidden = true;
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};
$('btnSchoolRefresh').onclick = () => loadSchoolStatus(false);
$('btnSchoolRunAll').onclick = async () => {
  if (!confirm('将对全部账号执行开学季闭环（分享/桌面/对话/专家 + 抽奖），约 1-2 分钟。确认继续？')) return;
  try {
    await api('school/run_all', { method: 'POST' });
    toast('开学季闭环已开始，结果看任务日志', 'ok');
    setTimeout(() => loadSchoolStatus(true), 15000);
  } catch (e) { toast(e.message, 'err'); }
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    for (const t of (a.school || [])) {
      if (t.task_code === 'task_student_verify') continue; // 需真实认证，永不出现在待办
      rows.push({ kind: 'school', code: t.task_code, prog: t.target_count ? t.progress + '/' + t.target_count : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  empty.style.display = 'none';
  let total = 0;
  // 按品牌再分段：外层品牌区，内层各账号（保留账号级 qgroup 不变）。
  // 用量/队列只带 uid，品牌靠 BRAND_OF_UID 反查——scan_all 与 live queue 共用此函数，一并分段。
  const withBrand = groups.map(g => ({ uid: g.uid, nick: g.nick, rows: g.rows, brand: brandOfUID(g.uid) }));
  list.innerHTML = groupByBrand(withBrand).map(s => {
    const inner = s.items.map(g => {
      total += g.rows.length;
      return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
        g.rows.map(qrowHTML).join('') + '</div>';
    }).join('');
    return '<div class="qbrand">' + brandBadge(s.brand) +
      (s.brand === 'codebuddy-global' ? '<span class="note">不参与开学季</span>' : '') +
      '<div class="qbrand-body">' + inner + '</div></div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
async function pollQueueOnce() {
  try {
    const q = await api('tasks/queue');
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（q.running 时也要同代次——刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    renderQueue(groupsFromQueue(q.items || []), q);
  } catch (e) { /* 静默 */ }
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    await pollQueueOnce();
    try {
      const q = await api('tasks/queue');
      if (!q.running) {
        clearInterval(queueTimer); queueTimer = null;
        toast('任务队列执行结束', 'ok');
        loadSchoolStatus(true);
      }
    } catch (e) { /* 忽略 */ }
  }, 3000);
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
/* fmtCredits 积分数量格式化：整数直接显示，小数保留一到两位。
   上游 credit 是小数（如 0.37/0.1），图表 y 轴与 tooltip 都用它，
   不能套 fmtTok（那个按 1000 进位，0.37 会显示成「0.37」但 1500 显示 1.5k，
   积分场景下 1.5k 积分反而不如 1500 直观）。 */
function fmtCredits(n) {
  n = Number(n || 0);
  if (!isFinite(n)) return '0';
  if (Number.isInteger(n)) return n.toLocaleString('en-US');
  const abs = Math.abs(n);
  if (abs >= 100) return n.toFixed(0);
  if (abs >= 10) return n.toFixed(1);
  if (abs >= 0.1) return n.toFixed(2);
  // 小值（每千 token 积分常见 0.003 这种）保留有效数字，否则一片 0.00 看不出差别
  return n.toFixed(4);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

/* realmLabel 把网关内部域键换成人话（用量页所有展示位统一走这里）。
   cn/global 是 codebuddy 的两套账号体系，qoder/trae 是品牌自己的池子；
   空串 = 老数据没记域，显示「未知」而不是留白。 */
function realmLabel(realm) {
  return ({ cn: '国内版', global: '国际版', qoder: 'Qoder', trae: 'Trae' })[realm] || (realm || '未知');
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

function usBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-wrapbar">' +
    '<span class="bar bar-p" style="width:' + (pp * 0.8).toFixed(1) + 'px" title="prompt"></span>' +
    '<span class="bar bar-c" style="width:' + Math.max(2, pc * 0.8).toFixed(1) + 'px" title="completion"></span>' +
    '</span>';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制是否追加延迟/速率两列——只有「按账号」表的表头带这两列；
   模型表与域表没有，多输出会造成列错位。早先靠「mid 是否为 undefined」隐式
   判断，调用方稍一改动就会错列，故改为显式参数。 */
function usRow(name, sub, a, mid, withPerf, lead) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + (lead || '') + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

function renderUsage(d) {
  const t = d.totals || {};
  $('usStats').innerHTML =
    usStat(fmtTok(t.requests), '请求数') +
    usStat(fmtTok(t.total_tokens), '总 token') +
    usStat(fmtCredits(t.credits), '积分消耗') +
    usStat(fmtTok(t.prompt_tokens), '输入 token') +
    usStat(fmtTok(t.completion_tokens), '输出 token') +
    usStat(t.errors ? String(t.errors) : '0', '失败尝试', t.errors ? 'warn' : '') +
    usStat(fmtMs(t.avg_latency_ms), '平均延迟');

  // 卡片与表格均按所选窗口统计（后端 Snapshot 按窗口聚合 Totals/By*），
  // 与时序图同口径；切换窗口后所有数字一起变。
  $('usNote').textContent = '卡片与表格按所选窗口统计 · ' +
    (d.buckets || 0) + ' 个分桶' +
    (d.file_bytes ? ' · ' + (d.file_bytes / 1024).toFixed(1) + ' KB' : '');

  // 按账号用量同样按品牌分段（用量数据带 uid+realm，品牌用 BRAND_OF_UID 反查；
  // 后端只给 codebuddy 品牌名，靠 realm 拆国内/国际版——与账号池同口径）。
  // 分隔行跨全部 10 列（与表头一致：mark/账号/域/请求/失败/输入/输出/合计/均延迟/均速率）。
  const accGroups = groupByBrand((d.by_account || []).map(x => ({ ...x, brand: displayBrand(x.brand || brandOfUID(x.key), x.realm) })));
  $('usAccBody').innerHTML = accGroups.map(s => {
    const sep = '<tr class="brand-sep"><td colspan="10">' + brandBadge(s.brand) +
      '<span class="brand-cnt">' + s.items.length + ' 个账号</span></td></tr>';
    const rows = s.items.map(x =>
      usRow(x.extra || x.key.slice(0, 8), x.key, x,
        '<td class="num">' + esc(realmLabel(x.realm)) + '</td>', true, brandBadge(x.brand))).join('');
    return sep + rows;
  }).join('') || '<tr><td colspan="10" class="empty">暂无数据</td></tr>';

  // 按模型：改用 by_model_account（按 账号×模型 拆分），每条记录标品牌+账号，
  // 解决同名模型跨品牌/账号混在一起（如 glm-5.2 在 codebuddy 与 qoder 都存在）。
  const maGroups = groupByBrand((d.by_model_account || []).map(x => ({ ...x, brand: displayBrand(x.brand || brandOfUID(x.key), x.realm) })));
  $('usModelBody').innerHTML = maGroups.map(s => {
    const sep = '<tr class="brand-sep"><td colspan="7">' + brandBadge(s.brand) +
      '<span class="brand-cnt">' + s.items.length + ' 条记录</span></td></tr>';
    const rows = s.items.map(x =>
      usRow(brandBadge(s.brand) + ' ' + (x.extra ? x.extra + ' · ' : '') + x.key, '', x, '', false, '')).join('');
    return sep + rows;
  }).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  // 按域表：realm 是内部键（cn/global/qoder/trae），展示层一律换成人话——
  // 用户不该为了看用量去记网关的内部域名字（v1.16.13）。
  $('usRealmBody').innerHTML = (d.by_realm || []).map(x =>
    usRow(realmLabel(x.key), '', x, '', false)).join('') || '<tr><td colspan="7" class="empty">暂无数据</td></tr>';

  // 积分消耗表（v1.16.9）：按品牌列出窗口内实测扣费。占比 = 该品牌 / 全部品牌之和；
  // 单价按**每百万 token**折算（实测 146M token 只花 0.7 积分，按千折算一律
  // 显示 0.0000，横向比不出贵贱）。跨模型混合均价，只用于品牌间比较。
  const brands = (d.by_brand || []).slice().sort((a, b) => (b.credits || 0) - (a.credits || 0));
  const credSum = brands.reduce((a, x) => a + (x.credits || 0), 0);
  $('usCreditBody').innerHTML = brands.length ? brands.map(x => {
    const c = Number(x.credits || 0);
    const pct = credSum > 0 ? (c / credSum * 100) : 0;
    const perM = x.total_tokens > 0 ? (c / x.total_tokens * 1e6) : null;
    return '<tr>' +
      '<td class="mark" aria-hidden="true"></td>' +
      '<td>' + brandBadge(displayBrand(x.key, x.realm)) + '</td>' +
      '<td class="num">' + fmtCredits(c) + '</td>' +
      '<td class="num" title="占窗口内全部品牌积分消耗的比例">' + (credSum > 0 ? pct.toFixed(1) + '%' : '—') + '</td>' +
      '<td class="num">' + fmtTok(x.requests) + '</td>' +
      '<td class="num">' + (perM === null ? '—' : fmtCredits(perM)) + '</td>' +
      '</tr>';
  }).join('') : '<tr><td colspan="6" class="empty">所选窗口内没有积分消耗记录（从下一次请求起开始统计）</td></tr>';

  renderUsageChart(d);
}

/* renderUsageChart 用 **ECharts** 画单条总 token 趋势线（smooth 曲线 + 数据点，
 * 鼠标悬停显示准确 token 数）。
 *
 * 为什么从手绘 SVG 换成 ECharts（v1.15）：
 *   - 悬浮提示/图例交互/缩放是现成的，不必自己维护 Catmull-Rom 双向 clamp 那套
 *     「防止尖峰卷成环」的补丁（v1.14.3 出过 bug）；
 *   - 时间轴刻度由 type:'time' 自己算好，30 天窗口和 24 小时窗口用同一套代码。
 *
 * 沿用 v1.14 定下的两条口径：
 *   ① x 轴**锚定所选窗口**（后端 window_from/window_to），不是「首个数据点」——
 *      否则数据只覆盖其中一段时会误以为「时间条和选的窗口对不上」；
 *   ② 只画窗口内的点：后端把超出小时窗口的历史桶折叠成**日点**（如 30 天窗口里
 *      的 9-23 日点），这些点落在窗口外，不剔除就会被画到窗口左缘。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。
   hour: "2026-09-16T13"  day: "2026-09-16" */
function parsePointTime(p) {
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* usChartInstance 承载 ECharts 实例。renderUsageChart 反复调用同一个 div，
   每次 setOption 前必须先 dispose，否则同一容器上叠多个实例 → 渲染错乱。 */
let usChartInstance = null;

/* usPointLabel 与 usAxisLabel 同义（ECharts time 轴的标签格式化器）。
   day 点 "10-12"，hour 点 "10-12 13:00"（窗口 < 3 天时带小时，避免歧义）。 */
function usPointLabel(ms, withHour) {
  const d = new Date(ms);
  const two = n => String(n).padStart(2, '0');
  const md = two(d.getMonth() + 1) + '-' + two(d.getDate());
  return withHour ? md + ' ' + two(d.getHours()) + ':00' : md;
}

/* usAxisLabel 与 usPointLabel 同义（ECharts time 轴的标签格式化器）。 */
function usAxisLabel(ms, withHour) { return usPointLabel(ms, withHour); }

/* usParsePoints 把一组后端点解析成 [{t, tt, req}]，时间解析不出来的直接丢
   （NaN 会传染整张图）。metric=token 时 tt 优先用上游给的 total，缺了用 pt+ct
   兜底；metric=credit 时 tt 取 credits（上游实测扣费，缺省 0）。 */
function usParsePoints(points, metric) {
  const out = [];
  const byCredit = metric === 'credit';
  for (const p of (points || [])) {
    const t = parsePointTime(p);
    if (t === null) continue;
    if (byCredit) {
      out.push({ t, tt: Number(p.credits || 0), req: p.requests || 0, scope: p.scope });
      continue;
    }
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    out.push({ t, tt: Number(p.total_tokens || 0) || (pt + ct), req: p.requests || 0, scope: p.scope });
  }
  return out;
}

/* usBuildGrid 把多个序列（点位数组）铺到统一的整点网格上（窗口锚定）。
   返回 { data: Map<系列名, [ts, tt, req][]>, t0, t1, withHour }。

   窗口内没有该系列桶的整点一律填 **0**（不是 null）：这样每个系列都是一条从
   窗口起点连到终点的连续线——模型没用过的小时就是贴着底部的 0 横线，一眼能
   对比出「谁在什么时候用、谁一直闲着」（null 断线在稀疏数据下会碎成一堆短线，
   看不出底部基准）。窗口外的 day 点不参与网格（与总序列同口径）。
   空数据系列（理论上不会出现，后端无桶就不返回该系列）画成整段 0 线也无妨。*/
function usBuildGrid(named, t0, t1) {
  const H = 3600 * 1000;
  const withHour = t1 - t0 < 3 * 24 * 3600 * 1000;
  const out = new Map();
  for (const s of named) {
    const byT = new Map(s.pts.map(p => [p.t, p]));
    const data = [];
    for (let t = t0; t <= t1; t += H) {
      const p = byT.get(t);
      data.push(p ? [t, p.tt, p.req] : [t, 0, 0]);
    }
    for (const p of s.pts) if (p.t % H !== 0) data.push([p.t, p.tt, p.req]);
    out.set(s.name, data);
  }
  return { data: out, t0, t1, withHour };
}

/* usPeakIdx 找一组数据里的峰值下标——只给峰值点画实心圆点，其余下标返回 0 不渲染。
   单线（全部消耗）与多线（按模型/按品牌/按 Key）共用：每个系列只有一个点。
   注意下标必须对完整数组（含 null）取：symbolSize 回调的 dataIndex 就是这个
   数组的下标，对过滤后的数组取会错位。全部为 0 时返回 -1（不画）。 */
function usPeakIdx(data) {
  let idx = -1, best = 0;
  for (let i = 0; i < data.length; i++) {
    if (!data[i]) continue;
    if (data[i][1] > best) { best = data[i][1]; idx = i; }
  }
  return idx;
}

/* US_PALETTE 分模型/分 key 多线的系列配色。深色底上同样可读（这是写死的 hex，
   不能取主题变量：canvas 不认 var()，且系列色本来就要与主题解耦）。
   12 色是刻意的上限：同一张图里人眼能区分的线色就这么多，再多必然撞色。
   后端也按 seriesTopN=10 截断命名系列 + 「其他」灰桶，所以颜色永远够用。
   「其他」单用灰色（US_OTHER_COLOR），弱化但不消失。 */
const US_PALETTE = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                    '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c',
                    '#3fb6c4', '#a58de0'];
const US_OTHER_COLOR = '#9aa3b5';
const US_OTHER_NAME = '其他';

/* usColorScheme 为本次渲染的系列分配颜色，两个诉求：
   1. **同帧不撞色**：同一批系列颜色互不相同（后端最多给 11 个系列 =
   seriesTopN 10 个命名 + 「其他」，12 色洗牌后逐个发，必然唯一）；
   2. **不跨次固化**：每次渲染洗牌重新发色——刻意**不做「模型名绑定颜色」**：
   模型目录随时更新，绑死会让某些颜色长期闲置，且"哪个模型该是什么色"没有
   业务含义。识别靠图例名字，颜色只要当帧准确即可。
   「其他」固定灰色，不参与洗牌（它是"杂项"，不该抢一个正式色）。 */
function usColorScheme(names) {
  const pool = US_PALETTE.slice();
  // Fisher-Yates 洗牌：每次渲染顺序不同
  for (let i = pool.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [pool[i], pool[j]] = [pool[j], pool[i]];
  }
  const out = {};
  for (const name of names) {
    if (name === US_OTHER_NAME) { out[name] = US_OTHER_COLOR; continue; }
    out[name] = pool.shift() || US_PALETTE[0];
  }
  return out;
}

/* usFigState 跨重渲染保留的图表状态：指标（token/积分）、模式（全部/按模型/按品牌/按 key）
   与图例开关。loadUsage 每次拉数据后重画；切换只重画不重新拉接口（数据已在 usLastUsage）。 */
let usLastUsage = null;
const usFigState = { metric: 'token', mode: 'total', selected: {} };

/* usModeValueSeries 按当前指标 + 模式取「待画的命名系列」。
   metric=token 用 total_tokens；metric=credit 用 credits（v1.16.9，上游实测扣费）。
   两者来自同一批桶、同一套时间片，只是取哪个字段。
   total: [{name:'总 token'|'总积分', pts: 总序列}]（单线）
   model: model_series 的每个模型一条线
   brand: brand_series 的每个品牌一条线（品牌由后端按 realm 派生，天然有界）
   key:   key_series 同理；空 key 桶（无归因）后端就不返回，天然不出现 */
function usModeValueSeries(snap) {
  const metric = usFigState.metric;
  const bagOf = { model: 'model_series', brand: 'brand_series', key: 'key_series' };
  if (bagOf[usFigState.mode]) {
    const bag = snap[bagOf[usFigState.mode]] || {};
    const list = [];
    for (const name of Object.keys(bag)) {
      list.push({ name, pts: usParsePoints(bag[name], metric) });
    }
    return list;
  }
  return [{ name: metric === 'credit' ? '总积分' : '总 token', pts: usParsePoints(snap.series || [], metric) }];
}

/* usFigSeries 把命名系列的网格数据转成 ECharts series 数组（每系列一色）。
   颜色用 usColorScheme 一次性分配：名字固定绑定 + 同帧不撞色。 */
function usFigSeries(named, grid, isMulti) {
  const scheme = usColorScheme(named.map(s => s.name));
  return named.map(s => {
    const color = scheme[s.name] || US_OTHER_COLOR;
    const data = grid.data.get(s.name) || [];
    const base = {
      name: s.name,
      type: 'line',
      smooth: true,
      data,
      connectNulls: false,
      symbol: 'circle',
      itemStyle: { color },
      lineStyle: { color, width: 2.2 },
      emphasis: { scale: 1.4 },
    };
    if (!isMulti) {
      // 单线（全部消耗）：峰值画点 + 淡面积，视觉重点在「何时冲高」。
      base.symbolSize = (val, pp) => pp.dataIndex === usPeakIdx(data) ? 7 : 0;
      base.areaStyle = { color, opacity: 0.08 };
    } else {
      // 多线：**每条线只画一个点——它自己的峰值点**（v1.16.19 用户诉求：
      // 「点太多了，只在峰值上显示一次，每个曲线只有一个峰值点」）。
      // 早期实现是按「非零点 ≤24 就给每个非零点画点」，稀疏积分曲线因此满是小点。
      // symbolSize 回调对非峰值返回 0，ECharts 就不渲染该点。
      // 注意：非零峰值不存在（整条 0 线）时返回 -1，全画 0 = 无点，符合预期。
      const peak = usPeakIdx(data);
      base.symbolSize = (val, pp) => pp.dataIndex === peak ? 6 : 0;
      base.symbolBorderWidth = 0;
      // 不裁剪到绘图区：峰值点落在窗口两端（最常见是「刚刚这一小时」）时，
      // 实心圆点会被 grid 边界切掉一半，看着像没有线（v1.16.11 实测）。
      base.clip = false;
    }
    return base;
  });
}


/* renderUsageChart 用 **ECharts** 画 Token 时序（v1.15.4 起支持三种模式）：
 *   - 全部消耗：单条总 token 曲线（smooth + 峰值点 + 淡面积）；
 *   - 按模型：每个调用过的模型一条**独立颜色**的线，图例列在图表下方（点图例
 *     可开关单个模型）；窗口内没调用的模型不画（null 断线，不画 0 线）；
 *   - 按 Key：同口径，维度换成 API Key 名。
 * 图例用 ECharts 内置 legend（交互现成），不是自定义 HTML。
 *
 * 沿用 v1.14 定下的两条口径：
 *   ① x 轴**锚定所选窗口**（后端 window_from/window_to），不是「首个数据点」——
 *      否则数据只覆盖其中一段时会误以为「时间条和选的窗口对不上」；
 *   ② 只画窗口内的点：后端把超出小时窗口的历史桶折叠成**日点**（如 30 天窗口里
 *      的 9-23 日点），这些点落在窗口外，不剔除就会被画到窗口左缘。
 */
function renderUsageChart(snap) {
  const host = $('usChart');
  usLastUsage = snap;

  const named = usModeValueSeries(snap);
  const metric = usFigState.metric;
  const isCredit = metric === 'credit';
  let anyPts = false;
  for (const s of named) if (s.pts.length) { anyPts = true; break; }
  if (!anyPts) {
    disposeUsChart();
    host.innerHTML = '<div class="us-empty">' +
      (isCredit ? '所选窗口内没有积分消耗记录（老数据无扣费明细，新请求起才有）。' : '暂无用量数据。发起一次对话后再刷新。') +
      '</div>';
    if ($('usFigNote')) $('usFigNote').textContent = '';
    return;
  }

  // 时间范围：优先用后端给出的所选窗口（左闭右闭，小时边界）；缺失时退回数据范围。
  // day 点按当天 00:00 参与定位，与 hour 点在同一条连续轴上——日桶本来就是他那天
  // 所有小时的聚合。
  const winFrom = snap && snap.window_from ? parsePointTime({ t: snap.window_from }) : null;
  const winTo   = snap && snap.window_to   ? parsePointTime({ t: snap.window_to })   : null;
  const hasWin  = winFrom !== null && winTo !== null && winTo > winFrom;
  // 数据范围（全部系列的并集），窗口缺失时兜底。
  const bounds = [];
  for (const s of named) for (const p of s.pts) bounds.push(p.t);
  bounds.sort((a, b) => a - b);
  let t0 = hasWin ? winFrom : bounds[0];
  let t1 = hasWin ? winTo   : bounds[bounds.length - 1];
  if (t1 <= t0) t1 = t0 + 3600 * 1000; // 单点窗口：至少 1 小时宽

  const isMulti = usFigState.mode !== 'total';
  const grid = usBuildGrid(named, t0, t1);
  const series = usFigSeries(named, grid, isMulti);

  // dataZoom 对时间轴不适用时（窗口内只有一两个点），ECharts 会画出 0 宽容器 →
  // 高度为 0 的 canvas 什么都不显示。给容器一个兜底高度即可，不在这里处理。
  disposeUsChart();
  host.innerHTML = ''; // 清掉"暂无用量数据"占位
  // 颜色一律从主题 CSS 变量取（不要用 data-theme 猜明暗：猜错就会出现
  // v1.15.4 那种「浅色底上深底深字」的空 tooltip）。变量取不到时用中性兜底。
  const INK = usThemeVar('--ink-3', '#a0a4b0');      // 轴标签/图例灰
  const GRID = usThemeVar('--line-soft', '#ececf1'); // 网格线
  const TIPBG = usThemeVar('--surface', '#ffffff');
  const TIPFG = usThemeVar('--ink', '#333333');
  const ACC = usAccentHex();

  const hostW = Math.max(320, host.clientWidth - 34); // 减左右 padding，估图例换行用

  const two = n => String(n).padStart(2, '0');
  const whenOf = ms => { const d = new Date(ms); return two(d.getMonth() + 1) + '-' + two(d.getDate()) + ' ' + two(d.getHours()) + ':00'; };

  usChartInstance = echarts.init(host);
  usChartInstance.setOption({
    // 图例放底部、**plain 自动换行**（scroll 模式会把名字藏进 ◀ 1/2 ▶ 翻页箭头，
    // 截图里就是这个问题）；grid 的 bottom 按估算的图例行数动态留位。
    // 绘图区顶部不再为 tooltip 预留大片空白：tooltip 现在是贴顶的紧凑浮层，
    // 多系列模式下曲线基本贴底，空出的顶部它可以直接盖住（v1.15.8 前留 108px
    // 导致图上方一大片空白）。单/多模式一致 top:24。
    // right 留 26px：时间轴右端就是「当前这一小时」，最后一个数据点若画在右边界
    // 上会被纵向裁掉一半（v1.16.11 前 right:20，实测点被切）。左端同理由 left:56 覆盖。
    grid: { left: 56, right: 26, top: 24, bottom: isMulti ? usLegendEstimate(named.map(s => s.name), hostW) : 32 },
    legend: {
      show: isMulti, bottom: 0, type: 'plain',
      icon: 'circle', itemWidth: 8, itemHeight: 8, itemGap: 14,
      textStyle: { color: INK, fontSize: 11.5 },
      inactiveColor: usThemeVar('--ink-4', '#c9ccd6'),
      selected: usFigState.selected,
      formatter: name => name === US_OTHER_NAME ? '其他（未逐个列出）' : name,
    },
    tooltip: {
      // 统一 axis 触发：悬停到时间列上就出提示。item 触发要点中线条/符号，
      // 符号默认不可见时几乎测不到（v1.15.4 用户反馈「鼠标放在线上没内容」）。
      trigger: 'axis',
      backgroundColor: TIPBG,
      borderColor: 'transparent',
      padding: [8, 12],
      // 不设 max-width：**模型名/key 名必须完整显示**（用户明确要求，v1.15.6 的
      // 330px 上限 + 名字截断不合格）。行内布局见 formatter（flex 两端对齐）。
      extraCssText: 'border-radius:10px;box-shadow:0 4px 14px rgba(60,70,120,.16);',
      textStyle: { color: TIPFG, fontSize: 12 },
      // 固定在图表顶部（不是跟着鼠标跑）：鼠标贴边时 tooltip 不再被容器/窗口裁掉。
      // 宽度自适应用 dom.style.maxWidth 直接设在内层内容元素上——**不要用回调里
      // 的 offsetWidth/contentSize 做夹紧计算**：那个值不可靠（实测 2 行内容报
      // 1094px，是外层整宽），会把 tooltip 挤到最左。
      position: (point, params, dom) => {
        if (dom && dom.style) {
          const hostW = host.clientWidth || 900;
          // fit-content：盒子按内容自适应（不因 max-width 撑成固定宽），名字过长
          // 才换行；上限跟随容器宽度。
          dom.style.width = 'fit-content';
          dom.style.maxWidth = Math.min(560, Math.max(360, hostW - 60)) + 'px';
        }
        const W = dom && dom.offsetWidth ? dom.offsetWidth : 240;
        const w = (host.clientWidth || 900) - 16;
        let x = point[0] - Math.round(W / 2);
        if (!isFinite(x) || W > w) x = 8;
        x = Math.max(8, Math.min(x, Math.max(8, w - W)));
        return [x, 6];
      },
      // 竖线上标记当前小时（多系列靠它对齐看）
      axisPointer: { type: 'line', lineStyle: { color: INK, width: 1, type: 'dashed' } },
      formatter: p => {
        // axis 触发：formatter 收到 [param, ...]（各系列在该时间列上的点）。
        const arr = Array.isArray(p) ? p : [p];
        let v = null;
        for (const it of arr) { if (it && it.value) { v = it.value; break; } }
        if (!v) return '';
        // 行内 flex 两端对齐：名字完整不截断 + 数值右对齐，比空格对齐更整齐。
        // （名字过长时整行换行，tooltip 内层已被 position 回调设了 max-width。）
        const row = (name, val, valStyle) =>
          '<div style="display:flex;justify-content:space-between;gap:18px">' +
          '<span>' + name + '</span><b' + (valStyle ? ' style="' + valStyle + '"' : '') + '>' + val + '</b></div>';
        if (!isMulti) {
          // value = [时间戳, 数值, 请求数]
          const label = isCredit ? '总积分' : '总 token';
          const val = isCredit ? fmtCredits(v[1]) : fmtTok(v[1]);
          return esc(whenOf(v[0])) + row(label, val + (v[2] ? '（' + v[2] + ' 次）' : ''));
        }
        // 多系列：只列当前时刻**有值**的系列（0 值不列，否则每条竖线都刷 11 行），
        // 按数值降序；最多 6 行（再多浮层过高），其余折叠成提示。
        const rows = [];
        for (const it of arr) {
          const val = it && it.value;
          if (!val) continue;
          const tt = val[1] || 0;
          if (tt > 0) rows.push({ name: it.seriesName, tt });
        }
        let html = esc(whenOf(v[0]));
        rows.sort((a, b) => b.tt - a.tt);
        const shown = rows.slice(0, 6);
        for (const r of shown) {
          html += row(esc(r.name), isCredit ? fmtCredits(r.tt) : fmtTok(r.tt));
        }
        if (rows.length > shown.length) {
          html += '<div style="opacity:.65">…另有 ' + (rows.length - shown.length) + ' 项（该时刻共 ' + rows.length + ' 项有数据）</div>';
        }
        if (!rows.length) html += '<div style="opacity:.65">该时刻无' + (isCredit ? '积分消耗' : '用量') + '</div>';
        return html;
      }
    },
    xAxis: {
      type: 'time',
      min: t0, max: t1,                      // 锚定窗口，两端不出界
      axisLabel: {
        formatter: v => usPointLabel(v, grid.withHour),
        color: INK, fontSize: 11.5
      },
      axisLine: { show: false },
      axisTick: { show: false },
      splitLine: { show: false }
    },
    yAxis: {
      type: 'value',
      axisLabel: {
        // 积分用 fmtCredits（带小数，量级小），token 用 fmtTok（k/M 缩写）
        formatter: isCredit ? fmtCredits : fmtTok,
        color: INK, fontSize: 11.5
      },
      splitLine: { lineStyle: { color: GRID } },
      axisLine: { show: false },
      axisTick: { show: false }
    },
    series: series,
  });

  // 图例占的行数依赖实际换行（plain 自动折行），setOption 后用**渲染后的真实
  // 高度**把 grid 的 bottom 校正一次——估算偏小的话图例会压到 x 轴刻度上。
  // 优先取 legend 视图的 zrender 包围盒；拿不到时退回估算值。
  if (isMulti) {
    let lgH = 0;
    try {
      const lg = usChartInstance.getModel().getComponent('legend');
      const gv = lg && usChartInstance.getViewOfComponentModel(lg);
      const rect = gv && gv.group && gv.group.getBoundingRect && gv.group.getBoundingRect();
      if (rect && rect.height > 0) lgH = rect.height;
    } catch (e) { lgH = 0; }
    if (!lgH) lgH = usLegendEstimate(named.map(s => s.name), hostW);
    const gb = Math.ceil(lgH) + 14;
    usChartInstance.setOption({ grid: { bottom: gb } });
    // 容器高度随图例行数增长（CSS 里的 260px 是单线模式的基准）
    const rows = Math.max(1, Math.round(lgH / 20));
    host.style.height = Math.max(300, 260 + Math.max(0, rows - 1) * 20) + 'px';
    usChartInstance.resize();
  } else {
    host.style.height = '';
  }

  // 图例开关记住（切模式 / 刷新后保持）。selected 以 ECharts 的实时状态为准，
  // 而不是我们自己维护一份——用户点过快照里没有的系列时不丢状态。
  usChartInstance.on('legendselectchanged', ev => {
    if (usFigState.mode === 'total') return;
    usFigState.selected = ev.selected || {};
  });

  // 单线模式的图例色被 canvas 接管，header 不再写死色块。
  const cap = { total: '全部消耗', model: '按模型', brand: '按品牌', key: '按 Key' }[usFigState.mode] || '';
  if ($('usChartLabel')) {
    $('usChartLabel').textContent = (isCredit ? '积分消耗时序 · ' : 'Token 时序 · ') + cap;
  }
  if ($('usFigNote')) {
    let note = '';
    if (isCredit) {
      note = isMulti
        ? '共 ' + named.length + ' 条系列（按品牌的实测扣费积分，点击图例可开关）；每条线只标出它自己的峰值点。'
        : '曲线为该时段实测扣费积分之和（上游 usage.credit），点即峰值。';
    } else if (isMulti) {
      const other = named.find(s => s.name === US_OTHER_NAME);
      note = '共 ' + named.length + ' 条系列（每系列一色，点击图例可开关）' +
             (other ? '；小量维度已并入「其他」' : '') +
             '；每条线只标出它自己的峰值点。';
    }
    $('usFigNote').textContent = note;
  }
}

/* usAccentHex 把主题变量 --accent 解析成实色 hex。
   canvas 画不了 CSS 变量（ECharts 的 canvas 渲染器不认识 var()，会当透明），
   所以线条/面积色要现取。取不到时退回 ECharts 默认蓝。 */
function usAccentHex() {
  const v = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim();
  if (/^#[0-9a-fA-F]{6}$/.test(v)) return v;
  if (/^#[0-9a-fA-F]{3}$/.test(v)) return '#' + v.slice(1).split('').map(c => c + c).join('');
  return '#4f8cff';
}

/* usThemeVar 读主题 CSS 变量（--surface / --ink / --line 等）为实色。
   **不要用 documentElement.dataset.theme 判明暗**：那套判定与 :root/媒体查询
   实际生效的颜色可能错位（v1.15.4 浅色主题下 tooltip 被染色成深底深字，
   出现「有框没字」——空的观感就是配色取错了源）。直接取变量，永远和页面一致。
   取不到时给一个中性浅色兜底（tooltip 白底黑字在任何主题下可读）。 */
function usThemeVar(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  if (/^#[0-9a-fA-F]{6}$/.test(v)) return v;
  if (/^#[0-9a-fA-F]{3}$/.test(v)) return '#' + v.slice(1).split('').map(c => c + c).join('');
  const m = v.match(/^rgba?\(([^)]+)\)$/);
  return m ? v : fallback;
}

/* usLegendEstimate 估算底部图例高度（像素）。
   legend 用 plain（自动换行），ECharts 不会回调告诉我们换了几行，而 grid 的
   bottom 要在 setOption 前定好——估算每项宽度（名字宽的按 CJK 字宽算）与行数，
   给绘图区留够位置，避免图例压到 x 轴。窗口变化时 renderUsageChart 会重算。 */
function usLegendEstimate(names, hostW) {
  const fs = 11.5;
  const rows = [];
  let used = 0;
  for (const nm of names) {
    const w = Math.max(30, usLabelWidth(nm === US_OTHER_NAME ? '其他（未逐个列出）' : nm, fs) + 30);
    if (used && used + w > hostW) { rows.push(used); used = 0; }
    used += w;
  }
  if (used) rows.push(used);
  return Math.max(1, rows.length) * 20 + 12;
}

/* usLabelWidth 估算一段文本的渲染宽度（px）：CJK/全角按字号计，其余按 0.55 字号。
   只是给图例排版用，不要求精确——多估一点比少估好（少估会压住 x 轴）。 */
function usLabelWidth(s, fs) {
  let w = 0;
  for (const ch of String(s)) {
    w += ch.charCodeAt(0) > 0x2e80 ? fs : fs * 0.55;
  }
  return w;
}


/* disposeUsChart 释放当前实例。必须在重渲染、页面隐藏（切换 tab）、
   以及"暂无数据"占位前调用，否则同一 div 上会叠加多个 canvas。 */
function disposeUsChart() {
  if (usChartInstance) {
    usChartInstance.dispose();
    usChartInstance = null;
  }
}

/* 窗口尺寸变化时重算 canvas 大小（ECharts 默认只认 dom 尺寸变化事件，
   侧栏折叠/窗口拖拽都要这个，否则曲线被裁掉一半）。 */
let usResizeT = null;
addEventListener('resize', () => {
  if (!usChartInstance) return;
  clearTimeout(usResizeT);
  usResizeT = setTimeout(() => usChartInstance && usChartInstance.resize(), 120);
});

async function loadUsage() {
  const hours = ($('usWindow') && $('usWindow').value) || 72;
  try {
    const d = await api('usage?hours=' + encodeURIComponent(hours));
    renderUsage(d);
  } catch (e) {
    disposeUsChart();
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
if ($('usWindow')) $('usWindow').onchange = loadUsage;

/* 图表模式切换（全部消耗 / 按模型 / 按品牌 / 按 Key）：只重画、不重新拉接口——
   数据已在 usLastUsage 里，切换即时生效。选中态类是 .seg（v1.16.18 起图表头
   两级筛选不再用 .chip，见 index.html .uschart-hd 段）。 */
if ($('usModes')) $('usModes').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-mode]');
  if (!b) return;
  usFigState.mode = b.dataset.mode;
  $('usModes').querySelectorAll('.seg').forEach(c => c.classList.toggle('on', c === b));
  if (usLastUsage) renderUsageChart(usLastUsage);
});

/* 指标切换（Token / 积分消耗）：同一份快照换 y 值字段重画，不重新拉接口。
   切到积分时把模式收窄到「按品牌」——用户的需求就是「积分按品牌看」；
   token 模式保留原来的全部/模型/品牌/key 四种。 */
if ($('usMetric')) $('usMetric').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-metric]');
  if (!b) return;
  const metric = b.dataset.metric;
  if (usFigState.metric === metric) return;
  usFigState.metric = metric;
  $('usMetric').querySelectorAll('.seg').forEach(c => c.classList.toggle('on', c === b));

  // 积分指标只保留「全部消耗」与「按品牌」两个模式：按模型/按 Key 的积分
  // 明细同样有数据，但用户这里的诉求是品牌维度，先把噪声收掉。
  const modes = $('usModes');
  if (modes) {
    const allow = metric === 'credit' ? ['total', 'brand'] : ['total', 'model', 'brand', 'key'];
    modes.querySelectorAll('button[data-mode]').forEach(btn => {
      btn.hidden = !allow.includes(btn.dataset.mode);
    });
    // 切到积分时默认按品牌（用户诉求就是「积分按品牌看」）；切回 token 保留原模式。
    if (metric === 'credit') usFigState.mode = 'brand';
    else if (!allow.includes(usFigState.mode)) usFigState.mode = 'total';
    modes.querySelectorAll('button[data-mode]').forEach(btn =>
      btn.classList.toggle('on', btn.dataset.mode === usFigState.mode));
  }
  if (usLastUsage) renderUsageChart(usLastUsage);
});

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

// 积分构成品牌筛选（与账号池/健康检验同一交互；配色基于全量名单，筛选不变色）。
let pkBrandFilter = 'all';
let pkData = null; // 最近一次 /packages 响应，筛选切换时本地重渲染不再请求上游

function renderPackages(d) {
  const list = (d.accounts || []);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    if ($('pkFilterBtns')) $('pkFilterBtns').innerHTML = '';
    if ($('pkNote')) $('pkNote').textContent = '—';
    pkBrandFilter = 'all';
    return;
  }
  // 品牌筛选按钮组（按当前实际存在的品牌渲染）
  const fb = $('pkFilterBtns');
  if (fb) {
    const present = [...new Set(list.map(brandOf))];
    if (!present.includes(pkBrandFilter)) pkBrandFilter = 'all';
    const opts = ['all'].concat(BRAND_ORDER.filter(b => present.includes(b)));
    fb.innerHTML = opts.map(b =>
      '<button class="xs' + (pkBrandFilter === b ? ' primary' : '') + '" data-pkf="' + b + '">' +
      (b === 'all' ? '全部' : (BRAND_LABEL[b] || b)) + '</button>').join(' ');
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  // 品牌筛选后的可见账号（配色/maxRemain 仍按全量算，筛选不改变色号和占比口径）
  const flist = pkBrandFilter === 'all' ? list : list.filter(s => brandOf(s) === pkBrandFilter);

  // 按品牌分组：每个品牌一张紧凑表（一个账号一行），号多时纵向占用小、可扫读。
  // 逐包明细在下方详表区，这里只放汇总：余额 / 构成条 / 图例 chips。
  const pkSummaryGroups = groupByBrand(flist).map(g => {
    const rows = g.items.map(a => {
      if (a.error) {
        return '<tr><td class="pk-who"><span class="nm">' +
          esc((a.nickname || a.uid.slice(0, 8))) + '</span> ' +
          '<span class="realm">' + esc(a.realm || '') + '</span></td>' +
          '<td colspan="2"><span class="err">查询失败：' + esc(a.error) + '</span></td></tr>';
      }
      const srcs = pkBySource(a.packages || []);
      const total = Math.max(1, Number(a.size || 0));
      const bar = srcs.map(s =>
        '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
        colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
      ).join('');
      const legend = srcs.map(s =>
        '<span><i style="background:' + colorOf(s.key) + '"></i>' +
        '<span class="lg-name">' + esc(s.name.replace(/^CodeBuddy/, '')) + '</span>' +
        '<span class="lg-val">x' + s.n + ' · ' + fmtTok(s.size) +
        (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span></span>'
      ).join('');
      return '<tr>' +
        '<td class="pk-who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span> ' +
        '<span class="realm">' + esc(a.realm || '') + '</span></td>' +
        '<td class="num pk-bal"><div class="big">' + fmtTok(a.remain) + '</div>' +
        '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
        ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div></td>' +
        '<td class="pk-mix"><div class="mixbar">' + bar + '</div>' +
        '<div class="pk-legend">' + legend + '</div></td>' +
        '</tr>';
    }).join('');
    return '<div class="pk-brand"><div class="pk-brand-hd">' + brandBadge(g.brand) +
      '<span class="note">' + g.items.length + ' 个账号</span><span class="grow"></span></div>' +
      '<div class="tbl-wrap"><table class="acc pk-tbl"><thead><tr>' +
      '<th>账号</th><th class="num">余额</th><th>构成（按面额降序）</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div></div>';
  }).join('');
  $('pkSummary').innerHTML = pkSummaryGroups;

  $('pkNote').textContent = flist.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，包的**面额**列是重点；同样按品牌归并。
  const pkDetailGroups = groupByBrand(flist).map(g => {
    const boxes = g.items.map(a => {
      if (a.error) return '';
      const packs = (a.packages || []);
      const rows = packs.map(p => {
        const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
        const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                    (p.package_code || '').replace(/^TCACA_/, '');
        return '<tr><td class="mark" aria-hidden="true"><i style="background:' +
          colorOf(k) + '"></i></td>' +
        '<td>' + esc(p.name || '(未命名)') +
          (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
        '<td class="num">' + fmtTok(p.size) + '</td>' +
        '<td class="num">' + fmtTok(p.remain) + '</td>' +
        '<td class="num">' + fmtTok(p.used) + '</td>' +
        '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
        '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
        '</tr>';
      }).join('');
      return '<div class="box"><header><h3>' +
        esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
        '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
        ' / 总额 ' + fmtTok(a.size) + ' · ' + packs.length + ' 个包（按面额降序）</span>' +
        '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
        '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
        '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
        '<th class="num">发放</th><th class="num">到期</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div></div>';
    }).join('');
    return '<div class="pk-detail-brand"><div class="pk-brand-hd">' + brandBadge(g.brand) +
      '<span class="note">' + g.items.length + ' 个账号</span></div>' + boxes + '</div>';
  }).join('');
  $('pkDetail').innerHTML = pkDetailGroups;
}

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  try {
    const d = await api('packages');
    pkData = d;
    renderPackages(d);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnPk')) $('btnPk').onclick = loadPackages;
// 品牌筛选：用缓存的响应本地重渲染，不再打上游
if ($('pkFilterBtns')) $('pkFilterBtns').addEventListener('click', ev => {
  const b = ev.target.closest('[data-pkf]');
  if (!b) return;
  pkBrandFilter = b.dataset.pkf;
  if (pkData) renderPackages(pkData);
});


/* ── 通知渠道行编辑器（v1.11）──────────────────────────────────────── */
/* 每个渠道一行：类型下拉 + URL + 附加参数 + 删除。占位提示随类型联动，
   底层仍是 config.notify.channels JSON（行收集为对象数组后整体提交）。 */
const CHANNEL_TYPES = ['telegram', 'bark', 'wecom', 'dingtalk', 'generic', 'smtp'];
const CHANNEL_URL_HINT = {
  telegram: 'https://api.telegram.org/bot<TOKEN>/sendMessage',
  bark: 'https://api.day.app/<device_key>',
  wecom: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…',
  dingtalk: 'https://oapi.dingtalk.com/robot/send?access_token=…',
  generic: '接收 {"event","title","body"} JSON POST 的端点',
};
const CHANNEL_SECRET_HINT = {
  telegram: 'chat_id', dingtalk: '加签密钥', bark: '', wecom: '', generic: '',
};
function renderChannels(channels) {
  const box = $('notifyChannels');
  box.innerHTML = '';
  for (const ch of channels) addChannelRow(ch);
}
function addChannelRow(ch) {
  ch = ch || {};
  const box = $('notifyChannels');
  const row = document.createElement('div');
  row.style.cssText = 'display:flex;gap:6px;align-items:center;margin:4px 0;flex-wrap:wrap';
  const sel = document.createElement('select');
  sel.className = 'ch-type';
  for (const t of CHANNEL_TYPES) {
    const opt = document.createElement('option');
    opt.value = t; opt.textContent = t === 'smtp' ? 'smtp 邮箱' : t;
    sel.appendChild(opt);
  }
  sel.value = CHANNEL_TYPES.includes(ch.type) ? ch.type : 'telegram';
  const mk = (cls, ph, val, css, type) => {
    const el = document.createElement('input');
    el.className = cls; el.placeholder = ph; el.value = val || '';
    el.style.cssText = css || 'flex:1;min-width:150px';
    if (type) el.type = type;
    return el;
  };
  // 通用 webhook 系字段
  const url = mk('ch-url', CHANNEL_URL_HINT[sel.value] || 'URL', ch.url, 'flex:1;min-width:240px');
  const secret = mk('ch-secret', CHANNEL_SECRET_HINT[sel.value] || '', ch.secret, 'width:170px');
  secret.title = '附加参数（占位符说明了该类型要填什么）';
  const secretSaved = ch.secret !== undefined; // 回显已脱敏为 ""，键存在 = 磁盘有值
  // smtp 邮箱渠道字段
  const sHost = mk('ch-smtp-host', 'SMTP 服务器，如 smtp.qq.com', ch.smtp_host, 'width:200px');
  const sPort = mk('ch-smtp-port', '端口 465', ch.smtp_port || '', 'width:80px', 'number');
  const sUser = mk('ch-smtp-user', '发信账号（邮箱）', ch.smtp_username, 'width:190px');
  const sPass = mk('ch-smtp-pass', '授权码 / 客户端专用密码', ch.smtp_password, 'width:180px', 'password');
  sPass.autocomplete = 'new-password';
  const passSaved = ch.smtp_password !== undefined;
  const sTo = mk('ch-smtp-to', '收件人，多个用逗号分隔', (ch.smtp_to || []).join(', '), 'flex:1;min-width:200px');
  const sFrom = mk('ch-smtp-from', '发件人（留空 = 账号）', ch.smtp_from, 'width:190px');
  const smtpFields = [sHost, sPort, sUser, sPass, sTo, sFrom];
  const del = document.createElement('button');
  del.type = 'button'; del.className = 'xs'; del.textContent = '删除';
  del.onclick = () => row.remove();
  const sync = () => {
    const isSmtp = sel.value === 'smtp';
    url.style.display = isSmtp ? 'none' : '';
    secret.style.display = isSmtp ? 'none' : '';
    smtpFields.forEach(el => el.style.display = isSmtp ? '' : 'none');
    url.placeholder = CHANNEL_URL_HINT[sel.value] || 'URL';
    secret.placeholder = secretSaved ? '已保存，留空保持不变' : (CHANNEL_SECRET_HINT[sel.value] || '');
    sPass.placeholder = passSaved ? '已保存，留空保持不变' : '授权码 / 客户端专用密码';
  };
  sel.onchange = sync;
  row.append(sel, url, secret, ...smtpFields, del);
  sync();
  box.appendChild(row);
}
function collectChannels() {
  const out = [];
  document.querySelectorAll('#notifyChannels .ch-type').forEach(sel => {
    const row = sel.closest('div');
    if (sel.value === 'smtp') {
      const host = row.querySelector('.ch-smtp-host').value.trim();
      const toRaw = row.querySelector('.ch-smtp-to').value.trim();
      if (!host || !toRaw) return; // 缺服务器或收件人的行丢弃
      const ch = {
        type: 'smtp',
        smtp_host: host,
        smtp_to: toRaw.split(/[,，;；\s]+/).filter(Boolean),
        smtp_username: row.querySelector('.ch-smtp-user').value.trim(),
      };
      const pass = row.querySelector('.ch-smtp-pass').value.trim();
      if (pass) ch.smtp_password = pass; // 空 = 保留磁盘原值（回显已脱敏）
      const port = parseInt(row.querySelector('.ch-smtp-port').value.trim(), 10);
      if (port > 0) ch.smtp_port = port;
      const from = row.querySelector('.ch-smtp-from').value.trim();
      if (from) ch.smtp_from = from;
      out.push(ch);
      return;
    }
    const url = row.querySelector('.ch-url').value.trim();
    if (!url) return; // 空行丢弃（点了添加没填）
    const secret = row.querySelector('.ch-secret').value.trim();
    const ch = { type: sel.value, url };
    if (secret) ch.secret = secret;
    out.push(ch);
  });
  return out;
}
$('btnAddChannel').onclick = () => addChannelRow({ type: 'telegram' });

/* ── API Keys 管理（多租户，v1.11）──────────────────────────────────── */
async function loadKeys() {
  try {
    const d = await api('keys');
    const tb = $('keysBody');
    tb.innerHTML = '';
    for (const k of (d.keys || [])) {
      const tr = document.createElement('tr');
      const models = (k.models || []).join(', ') || '不限';
      tr.innerHTML = '<td>' + esc(k.name) + '</td>' +
        '<td class="num">' + esc(k.key) + '</td>' +
        '<td>' + esc(models) + '</td>' +
        '<td class="num">' + (k.rpm ? k.rpm + '/min' : '不限') + '</td>' +
        '<td>' + (k.free_guard ? '<span class="tag ok" title="免费额度尽即报错 402，不静默花积分">守护</span>' : '<span class="note">—</span>') + '</td>' +
        '<td>' + (k.disabled ? '<span class="tag warn">已禁用</span>' : '<span class="tag ok">启用</span>') + '</td>' +
        '<td><button type="button" class="xs" data-key-edit="' + esc(k.name) + '">编辑</button></td>';
      tb.appendChild(tr);
      tr.querySelector('[data-key-edit]').onclick = () => {
        $('keyName').value = k.name;
        $('keySecret').value = '';           // 掩码态，留空 = 保留原值
        $('keyModels').value = (k.models || []).join(', ');
        $('keyRPM').value = k.rpm || 0;
        $('keyNote').value = k.note || '';
        $('keyFreeGuard').checked = !!k.free_guard;
        $('keyName').dataset.editing = '1';
        toast('编辑 ' + k.name + '：密钥留空表示保留原值', 'ok');
      };
    }
    if (!(d.keys || []).length) {
      tb.innerHTML = '<tr><td colspan="7" class="note">暂无 key（default 由 config.api_key 合成）</td></tr>';
    }
  } catch (e) { toast('读取 API Keys 失败：' + e.message, 'err'); }
}

$('btnKeySave').onclick = async () => {
  const name = $('keyName').value.trim();
  if (!name) { toast('名称不能为空', 'err'); return; }
  const models = $('keyModels').value.trim();
  const body = {
    name,
    key: $('keySecret').value.trim(),
    models: models ? models.split(/[,，\s]+/).filter(Boolean) : [],
    rpm: Number($('keyRPM').value || 0),
    note: $('keyNote').value.trim(),
    free_guard: $('keyFreeGuard').checked,
  };
  try {
    const r = await api('keys/upsert', { method: 'POST', body: JSON.stringify(body) });
    if (r.generated_key) {
      $('keyGenNote').textContent = '「' + name + '」新密钥（仅显示一次，请立即保存）：' + r.generated_key;
      toast('key 已创建，密钥显示在下方提示区', 'ok');
    } else {
      toast('key 已保存并热生效', 'ok');
    }
    $('keyName').dataset.editing = '';
    loadKeys();
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
};

$('btnKeyDelete').onclick = async () => {
  const name = $('keyName').value.trim();
  if (!name) { toast('先在名称框输入要删除的 key 名', 'err'); return; }
  if (!confirm('确定删除 key「' + name + '」？使用该 key 的客户端将立即 401。')) return;
  try {
    await api('keys/remove', { method: 'POST', body: JSON.stringify({ name }) });
    toast('key 已删除', 'ok');
    $('keyName').value = ''; $('keySecret').value = ''; $('keyModels').value = '';
    $('keyRPM').value = 0; $('keyNote').value = ''; $('keyFreeGuard').checked = false;
    loadKeys();
  } catch (e) { toast('删除失败：' + e.message, 'err'); }
};
