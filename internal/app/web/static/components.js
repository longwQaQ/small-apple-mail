export const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[ch]));
export const formatTime = value => {
  if (!value || String(value).startsWith('0001-01-01')) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat('zh-CN',{dateStyle:'medium',timeStyle:'short'}).format(date);
};
export const statusLabel = value => ({healthy:'正常',not_configured:'未配置',pending_credentials:'待凭据',pending_2fa:'等待 2FA',checking:'检测中',expiring:'即将过期',expired:'已过期',rate_limited:'已限流',failed:'失败',disabled:'已停用',paused:'已暂停',available:'可用',leased:'租用中',remote_missing:'远端缺失',missing:'远端缺失',unknown:'待确认',queued:'排队中',running:'运行中',succeeded:'已完成',partially_succeeded:'部分完成',cancelling:'取消中',cancelled:'已取消',interrupted:'已中断',active:'启用',consumed:'已消费'}[value] || value || '未知');
export const statusBadge = value => `<span class="status ${escapeHTML(value)}">${escapeHTML(statusLabel(value))}</span>`;

export function toast(message, type = '') {
  const dialog = document.querySelector('#app-dialog');
  const region = dialog?.open ? dialog : document.querySelector('#toast-region'); if (!region) return;
  const node = document.createElement('div'); node.className = `toast ${type}`; node.setAttribute('role', type === 'bad' ? 'alert' : 'status'); node.textContent = message; region.append(node);
  setTimeout(() => node.remove(), 4200);
}

export function setButtonBusy(button, busy, label = '') {
  if (!button) return;
  if (busy) {
    if (!button.hasAttribute('data-busy-was-disabled')) button.dataset.busyWasDisabled = String(Boolean(button.disabled));
    button.disabled = true; button.setAttribute('aria-busy', 'true');
  } else {
    button.disabled = button.dataset.busyWasDisabled === 'true'; button.removeAttribute('data-busy-was-disabled'); button.setAttribute('aria-busy', 'false');
  }
  const target = button.querySelector('[data-button-label]'); if (target && label) target.textContent = label;
  let spinner = button.querySelector('.spinner');
  if (busy && !spinner) { spinner = document.createElement('span'); spinner.className = 'spinner'; spinner.dataset.dynamicSpinner = ''; spinner.setAttribute('aria-hidden', 'true'); button.append(spinner); }
  if (spinner) spinner.hidden = !busy;
  if (!busy && spinner?.hasAttribute('data-dynamic-spinner')) spinner.remove();
}

export function emptyState(title, text, action = '') {
  return `<div class="empty-state"><div><svg viewBox="0 0 24 24"><path d="M4 5h16v14H4zM4 8l8 6 8-6"/></svg><h3>${escapeHTML(title)}</h3><p>${escapeHTML(text)}</p>${action}</div></div>`;
}

let restoreFocus = null;
export function openDialog({title, eyebrow = 'ACTION', content = '', actions = '', size = ''}) {
  const dialog = document.querySelector('#app-dialog'); restoreFocus = document.activeElement;
  if (size) dialog.dataset.size = size; else delete dialog.dataset.size;
  dialog.querySelector('#dialog-title').textContent = title; dialog.querySelector('.eyebrow').textContent = eyebrow;
  dialog.querySelector('#dialog-content').innerHTML = content; dialog.querySelector('#dialog-actions').innerHTML = actions;
  dialog.showModal(); requestAnimationFrame(() => dialog.querySelector('input,select,textarea,button:not([value=cancel])')?.focus());
}
export function closeDialog() { const dialog = document.querySelector('#app-dialog'); if (dialog.open) dialog.close(); restoreFocus?.focus?.(); restoreFocus = null; }

export function metric(icon, value, label, detail = '') { return `<article class="metric-card"><span class="metric-icon">${icon}</span><div><strong>${escapeHTML(value)}</strong><span>${escapeHTML(label)}</span>${detail ? `<small class="muted">${escapeHTML(detail)}</small>` : ''}</div></article>`; }

export const icons = {
  account:'<svg viewBox="0 0 24 24"><circle cx="9" cy="9" r="4"/><path d="M3 21a6 6 0 0 1 12 0M16 7h5M18.5 4.5v5"/></svg>',
  mail:'<svg viewBox="0 0 24 24"><path d="M4 5h16v14H4zM4 8l8 6 8-6"/></svg>',
  lease:'<svg viewBox="0 0 24 24"><circle cx="8" cy="15" r="4"/><path d="m11 12 8-8M15 4h4v4"/></svg>',
  job:'<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="4"/><path d="M12 2v4M12 18v4M2 12h4M18 12h4"/></svg>'
};
