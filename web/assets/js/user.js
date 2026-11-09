const lookupForm = document.querySelector('#lookup-form');
const subscriptionInput = document.querySelector('#subscription-id');
const entryPanel = document.querySelector('#entry-panel');
const message = document.querySelector('#message');
const clientInfoPanel = document.querySelector('#client-info');
const lookupButton = document.querySelector('#lookup-button');
const switchAccountButton = document.querySelector('#switch-account');
const historyOrdersButton = document.querySelector('#history-orders');

const activeSubscriptionKey = 'flow-active-subscription-id';
let currentSubscriptionID = '';
let showingHistory = false;
let currentResetRequestKey = null;
let currentResetPlanCode = '100g';
let currentResetUncertain = false;
let appTimezone = 'Asia/Singapore';
let paymentEnabled = false;
let currentResetResult = null;
let pendingReturnOrderNo = '';
let currentLookupData = null;
const legacyClientResetPlan = {code: '1g', name: '1 GB', trafficBytes: 1024 ** 3, pointsCost: 1, durationDays: 30};
let clientResetPlans = [
  {code: '100g', name: '100 GB', trafficBytes: 100 * 1024 ** 3, pointsCost: 100, durationMonths: 1},
  {code: '200g', name: '200 GB', trafficBytes: 200 * 1024 ** 3, pointsCost: 200, durationMonths: 1},
  {code: '300g', name: '300 GB', trafficBytes: 300 * 1024 ** 3, pointsCost: 300, durationMonths: 1},
];

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
}

function resetStorageKey(subscriptionID) {
  return `client-reset-request:${encodeURIComponent(subscriptionID)}`;
}

function saveActiveSubscriptionID(subscriptionID) {
  try { localStorage.setItem(activeSubscriptionKey, subscriptionID); } catch {}
  try { sessionStorage.removeItem(activeSubscriptionKey); } catch {}
}

function loadActiveSubscriptionID() {
  try {
    const saved = localStorage.getItem(activeSubscriptionKey);
    if (saved) return saved;
  } catch {}
  try {
    const previous = sessionStorage.getItem(activeSubscriptionKey) || '';
    if (previous) {
      try { localStorage.setItem(activeSubscriptionKey, previous); } catch {}
      sessionStorage.removeItem(activeSubscriptionKey);
    }
    return previous;
  } catch {
    return '';
  }
}

function clearActiveSubscriptionID() {
  try { localStorage.removeItem(activeSubscriptionKey); } catch {}
  try { sessionStorage.removeItem(activeSubscriptionKey); } catch {}
}

function formatGiB(bytes) {
  return (Math.max(0, Number(bytes) || 0) / (1024 ** 3)).toLocaleString('zh-CN', {maximumFractionDigits: 2});
}

function formatBytes(bytes) {
  return `${formatGiB(bytes)} GiB`;
}

function formatDate(value) {
  if (value < 0) return '特殊期限';
  if (!value) return '无限期限';
  return new Intl.DateTimeFormat('zh-CN', {dateStyle: 'medium', timeStyle: 'short', timeZone: appTimezone}).format(new Date(value));
}

function remainingDays(expiryTime) {
  if (expiryTime === 0) return '无限期';
  if (expiryTime < 0) return '特殊期限';
  return `${Math.max(0, Math.ceil((expiryTime - Date.now()) / 86400000))} 天`;
}

function durationText(plan) {
  if (plan.durationDays) return `${plan.durationDays} 天`;
  if (plan.durationMonths) return `${plan.durationMonths} 个月`;
  return '期限待确认';
}

function previewExpiryTime(plan) {
  const now = new Date();
  if (plan.durationDays) {
    return now.getTime() + Number(plan.durationDays) * 86400000;
  }
  const months = Number(plan.durationMonths) || 1;
  const firstOfMonth = new Date(Date.UTC(
    now.getUTCFullYear(), now.getUTCMonth() + months, 1,
    now.getUTCHours(), now.getUTCMinutes(), now.getUTCSeconds(), now.getUTCMilliseconds(),
  ));
  const lastDay = new Date(Date.UTC(firstOfMonth.getUTCFullYear(), firstOfMonth.getUTCMonth() + 1, 0)).getUTCDate();
  firstOfMonth.setUTCDate(Math.min(now.getUTCDate(), lastDay));
  return firstOfMonth.getTime();
}

function requestPurchaseConfirmation(plan) {
  return new Promise((resolve) => {
    const overlay = document.createElement('div');
    overlay.className = 'flow-confirm-backdrop';
    overlay.innerHTML = `<section class="flow-confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="flow-confirm-title">
      <button type="button" class="flow-confirm-close" data-confirm-cancel aria-label="关闭">×</button>
      <span class="flow-confirm-kicker">ORDER CONFIRMATION</span>
      <h2 id="flow-confirm-title">确认套餐与支付</h2>
      <p class="flow-confirm-intro">请在前往 LDC 支付前核对本次套餐内容。</p>
      <div class="flow-confirm-details">
        <div><span>购买套餐</span><b>${escapeHTML(plan.name)}</b></div>
        <div><span>流量重置为</span><b>${escapeHTML(formatBytes(plan.trafficBytes))}</b></div>
        <div><span>预计到期日期</span><b>${escapeHTML(formatDate(previewExpiryTime(plan)))}</b></div>
        <div><span>支付金额</span><b class="flow-confirm-price">${Number(plan.pointsCost).toLocaleString('zh-CN')} LDC</b></div>
      </div>
      <p class="flow-confirm-note">付款确认后将清零已用流量；有效期按套餐期限重置。日期为预计值，以系统确认并处理付款的时间为准。</p>
      <div class="flow-confirm-actions"><button type="button" class="flow-confirm-cancel" data-confirm-cancel>返回修改</button><button type="button" class="flow-confirm-submit" data-confirm-submit>确认并前往支付 <span aria-hidden="true">→</span></button></div>
    </section>`;
    document.body.append(overlay);

    const finish = (confirmed) => {
      overlay.remove();
      document.removeEventListener('keydown', onKeyDown);
      resolve(confirmed);
    };
    const onKeyDown = (event) => {
      if (event.key === 'Escape') finish(false);
    };
    overlay.addEventListener('click', (event) => {
      if (event.target === overlay) finish(false);
    });
    overlay.querySelectorAll('[data-confirm-cancel]').forEach((button) => button.addEventListener('click', () => finish(false)));
    overlay.querySelector('[data-confirm-submit]').addEventListener('click', () => finish(true));
    document.addEventListener('keydown', onKeyDown);
    overlay.querySelector('.flow-confirm-cancel').focus();
  });
}

function showMessage(text, kind = 'error') {
  message.textContent = text;
  message.className = `flow-message ${kind}`;
}

function hideMessage() {
  message.textContent = '';
  message.className = 'flow-message hidden';
}

async function request(url, options = {}) {
  const response = await fetch(url, {
    credentials: 'same-origin',
    ...options,
    headers: {'Content-Type': 'application/json', ...(options.headers || {})},
  });
  const body = await response.json().catch(() => null);
  if (!response.ok || !body?.success) {
    const error = new Error(body?.message || '请求失败，请稍后重试');
    error.status = response.ok ? 0 : response.status;
    throw error;
  }
  return body.data;
}

function resetAccountView({clearInput = false, clearSavedSubscription = false} = {}) {
  currentSubscriptionID = '';
  currentLookupData = null;
  currentResetResult = null;
  currentResetRequestKey = null;
  currentResetPlanCode = '100g';
  currentResetUncertain = false;
  showingHistory = false;
  clientInfoPanel.replaceChildren();
  clientInfoPanel.classList.add('hidden');
  historyOrdersButton.disabled = true;
  switchAccountButton.classList.add('hidden');
  if (clearInput) subscriptionInput.value = '';
  if (clearSavedSubscription) clearActiveSubscriptionID();
}

function paymentStatusLabel(item) {
  if (item.status === 'CANCELLED') return '已取消';
  if (item.paymentStatus === 'REFUNDED') return '已退款';
  if (item.paymentStatus === 'REFUNDING') return '退款中';
  if (item.paymentStatus === 'REFUND_UNCERTAIN') return '退款待核实';
  if (item.status === 'SUCCESS') return '已完成';
  if (item.status === 'REVIEW_REQUIRED') return '待核对';
  if (item.status === 'PAYMENT_PENDING') return '待支付';
  if (['PAYMENT_CONFIRMED', 'APPLYING', 'VERIFYING'].includes(item.status)) return '处理中';
  if (item.status === 'FAILED') return '未完成';
  return item.paymentStatus === 'SUCCESS' ? '已付款' : '待处理';
}

function historyMarkup(history) {
  const items = Array.isArray(history.items) ? history.items : [];
  const rows = items.length
    ? items.map((item) => {
      const status = paymentStatusLabel(item);
      const statusClass = ['已完成', '已退款'].includes(status) ? 'success' : ['待核对', '待支付', '处理中', '退款中', '退款待核实'].includes(status) ? 'review' : status === '已取消' ? 'cancelled' : 'failed';
      const duration = item.durationDays ? `${item.durationDays} 天` : `${item.durationMonths || 0} 个月`;
      const actions = item.status === 'PAYMENT_PENDING'
        ? `<div class="flow-history-actions"><button type="button" data-order-action="continue" data-order-no="${escapeHTML(item.orderNo)}">继续支付</button><button type="button" class="secondary" data-order-action="cancel" data-order-no="${escapeHTML(item.orderNo)}">取消支付</button></div>`
        : '';
      const cancelledNote = item.status === 'CANCELLED' ? '<small class="flow-history-warning">旧 LDC 支付页仍可能收款，到账后会转入待核对</small>' : '';
      return `<div class="flow-history-item"><div class="flow-history-item-main"><b>${escapeHTML(item.planName)} · ${escapeHTML(duration)}</b><small>${escapeHTML(formatDate(item.createdAt))} · 订单 ${escapeHTML(item.orderNo)}</small>${cancelledNote}</div><span class="flow-history-amount">${escapeHTML(item.paymentMoney || item.pointsCost)} LDC</span><span class="flow-history-status ${statusClass}">${escapeHTML(status)}</span>${actions}</div>`;
    }).join('')
    : '<div class="flow-history-empty">还没有套餐订单</div>';
  return `<div class="flow-history-stats">
    <div class="flow-history-stat"><span>全部订单</span><b>${Number(history.total || 0)}<small>笔</small></b></div>
    <div class="flow-history-stat"><span>成功充值</span><b>${Number(history.successCount || 0)}<small>笔</small></b></div>
    <div class="flow-history-stat"><span>累计支付</span><b>${Number(history.totalPaid || 0)}<small>LDC</small></b></div>
  </div><div class="flow-history-list">${rows}</div>`;
}

async function loadHistory(subscriptionID) {
  const target = clientInfoPanel.querySelector('[data-history-content]');
  if (!target) return;
  target.innerHTML = '<div class="flow-history-loading">正在加载订单记录…</div>';
  try {
    const history = await request('/api/public/client-reset/history', {
      method: 'POST',
      body: JSON.stringify({subscriptionId: subscriptionID}),
    });
    if (currentSubscriptionID !== subscriptionID || !target.isConnected) return;
    target.innerHTML = historyMarkup(history);
    const count = clientInfoPanel.querySelector('[data-history-count]');
    if (count) count.textContent = `${Number(history.total || 0)} 笔`;
    target.querySelectorAll('[data-order-action]').forEach((button) => {
      button.addEventListener('click', async () => {
        const orderNo = button.dataset.orderNo;
        if (!orderNo || !currentSubscriptionID) return;
        const action = button.dataset.orderAction;
        if (action === 'cancel' && !window.confirm('确认取消这笔订单？LDC 官方接口不能关闭旧支付页，旧链接仍可能付款；之后若到账，订单会进入待核对且不会自动充值。')) return;
        const siblingButtons = [...target.querySelectorAll('[data-order-no]')].filter((item) => item.dataset.orderNo === orderNo);
        siblingButtons.forEach((item) => { item.disabled = true; });
        try {
          if (action === 'continue') {
            const order = await request(`/api/public/client-reset/orders/${encodeURIComponent(orderNo)}`, {
              method: 'POST',
              body: JSON.stringify({subscriptionId: currentSubscriptionID}),
            });
            if (currentSubscriptionID !== subscriptionID) return;
            if (order.status === 'PAYMENT_PENDING' && order.paymentUrl) {
              if (!await requestPurchaseConfirmation(order.plan)) {
                siblingButtons.forEach((item) => { item.disabled = false; });
                return;
              }
              saveActiveSubscriptionID(currentSubscriptionID);
              window.location.assign(order.paymentUrl);
              return;
            }
            drawClientInfo(currentLookupData, order);
            showMessage(order.status === 'CANCELLED' ? '这笔订单已取消，请选择套餐创建新订单。' : '订单状态已更新，已为你刷新。', order.status === 'REVIEW_REQUIRED' ? 'review' : 'success');
            return;
          }
          const order = await request(`/api/public/client-reset/orders/${encodeURIComponent(orderNo)}/cancel`, {
            method: 'POST',
            body: JSON.stringify({subscriptionId: currentSubscriptionID}),
          });
          if (currentSubscriptionID !== subscriptionID) return;
          drawClientInfo(currentLookupData, order);
          if (order.status === 'CANCELLED') {
            showMessage('订单已在本地取消。旧 LDC 支付页仍可能收款，若之后到账会转入待核对。', 'review');
          } else {
            showMessage('订单状态已变化，已为你刷新。', order.status === 'REVIEW_REQUIRED' ? 'review' : 'success');
          }
        } catch (error) {
          if (currentSubscriptionID !== subscriptionID) return;
          showMessage(error.message);
          siblingButtons.forEach((item) => { item.disabled = false; });
        }
      });
    });
  } catch {
    if (currentSubscriptionID === subscriptionID && target.isConnected) {
      target.innerHTML = '<div class="flow-history-empty">暂时无法读取订单记录</div>';
    }
  }
}

function renderHistoryView() {
  if (!currentSubscriptionID) return;
  clientInfoPanel.innerHTML = `<div class="flow-history-page">
    <div class="flow-history-page-head">
      <div><button type="button" class="flow-history-back" data-history-back>← 返回账号</button><h2>最近套餐订单</h2><p>订阅 ID：${escapeHTML(currentSubscriptionID)}</p></div>
      <span class="flow-history-count" data-history-count>读取中</span>
    </div>
    <section class="flow-panel flow-history-panel"><div data-history-content><div class="flow-history-loading">正在加载订单记录…</div></div></section>
  </div>`;
  clientInfoPanel.classList.remove('hidden');
  hideMessage();
  clientInfoPanel.querySelector('[data-history-back]').addEventListener('click', () => {
    showingHistory = false;
    drawClientInfo(currentLookupData, currentResetResult);
  });
  loadHistory(currentSubscriptionID);
}

function resetResultMessage(result) {
  if (!result) return '';
  if (result.status === 'CANCELLED') return '本地订单已取消。旧 LDC 支付页仍可能收款；如已付款，请联系管理员核对。';
  if (result.status === 'PAYMENT_PENDING') return '订单已创建，等待 LDC 支付。付款后服务会自动查单并执行套餐重置。';
  if (result.status === 'PAYMENT_CONFIRMED') return 'LDC 已确认付款，正在更新流量和有效期。';
  if (result.status === 'SUCCESS') return '付款确认，流量与有效期已更新。';
  if (result.status === 'REVIEW_REQUIRED') return result.paymentStatus === 'SUCCESS'
    ? 'LDC 已收款，套餐处理结果待管理员核对。'
    : '订单结果待核对，请联系管理员并提供订单号。';
  if (result.status === 'FAILED' && result.paymentStatus === 'SUCCESS') return result.errorMessage || 'LDC 已收款，但套餐未完成，请联系管理员处理。';
  if (result.status === 'FAILED') return '上次操作未完成，可以重新提交。';
  if (['APPLYING', 'VERIFYING'].includes(result.status)) return '套餐正在更新，请稍后查询。';
  return result.errorMessage || '';
}

function durationDaysRemaining(expiryTime) {
  if (expiryTime <= 0) return 0;
  return Math.max(0, Math.ceil((expiryTime - Date.now()) / 86400000));
}

function renderCheckoutSummary(plan, resetResult, pendingReset, resetBlocked, resetUncertain) {
  const resetStatus = resetResult?.status;
  const messageText = resetResultMessage(resetResult);
  const messageClass = resetStatus === 'SUCCESS' ? 'success' : resetStatus === 'CANCELLED' || resetStatus === 'REVIEW_REQUIRED' || resetUncertain ? 'review' : '';
  const pendingOrderNo = resetResult?.orderNo || pendingReset?.orderNo;
  const buttonText = resetBlocked ? '查询订单状态'
    : resetStatus === 'PAYMENT_PENDING' ? '查询支付状态'
      : resetUncertain ? '查询这次操作'
        : resetStatus === 'FAILED' ? '重新提交支付'
          : '确认套餐并跳转支付';
  const continuePayment = resetStatus === 'PAYMENT_PENDING' && resetResult?.paymentUrl
    ? '<button class="flow-continue-payment" id="continue-existing-payment" type="button">继续完成这笔支付 <span aria-hidden="true">↗</span></button>'
    : '';
  const pendingNote = pendingOrderNo && resetStatus === 'PAYMENT_PENDING'
    ? `<div class="flow-pending-order"><span>◷</span><span>有一笔 ${escapeHTML(plan.name)} 待支付</span><span class="flow-history-status pending">待支付</span></div>`
    : '';
  const messageMarkup = messageText ? `<div class="flow-operation-message ${messageClass}" role="status">${escapeHTML(messageText)}${resetResult?.orderNo ? `<small>订单号：${escapeHTML(resetResult.orderNo)}</small>` : ''}</div>` : '';
  return `<section class="flow-panel flow-summary-panel">
    <div class="flow-summary-heading"><h2>本次充值</h2><span>ORDER SUMMARY</span></div>
    ${pendingNote}
    <div class="flow-selected-plan"><span class="flow-selected-icon">↗</span><div><h3 data-selected-plan>${escapeHTML(plan.name)}</h3><p data-selected-plan-meta>${escapeHTML(formatBytes(plan.trafficBytes))} 流量 · ${escapeHTML(durationText(plan))} 有效期</p></div><span class="flow-selected-check">✓</span></div>
    <div class="flow-summary-rows">
      <div class="flow-summary-row"><span>充值账号</span><b class="flow-summary-subscription">${escapeHTML(currentSubscriptionID)}</b></div>
      <div class="flow-summary-row"><span>充值后总流量</span><b data-summary-traffic>${escapeHTML(formatBytes(plan.trafficBytes))}</b></div>
      <div class="flow-summary-row"><span>流量使用记录</span><b>清零重置</b></div>
      <div class="flow-summary-row"><span>有效期</span><b data-summary-duration>付款成功起 ${escapeHTML(durationText(plan))}</b></div>
    </div>
    <div class="flow-price-total"><span>合计支付</span><b data-summary-price>${Number(plan.pointsCost).toLocaleString('zh-CN')}<small>LDC</small></b></div>
    ${messageMarkup}
    <button class="flow-checkout" id="client-reset-button" type="button"><span>${escapeHTML(buttonText)}</span><span aria-hidden="true">→</span></button>
    ${continuePayment}
    <div class="flow-secure-caption"><span>●</span> Linux.do Credit 安全支付</div>
    <p class="flow-payment-note">付款确认后自动更新，无需手动刷新。</p>
    ${!paymentEnabled ? '<p class="flow-payment-note">LDC 支付尚未配置，请联系管理员。</p>' : ''}
  </section>`;
}

function drawClientInfo(data, resetResult = null) {
  currentLookupData = data;
  currentResetResult = resetResult;
  currentSubscriptionID = data.subscriptionId || currentSubscriptionID || subscriptionInput.value.trim();
  if (!currentSubscriptionID) return;
  saveActiveSubscriptionID(currentSubscriptionID);
  historyOrdersButton.disabled = false;
  switchAccountButton.classList.remove('hidden');

  const pendingKey = resetStorageKey(currentSubscriptionID);
  let pendingReset = null;
  try { pendingReset = JSON.parse(sessionStorage.getItem(pendingKey) || 'null'); } catch { sessionStorage.removeItem(pendingKey); }
  if (!resetResult && pendingReset?.requestKey) {
    currentResetRequestKey = pendingReset.requestKey;
    currentResetPlanCode = pendingReset.planCode || '100g';
    currentResetUncertain = true;
  }
  const resetStatus = resetResult?.status;
  if (resetResult?.orderNo) {
    sessionStorage.setItem(pendingKey, JSON.stringify({
      planCode: resetResult.plan?.code || currentResetPlanCode,
      requestKey: currentResetRequestKey || pendingReset?.requestKey || '',
      orderNo: resetResult.orderNo,
    }));
  }
  if (resetStatus === 'SUCCESS' || resetStatus === 'CANCELLED' || (resetStatus === 'FAILED' && resetResult?.paymentStatus !== 'SUCCESS')) {
    currentResetRequestKey = null;
    currentResetUncertain = false;
    sessionStorage.removeItem(pendingKey);
    pendingReset = null;
  } else if (resetResult && ['PAYMENT_PENDING', 'PAYMENT_CONFIRMED', 'APPLYING', 'VERIFYING', 'REVIEW_REQUIRED'].includes(resetStatus)) {
    currentResetUncertain = true;
  }

  if (showingHistory) {
    renderHistoryView();
    return;
  }

  const snapshot = data.snapshot;
  const expired = snapshot.expiryTime > 0 && snapshot.expiryTime <= Date.now();
  const statusLabel = expired ? '已到期' : snapshot.hasEnabled ? (snapshot.enabled ? '运行正常' : '已停用') : '状态未知';
  const statusClass = expired ? 'expired' : !snapshot.hasEnabled || snapshot.enabled ? '' : 'disabled';
  const totalBytes = Number(snapshot.totalBytes || 0);
  const used = Number(snapshot.usedBytes || 0);
  const remaining = Math.max(0, Number(snapshot.remainingBytes ?? totalBytes - used));
  const usedPercent = totalBytes > 0 ? Math.min(100, Math.max(0, (used / totalBytes) * 100)) : 0;
  const expiryLabel = snapshot.expiryTime === 0 ? '无限期限' : formatDate(snapshot.expiryTime);
  const daysLabel = snapshot.expiryTime < 0 ? '特殊期限' : snapshot.expiryTime === 0 ? '不设期限' : `剩余 ${durationDaysRemaining(snapshot.expiryTime)} 天`;
  const requestedPlanCode = resetResult?.plan?.code || pendingReset?.planCode || currentResetPlanCode;
  const pendingLegacyPlan = requestedPlanCode === legacyClientResetPlan.code && currentResetUncertain;
  const plan = clientResetPlans.find((item) => item.code === requestedPlanCode) || (pendingLegacyPlan ? legacyClientResetPlan : null) || clientResetPlans.find((item) => item.code === currentResetPlanCode) || clientResetPlans[0];
  currentResetPlanCode = plan.code;
  const resetBlocked = resetStatus === 'REVIEW_REQUIRED' || (resetStatus === 'FAILED' && resetResult?.paymentStatus === 'SUCCESS');
  const resetUncertain = currentResetUncertain || ['PAYMENT_PENDING', 'PAYMENT_CONFIRMED', 'APPLYING', 'VERIFYING'].includes(resetStatus);
  const planCards = clientResetPlans.map((item) => {
    const selected = item.code === plan.code;
    const recommend = item.code === '100g' ? '<span class="flow-plan-recommend">人气之选</span>' : '';
    return `<label class="flow-plan-card ${selected ? 'selected' : ''}">
      ${recommend}<input type="radio" name="reset-plan" value="${escapeHTML(item.code)}" ${selected ? 'checked' : ''} ${resetBlocked || resetUncertain ? 'disabled' : ''}>
      <div class="flow-plan-top"><span class="flow-plan-icon">${escapeHTML(item.code === '100g' ? '↗' : item.code === '200g' ? 'Ⅱ' : 'Ⅲ')}</span><span class="flow-plan-radio"></span></div>
      <h3>${escapeHTML(item.name)}</h3><span class="flow-plan-duration">${escapeHTML(durationText(item))} 有效期</span>
      <div class="flow-plan-price"><b>${Number(item.pointsCost).toLocaleString('zh-CN')}</b><small>LDC</small></div>
    </label>`;
  }).join('');
  const statusMessage = resetResultMessage(resetResult);
  const statusClassName = resetStatus === 'SUCCESS' ? 'success' : resetStatus === 'REVIEW_REQUIRED' || resetUncertain ? 'review' : '';

  clientInfoPanel.innerHTML = `<div class="flow-dashboard-grid">
    <div class="flow-dashboard-main">
      <section class="flow-panel flow-account-panel">
        <div class="flow-account-head">
          <div class="flow-account-title"><span class="flow-tiny-square"></span><h2>账号概览</h2><span class="flow-status ${statusClass}">${escapeHTML(statusLabel)}</span></div>
          <div class="flow-account-meta"><span class="flow-sub-id">订阅 ID：<b>${escapeHTML(currentSubscriptionID)}</b><button type="button" class="flow-copy-button" id="copy-subscription">复制</button></span></div>
        </div>
        <div class="flow-account-content">
          <div class="flow-balance-block">
            <span class="flow-balance-label">剩余可用流量</span>
            <div class="flow-balance-value">${totalBytes > 0 ? formatGiB(remaining) : '∞'}<small>GiB</small></div>
            <div class="flow-progress" aria-label="流量使用进度"><span style="width:${usedPercent.toFixed(1)}%"></span></div>
            <div class="flow-usage-caption"><span>已使用 ${escapeHTML(formatBytes(used))}</span><span>共 ${escapeHTML(totalBytes > 0 ? formatBytes(totalBytes) : '无限制')}</span></div>
          </div>
          <div class="flow-account-details">
            <div class="flow-detail-card"><span class="flow-detail-label">套餐到期时间</span><b>${escapeHTML(expiryLabel)}</b><small>${escapeHTML(daysLabel)}</small></div>
            <div class="flow-detail-card"><span class="flow-detail-label">流量使用记录</span><b>${escapeHTML(formatBytes(used))}</b><small>${snapshot.hasTrafficBreakdown ? `上传 ${escapeHTML(formatBytes(snapshot.uploadBytes))} · 下载 ${escapeHTML(formatBytes(snapshot.downloadBytes))}` : '统计明细暂不可用'}</small></div>
          </div>
        </div>
      </section>

      <section class="flow-panel flow-packages-panel">
        <div class="flow-section-head"><div><h2>为下一程，选一份流量</h2><p>付款成功后重置流量和期限，原有剩余流量不叠加。</p></div><span class="flow-section-tag">LINUX.DO CREDIT</span></div>
        <div class="flow-plan-grid">${planCards}</div>
        <div class="flow-reset-note"><span aria-hidden="true">ⓘ</span><span><b>充值说明</b> · 套餐付款确认后，将总流量设为所选额度并清零已用流量；有效期从付款成功时按套餐期限重新计算。</span></div>
      </section>

      <div class="flow-help-line"><span aria-hidden="true">ⓘ</span><span><b>处理结果待核对？</b> 请暂勿重复付款，联系管理员并提供订单编号。付款状态可由 LDC 回调及服务端查单核实。</span></div>
    </div>
    <aside class="flow-checkout-column">${renderCheckoutSummary(plan, resetResult, pendingReset, resetBlocked, resetUncertain)}</aside>
  </div>`;
  clientInfoPanel.classList.remove('hidden');
  hideMessage();

  clientInfoPanel.querySelector('#copy-subscription').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(currentSubscriptionID);
      showMessage('订阅 ID 已复制。', 'success');
    } catch {
      showMessage('复制失败，请手动选择订阅 ID。');
    }
  });

  clientInfoPanel.querySelectorAll('input[name="reset-plan"]').forEach((radio) => {
    radio.addEventListener('change', () => {
      if (!radio.checked) return;
      currentResetPlanCode = radio.value;
      currentResetRequestKey = null;
      clientInfoPanel.querySelectorAll('.flow-plan-card').forEach((card) => card.classList.toggle('selected', card.contains(radio)));
      const selectedPlan = clientResetPlans.find((item) => item.code === radio.value);
      if (!selectedPlan) return;
      const name = clientInfoPanel.querySelector('[data-selected-plan]');
      const meta = clientInfoPanel.querySelector('[data-selected-plan-meta]');
      const traffic = clientInfoPanel.querySelector('[data-summary-traffic]');
      const duration = clientInfoPanel.querySelector('[data-summary-duration]');
      const price = clientInfoPanel.querySelector('[data-summary-price]');
      if (name) name.textContent = selectedPlan.name;
      if (meta) meta.textContent = `${formatBytes(selectedPlan.trafficBytes)} 流量 · ${durationText(selectedPlan)} 有效期`;
      if (traffic) traffic.textContent = formatBytes(selectedPlan.trafficBytes);
      if (duration) duration.textContent = `付款成功起 ${durationText(selectedPlan)}`;
      if (price) price.innerHTML = `${Number(selectedPlan.pointsCost).toLocaleString('zh-CN')}<small>LDC</small>`;
    });
  });

  clientInfoPanel.querySelector('#continue-existing-payment')?.addEventListener('click', async (event) => {
    const button = event.currentTarget;
    const subscriptionID = currentSubscriptionID;
    button.disabled = true;
    try {
      const order = await request(`/api/public/client-reset/orders/${encodeURIComponent(pendingOrderNo)}`, {
        method: 'POST',
        body: JSON.stringify({subscriptionId: subscriptionID}),
      });
      if (currentSubscriptionID !== subscriptionID) return;
      if (order.status !== 'PAYMENT_PENDING' || !order.paymentUrl) {
        drawClientInfo(currentLookupData, order);
        return;
      }
      if (!await requestPurchaseConfirmation(order.plan)) return;
      saveActiveSubscriptionID(subscriptionID);
      window.location.assign(order.paymentUrl);
    } catch (error) {
      if (currentSubscriptionID === subscriptionID) showMessage(error.message);
    } finally {
      if (button.isConnected) button.disabled = false;
    }
  });

  clientInfoPanel.querySelector('#client-reset-button').addEventListener('click', async (event) => {
    const button = event.currentTarget;
    if (!paymentEnabled) {
      try {
        const config = await request('/api/public/client-reset/plans');
        paymentEnabled = config.paymentEnabled === true;
      } catch {}
      if (!paymentEnabled) {
        showMessage('LDC 支付尚未配置，请联系管理员。');
        return;
      }
    }
    const pendingState = (() => {
      try { return JSON.parse(sessionStorage.getItem(pendingKey) || 'null'); } catch { return null; }
    })();
    const knownOrder = resetResult?.orderNo || pendingState?.orderNo;
    const checkingExisting = resetBlocked || resetUncertain;
    const selectedPlan = clientResetPlans.find((item) => item.code === currentResetPlanCode);
    if (!checkingExisting && !selectedPlan) return;
    if (!checkingExisting) {
      button.disabled = true;
      if (!await requestPurchaseConfirmation(selectedPlan)) {
        button.disabled = false;
        return;
      }
    }
    if (!currentResetRequestKey) currentResetRequestKey = pendingState?.requestKey || globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
    sessionStorage.setItem(pendingKey, JSON.stringify({planCode: currentResetPlanCode, requestKey: currentResetRequestKey, orderNo: knownOrder || ''}));
    saveActiveSubscriptionID(currentSubscriptionID);
    button.disabled = true;
    button.querySelector('span:first-child').textContent = checkingExisting ? '正在查询…' : '正在创建支付…';
    try {
      if (knownOrder && checkingExisting) {
        const result = await request(`/api/public/client-reset/orders/${encodeURIComponent(knownOrder)}`, {
          method: 'POST',
          body: JSON.stringify({subscriptionId: currentSubscriptionID}),
        });
        drawClientInfo(currentLookupData, result);
        return;
      }
      const result = await request('/api/public/client-reset', {
        method: 'POST',
        body: JSON.stringify({subscriptionId: currentSubscriptionID, planCode: currentResetPlanCode, requestKey: currentResetRequestKey}),
      });
      sessionStorage.setItem(pendingKey, JSON.stringify({planCode: currentResetPlanCode, requestKey: currentResetRequestKey, orderNo: result.orderNo || ''}));
      if (result.paymentUrl && result.status === 'PAYMENT_PENDING') {
        window.location.assign(result.paymentUrl);
        return;
      }
      drawClientInfo(currentLookupData, result);
    } catch (error) {
      if (error.status >= 400 && error.status < 500) {
        currentResetRequestKey = null;
        currentResetUncertain = false;
        sessionStorage.removeItem(pendingKey);
        showMessage(error.message);
        clientInfoPanel.querySelectorAll('input[name="reset-plan"]').forEach((radio) => { radio.disabled = false; });
        button.querySelector('span:first-child').textContent = '重新提交支付';
      } else {
        currentResetUncertain = true;
        clientInfoPanel.querySelectorAll('input[name="reset-plan"]').forEach((radio) => { radio.disabled = true; });
        button.querySelector('span:first-child').textContent = '查询这次操作';
        showMessage(`${error.message}。请使用同一订阅 ID 查询结果，暂勿重复付款。`, 'review');
      }
    } finally {
      button.disabled = false;
    }
  });
}

async function performLookup(subscriptionID, orderNo = '') {
  const value = subscriptionID.trim();
  if (!value) {
    showMessage('请输入订阅 ID。');
    return;
  }
  hideMessage();
  lookupButton.disabled = true;
  lookupButton.querySelector('span:last-child').textContent = '查询中…';
  try {
    const data = await request('/api/public/clients/lookup', {
      method: 'POST',
      body: JSON.stringify({subscriptionId: value}),
    });
    if (subscriptionInput.value.trim() && subscriptionInput.value.trim() !== value) return;
    data.subscriptionId = data.subscriptionId || value;
    currentSubscriptionID = data.subscriptionId;
    saveActiveSubscriptionID(currentSubscriptionID);
    drawClientInfo(data);
    const key = resetStorageKey(currentSubscriptionID);
    let savedOrder = null;
    try { savedOrder = JSON.parse(sessionStorage.getItem(key) || 'null'); } catch { sessionStorage.removeItem(key); }
    const orderToRestore = orderNo || savedOrder?.orderNo;
    if (orderToRestore) {
      const order = await request(`/api/public/client-reset/orders/${encodeURIComponent(orderToRestore)}`, {
        method: 'POST',
        body: JSON.stringify({subscriptionId: currentSubscriptionID}),
      });
      drawClientInfo(data, order);
    }
    pendingReturnOrderNo = '';
    if (window.location.search) history.replaceState(null, '', window.location.pathname);
  } catch (error) {
    showMessage(error.message);
  } finally {
    lookupButton.disabled = false;
    lookupButton.querySelector('span:last-child').textContent = '查询账号';
  }
}

lookupForm.addEventListener('submit', (event) => {
  event.preventDefault();
  performLookup(subscriptionInput.value, pendingReturnOrderNo);
});

historyOrdersButton.addEventListener('click', () => {
  if (!currentSubscriptionID || !currentLookupData) return;
  showingHistory = true;
  renderHistoryView();
  clientInfoPanel.scrollIntoView({behavior: 'smooth', block: 'start'});
});

subscriptionInput.addEventListener('input', () => {
  hideMessage();
  if (currentSubscriptionID && subscriptionInput.value.trim() !== currentSubscriptionID) {
    resetAccountView({clearSavedSubscription: true});
  }
});

switchAccountButton.addEventListener('click', () => {
  resetAccountView({clearInput: true, clearSavedSubscription: true});
  pendingReturnOrderNo = '';
  if (window.location.search) history.replaceState(null, '', window.location.pathname);
  hideMessage();
  subscriptionInput.focus();
  entryPanel.scrollIntoView({behavior: 'smooth', block: 'center'});
});

fetch('/api/public/config').then((response) => response.json()).then((body) => {
  if (body?.data?.timezone) appTimezone = body.data.timezone;
}).catch(() => {});

fetch('/api/public/client-reset/plans').then((response) => response.json()).then((body) => {
  if (body?.success) {
    if (Array.isArray(body.data?.items) && body.data.items.length) clientResetPlans = body.data.items;
    paymentEnabled = body.data?.paymentEnabled === true;
    if (currentLookupData) drawClientInfo(currentLookupData, currentResetResult);
  }
}).catch(() => {});

(async function restoreAccount() {
  const params = new URLSearchParams(window.location.search);
  pendingReturnOrderNo = params.get('payment_order') || params.get('o') || '';
  const savedSubscriptionID = loadActiveSubscriptionID();
  if (savedSubscriptionID) {
    subscriptionInput.value = savedSubscriptionID;
    await performLookup(savedSubscriptionID, pendingReturnOrderNo);
  } else if (pendingReturnOrderNo) {
    showMessage('请输入本次订单对应的订阅 ID，以查看付款及处理状态。', 'review');
  }
})();
