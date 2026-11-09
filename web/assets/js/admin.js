const content = document.querySelector('#view-content');
const toast = document.querySelector('#toast');
const loginView = document.querySelector('#login-view');
const appView = document.querySelector('#admin-app');
let currentView = 'dashboard';
let listOffset = 0;
const pageSize = 20;
let appTimezone = 'Asia/Singapore';

function esc(value) {
  return String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
}
function cookie(name) {
  const item = document.cookie.split('; ').find((part) => part.startsWith(`${name}=`));
  return item ? decodeURIComponent(item.slice(name.length + 1)) : '';
}
async function api(url, options = {}) {
  const method = (options.method || 'GET').toUpperCase();
  const headers = {...(options.headers || {})};
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (!['GET','HEAD','OPTIONS'].includes(method)) headers['X-CSRF-Token'] = cookie('admin_csrf');
  const response = await fetch(url, {credentials: 'same-origin', ...options, method, headers});
  const body = await response.json().catch(() => null);
  if (response.status === 401) {
    showLogin('登录状态已过期，请重新登录。');
    throw new Error(body?.message || '登录状态已过期');
  }
  if (!response.ok || !body?.success) throw new Error(body?.message || '请求失败');
  return body.data;
}
function notify(text, kind = '') {
  toast.textContent = text;
  toast.className = `toast ${kind}`;
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => toast.classList.add('hidden'), 3200);
}
function showLogin(text = '') {
  appView.classList.add('hidden');
  loginView.classList.remove('hidden');
  const box = document.querySelector('#login-message');
  if (text) { box.textContent = text; box.className = 'notice error'; }
  else box.classList.add('hidden');
}
function showApp(admin) {
  loginView.classList.add('hidden');
  appView.classList.remove('hidden');
  document.querySelector('#admin-name').textContent = admin.username;
  loadView(currentView);
  checkPanel();
}
function bytes(value) {
  if (!value) return '无限制';
  return `${(value / (1024 ** 3)).toLocaleString('zh-CN', {maximumFractionDigits: 2})} GiB`;
}
function usedBytes(value) {
  return `${(value / (1024 ** 3)).toLocaleString('zh-CN', {maximumFractionDigits: 2})} GiB`;
}
function date(value) {
  if (!value) return '—';
  return new Intl.DateTimeFormat('zh-CN', {dateStyle: 'medium', timeStyle: 'short', timeZone: appTimezone}).format(new Date(value));
}
function pill(status) {
  const labels = {SUCCESS:'正常',FAILED:'失败',CANCELLED:'已取消付款',DISABLED:'已停用',REVIEW_REQUIRED:'待核对',PROCESSING:'处理中',RESERVED:'待执行',APPLYING:'执行中',VERIFYING:'核验中'};
  return `<span class="status-pill ${esc(String(status).toLowerCase())}">${esc(labels[status] || status)}</span>`;
}
function heading(title, description, action = '') {
  return `<div class="page-heading"><div><h2>${title}</h2><p>${description}</p></div>${action}</div>`;
}
function empty(label) { return `<div class="empty-state">${esc(label)}</div>`; }
function table(headers, rows, minWidth = '') {
  if (!rows.length) return empty('暂无数据');
  return `<div class="table-wrap"><table ${minWidth ? `style="min-width:${minWidth}"` : ''}><thead><tr>${headers.map((h) => `<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table></div>`;
}
function setTitle(view) {
  const titles = {dashboard:'仪表盘','client-resets':'重置记录',clients:'客户查询',settings:'系统设置'};
  document.querySelector('#view-title').textContent = titles[view] || '管理后台';
  document.querySelectorAll('.nav-item').forEach((item) => item.classList.toggle('active', item.dataset.view === view));
}
async function loadView(view) {
  currentView = view;
  setTitle(view);
  content.innerHTML = '<div class="empty-state">正在加载…</div>';
  try {
    if (view === 'dashboard') await renderDashboard();
    else if (view === 'client-resets') await renderClientResets();
    else if (view === 'clients') await renderClients();
    else await renderSettings();
  } catch (error) { content.innerHTML = `<div class="panel-card">${esc(error.message)}</div>`; }
}
function resetStatusLabel(paymentStatus) {
  const labels = {NOT_REQUIRED:'旧订单未扣积分', PENDING:'等待付款', SUCCESS:'已付款', AMOUNT_MISMATCH:'金额异常', REFUNDING:'退款处理中', REFUND_UNCERTAIN:'退款待核实', REFUNDED:'已退款'};
  return esc(labels[paymentStatus] || paymentStatus || '未知');
}
function clientResetActions(row) {
  const buttons = [];
  const orderNo = esc(row.orderNo);
  const add = (action, label) => buttons.push(`<button class="text-button" data-reset-action="${action}" data-order-no="${orderNo}">${label}</button>`);
  if (row.status === 'PAYMENT_PENDING' && row.paymentStatus === 'PENDING') add('confirm-payment', '手动确认收款');
  if (row.paymentStatus === 'SUCCESS') {
    add('refund', '发起全额退款');
    if (row.status === 'REVIEW_REQUIRED') add('confirm-success', '核验并确认完成');
  }
  if (row.paymentStatus === 'AMOUNT_MISMATCH') add('refund', '发起全额退款');
  if (row.paymentStatus === 'REFUND_UNCERTAIN') add('confirm-refund', '确认已退款');
  if (row.status === 'REVIEW_REQUIRED' && ['NOT_REQUIRED','PENDING','REFUNDED'].includes(row.paymentStatus)) {
    add('reconcile', '核对并解除');
  }
  return buttons.length ? buttons.join(' ') : '—';
}
async function renderDashboard() {
  const [stats, resets, panelStatus] = await Promise.all([
    api('/api/admin/dashboard'),
    api('/api/admin/client-resets?limit=6'),
    api('/api/admin/panel/status').catch(() => null)
  ]);
  const items = [
    ['重置总数', stats.resetTotal, '累计提交'], ['成功重置', stats.resetSuccess, '面板已确认'],
    ['待核对', stats.resetReview, '需要管理员检查'], ['今日重置', stats.todayResets, '按服务时区统计']
  ];
  const rows = resets.items.map((row) => `<tr><td><strong>${esc(row.orderNo)}</strong></td><td>${esc(row.clientEmail)}</td><td>${esc(row.planName)} · ${row.durationMonths ? `${row.durationMonths} 个月` : `${row.durationDays} 天`}</td><td>${row.pointsCost}（${resetStatusLabel(row.paymentStatus)}）</td><td>${pill(row.status)}</td><td>${date(row.createdAt)}</td></tr>`);
  content.innerHTML = heading('重置服务概览','用户可选 100 GB / 100 LDC、200 GB / 200 LDC 或 300 GB / 300 LDC，均为 1 个月套餐；LDC 确认付款后，服务重置流量和有效期。') + `<div class="stats-grid">${items.map(([label,value,meta])=>`<div class="stat-card"><div class="stat-label">${label}</div><div class="stat-value">${Number(value || 0).toLocaleString()}</div><div class="stat-meta">${meta}</div></div>`).join('')}</div><div class="panel-card"><h3 class="section-title">最近重置</h3>${table(['订单号','客户端 ID','套餐与期限','积分状态','处理状态','时间'],rows)}</div><div class="panel-card" style="margin-top:14px"><h3 class="section-title">面板连接</h3><p class="muted">${panelStatus ? `3x-ui ${esc(panelStatus.version)} · 主面板连接正常 · 远程节点同步状态：未知` : '当前无法读取面板状态。请在系统设置中检查连接。'}</p></div>`;
}
function pager(total) {
  const first = total ? listOffset + 1 : 0;
  const last = Math.min(listOffset + pageSize, total);
  return `<div class="pagination"><span>显示 ${first}-${last} 条，共 ${total} 条</span><div><button class="button subtle" data-page="prev" ${listOffset <= 0 ? 'disabled' : ''}>上一页</button> <button class="button subtle" data-page="next" ${listOffset + pageSize >= total ? 'disabled' : ''}>下一页</button></div></div>`;
}
async function renderClientResets() {
  const data = await api(`/api/admin/client-resets?limit=${pageSize}&offset=${listOffset}`);
  const rows = data.items.map((r) => `<tr><td><strong>${esc(r.orderNo)}</strong></td><td>${esc(r.paymentTradeNo || '—')}</td><td>${esc(r.paymentMoney || '—')}</td><td>${esc(r.clientEmail)}</td><td>${esc(r.planName)} · ${r.durationMonths ? `${r.durationMonths} 个月` : `${r.durationDays} 天`}</td><td>${r.pointsCost}（${resetStatusLabel(r.paymentStatus)}）</td><td>${pill(r.status)}</td><td>${date(r.createdAt)}</td><td>${esc(r.errorMessage || '—')}</td><td>${clientResetActions(r)}</td></tr>`);
  content.innerHTML = heading('客户端重置记录','管理员可手动确认 LDC 收款、核验重置结果或随时发起全额退款。退款结果不确定时，可先在 LDC 核对再手动确认。') + `<div class="panel-card">${table(['订单号','LDC 交易号','实付积分','客户端 ID','套餐与期限','积分','处理状态','时间','处理信息','操作'],rows,'1600px')}${pager(data.total)}</div>`;
}
async function renderClients() {
  const clients = await api('/api/admin/clients?limit=200');
  const rows = clients.items.map((c) => `<tr><td><strong>${esc(c.email)}</strong></td><td>${c.enabled ? pill('SUCCESS') : pill('DISABLED')}</td><td>${date(c.expiryTime)}</td><td>${bytes(c.totalBytes)}</td><td>${usedBytes(c.usedBytes)}</td><td>${c.inboundIds.length}</td></tr>`);
  content.innerHTML = heading('客户查询','只读读取主面板客户端状态；同步状态未被面板确认时显示未知。') + `<div class="panel-card">${table(['客户端 ID','状态','到期时间','总流量','已用流量','关联入站'],rows)}</div>`;
}
async function checkPanel() {
  try {
    const status = await api('/api/admin/panel/status');
    const pillEl = document.querySelector('#panel-pill');
    pillEl.textContent = `面板已连接 · ${status.version}`;
    pillEl.className = 'status-pill success';
  } catch {
    const pillEl = document.querySelector('#panel-pill');
    pillEl.textContent = '面板未连接';
    pillEl.className = 'status-pill failed';
  }
}
async function renderSettings() {
  const status = await api('/api/admin/settings');
  content.innerHTML = heading('系统设置','面板连接信息由服务器环境变量管理，不会在页面中显示令牌。') + `<div class="panel-card"><h3 class="section-title">3x-ui 主面板</h3><div class="detail-grid"><div class="detail-item"><span>运行模式</span><strong>${esc(status.panelMode)}</strong></div><div class="detail-item"><span>API 令牌</span><strong>${status.tokenConfigured ? '已配置' : '未配置'}</strong></div><div class="detail-item"><span>当前状态</span><strong>${status.connected ? `连接正常 · ${esc(status.version)}` : '尚未连接'}</strong></div><div class="detail-item"><span>节点同步</span><strong>未知</strong></div></div><div class="modal-actions"><button id="check-panel" class="button primary">测试面板连接</button><button id="download-backup" class="button subtle">下载数据库备份</button></div></div><div class="panel-card" style="margin-top:14px"><h3 class="section-title">管理员安全</h3><p class="muted">修改管理员密码后，当前所有会话都会退出。</p><button id="change-password" class="button subtle">修改密码</button></div>`;
  document.querySelector('#check-panel').addEventListener('click', async () => {await checkPanel();notify('面板连接检查已完成');renderSettings();});
  document.querySelector('#download-backup').addEventListener('click', async () => {
    try {
      const response = await fetch('/api/admin/backup', {method:'POST', credentials:'same-origin', headers:{'X-CSRF-Token':cookie('admin_csrf')}});
      if (response.status === 401) { showLogin('登录状态已过期，请重新登录。'); throw new Error('登录状态已过期'); }
      if (!response.ok) throw new Error('备份下载失败');
      const blob = await response.blob();
      const link = document.createElement('a');
      link.href = URL.createObjectURL(blob);
      link.download = `client-reset-backup-${new Date().toISOString().replaceAll(':','-')}.db`;
      link.click();
      setTimeout(() => URL.revokeObjectURL(link.href), 1000);
      notify('数据库备份已下载','success');
    } catch (error) { notify(error.message,'error'); }
  });
  document.querySelector('#change-password').addEventListener('click', () => {
    const oldPassword = prompt('请输入当前密码');
    if (oldPassword === null) return;
    const newPassword = prompt('请输入新密码（至少 12 个字符）');
    if (newPassword === null) return;
    api('/api/admin/auth/change-password', {method:'POST',body:JSON.stringify({currentPassword:oldPassword,newPassword})}).then(()=>showLogin('密码已修改，请使用新密码登录。')).catch((error)=>notify(error.message,'error'));
  });
}

document.querySelector('#login-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector('button[type="submit"]');
  button.disabled = true;
  try {
    const data = await api('/api/admin/auth/login', {method:'POST',body:JSON.stringify({username:form.elements.username.value,password:form.elements.password.value})});
    form.reset();
    showApp(data.admin);
  } catch (error) {
    const box = document.querySelector('#login-message');
    box.textContent = error.message;
    box.className = 'notice error';
  } finally { button.disabled = false; }
});
document.querySelector('#logout-button').addEventListener('click', async () => {
  try { await api('/api/admin/auth/logout',{method:'POST',body:'{}'}); } catch {}
  document.cookie = 'admin_csrf=; Max-Age=0; path=/; SameSite=Strict';
  showLogin();
});
document.querySelector('#navigation').addEventListener('click', (event) => {
  const item = event.target.closest('[data-view]');
  if (!item) return;
  document.querySelector('.sidebar').classList.remove('open');
  listOffset = 0;
  loadView(item.dataset.view);
});
document.querySelector('#menu-toggle').addEventListener('click', () => document.querySelector('.sidebar').classList.toggle('open'));
document.addEventListener('click', (event) => {
  const action = event.target.closest('[data-reset-action]');
  if (action) {
    const orderNo = action.dataset.orderNo;
    const kind = action.dataset.resetAction;
    let note = '';
    let endpoint = '';
    let payload = {};
    if (kind === 'confirm-payment') {
      const tradeNo = prompt('请填写 LDC 后台显示的 trade_no：');
      if (!tradeNo?.trim()) return;
      note = prompt('请填写包含“LDC 收款”的核对说明：', '已在 LDC 核对该交易号与订单金额，确认收款成功。');
      if (note === null || !note.trim()) return;
      endpoint = 'confirm-payment';
      payload = {tradeNo: tradeNo.trim(), note};
    } else if (kind === 'refund') {
      if (!confirm('将通过 LDC 对此订单发起全额退款，是否继续？')) return;
      note = prompt('请填写退款原因：');
      if (note === null || !note.trim()) return;
      endpoint = 'refund';
      payload = {note};
    } else if (kind === 'confirm-refund') {
      note = prompt('请先在 LDC 核对全额退款已完成，再填写包含“LDC 退款”的核对说明：', '已在 LDC 核实全额退款成功。');
      if (note === null || !note.trim()) return;
      endpoint = 'confirm-refund';
      payload = {note};
    } else if (kind === 'confirm-success') {
      note = prompt('系统会读取面板并核对套餐额度、期限和流量归零。请填写人工确认说明：');
      if (note === null || !note.trim()) return;
      endpoint = 'confirm-success';
      payload = {note};
    } else if (kind === 'reconcile') {
      note = prompt('系统会确认面板额度、期限和启用状态与操作前一致后解除订单。请填写核对说明：', '已复核面板当前状态与操作前一致，确认本次重置未执行。');
      if (note === null || !note.trim()) return;
      endpoint = 'reconcile';
      payload = {note};
    }
    api(`/api/admin/client-resets/${encodeURIComponent(orderNo)}/${endpoint}`, {method:'POST',body:JSON.stringify(payload)})
      .then(() => {notify(kind === 'refund' ? 'LDC 全额退款已处理' : '订单操作已处理','success');renderClientResets();})
      .catch((error) => notify(error.message,'error'));
    return;
  }
  const page = event.target.closest('[data-page]');
  if (!page || page.disabled || currentView !== 'client-resets') return;
  listOffset = Math.max(0, listOffset + (page.dataset.page === 'next' ? pageSize : -pageSize));
  renderClientResets();
});
fetch('/api/public/config').then((response) => response.json()).then((body) => {if(body?.data?.timezone) appTimezone = body.data.timezone;}).catch(() => {});
api('/api/admin/auth/me').then((data) => showApp(data.admin)).catch(() => showLogin());
