import {emptyState, escapeHTML, formatTime, icons, statusBadge} from './components.js?v=1.0.2';

export const connectionNames = {icloud_web:'iCloud 邮箱管理', imap:'IMAP 收件', apple_account:'Apple 账号创建通道'};
const basicKinds = ['icloud_web', 'imap'];
const usableStatuses = ['healthy', 'expiring'];
const validTime = value => value && !String(value).startsWith('0001-');
const connectionMap = account => new Map((account.connections || []).map(connection => [connection.kind, connection]));
const basicReady = account => basicKinds.filter(kind => usableStatuses.includes(connectionMap(account).get(kind)?.status)).length;

export function renderAccountList(accounts, expanded = new Set()) {
  const heading = `<div class="accounts-heading"><div><h2>账号连接</h2><p>配置邮箱管理与收件即可开始使用，备用创建通道按需启用。</p></div><button class="button primary" type="button" data-action="add-account">添加账号</button></div>`;
  if (!accounts.length) return heading + emptyState('还没有 Apple 账号', '添加账号后，连接 iCloud 管理邮箱，再配置 IMAP 接收邮件。', '<button class="button primary" type="button" data-action="add-account">添加账号</button>');
  const ready = accounts.filter(account => basicReady(account) === 2).length;
  const issues = accounts.filter(account => ['failed', 'pending_2fa'].includes(account.status)).length;
  const receiving = accounts.filter(account => connectionMap(account).get('imap')?.status === 'healthy').length;
  const overview = `<section class="accounts-overview" aria-label="基础连接概览"><div><strong>${accounts.length}</strong><span>账号总数</span></div><div><strong>${ready}</strong><span>基础连接就绪</span></div><div><strong>${receiving}</strong><span>收件正常</span></div><div><strong>${issues}</strong><span>基础连接待处理</span></div></section>`;
  return heading + overview + `<div class="account-list">${accounts.map(account => accountCard(account, expanded.has(account.id))).join('')}</div>`;
}

function accountCard(account, expanded) {
  const connections = connectionMap(account);
  const name = account.display_name || account.apple_id;
  const optional = connections.get('apple_account');
  const enabled = optional && optional.status !== 'disabled';
  const optionalLabel = !enabled ? '未启用' : usableStatuses.includes(optional.status) ? '已启用' : optional.status === 'checking' ? '检测中' : '需要处理 · 可选';
  return `<article class="account-record" data-account-record="${escapeHTML(account.id)}">
    <header class="account-record-header"><div class="account-identity"><span class="account-avatar" aria-hidden="true">${escapeHTML(Array.from(name || 'A')[0].toUpperCase())}</span><div><div class="account-title-line"><h3>${escapeHTML(name)}</h3>${account.status !== 'not_configured' ? statusBadge(account.status) : ''}</div><p>${escapeHTML(account.apple_id)}</p>${account.note ? `<small>${escapeHTML(account.note)}</small>` : ''}</div></div>
    <div class="account-summary"><span>基础连接 <strong>${basicReady(account)} / 2</strong></span><span>创建于 ${formatTime(account.created_at)}</span><button class="button compact danger account-delete" type="button" data-action="delete-account" data-account="${escapeHTML(account.id)}" data-name="${escapeHTML(name)}" data-apple-id="${escapeHTML(account.apple_id)}">删除账号</button></div></header>
    <div class="connection-grid primary-connections">
      ${connectionCard(account, 'icloud_web', connections.get('icloud_web'))}
      ${connectionCard(account, 'imap', connections.get('imap'))}
    </div>
    <details class="optional-connection" data-optional-account="${escapeHTML(account.id)}" ${expanded ? 'open' : ''}>
      <summary><div><strong>备用创建通道 <span class="optional-label">可选</span></strong><p>需要另一条创建路径时再启用，不影响现有邮箱收件。</p></div><span class="optional-state">${optionalLabel}</span><span class="optional-chevron" aria-hidden="true">⌄</span></summary>
      <div class="optional-connection-body">${connectionCard(account, 'apple_account', optional)}</div>
    </details>
  </article>`;
}

function connectionCard(account, kind, connection) {
  const optional = kind === 'apple_account';
  const name = connectionNames[kind];
  const status = connection?.status || 'not_configured';
  const disabled = status === 'disabled';
  const descriptions = {
    icloud_web:'创建隐私邮箱、同步 Apple 上已有的邮箱。',
    imap:'接收邮件，提取验证码与 Claude 登录链接。',
    apple_account:'通过 Apple 账号管理会话创建邮箱，不负责同步或收件。'
  };
  const label = kind === 'icloud_web' ? '邮箱管理' : kind === 'imap' ? '邮件接收' : '备用创建';
  const detail = (connection?.status_message || (optional && !connection ? '默认不启用，需要时再配置。' : '尚未连接，配置后即可使用。')).replaceAll('新接口', '备用创建通道').replaceAll('旧接口', 'iCloud 邮箱管理');
  const next = validTime(connection?.next_check_at) ? formatTime(connection.next_check_at) : '';
  const timing = [];
  if (validTime(connection?.last_checked_at)) timing.push(`最近检测 ${formatTime(connection.last_checked_at)}`);
  if (connection && kind !== 'imap') {
    if (disabled) timing.push('已停止使用与自动维护，凭据保留');
    else if (['expired', 'pending_2fa'].includes(status)) timing.push('自动维护暂停，等待重新登录');
    else if (next) timing.push(`${['failed', 'rate_limited'].includes(status) ? '下次重试' : '下次维护'} ${next}`);
    else timing.push('自动维护待执行');
  }
  let configureLabel = !connection ? (optional ? '配置并启用' : '配置连接') : status === 'expired' ? '重新登录' : disabled ? '重新登录并启用' : '更新授权';
  if (kind === 'imap' && connection) configureLabel = '更新密码';
  const attrs = `data-account="${escapeHTML(account.id)}" data-kind="${kind}"`;
  const configure = `<button class="button compact ${!connection && !optional ? 'primary' : ''}" type="button" data-action="configure-connection" ${attrs} data-apple-id="${escapeHTML(account.apple_id)}">${configureLabel}</button>`;
  const toggle = optional && connection ? `<button class="button compact" type="button" data-action="toggle-optional-connection" ${attrs} data-enabled="${disabled}" aria-label="${disabled ? '启用' : '停用'}备用创建通道">${disabled ? '启用' : '停用'}</button>` : '';
  const check = connection && !disabled ? `<button class="icon-button" type="button" data-action="check-connection" ${attrs} aria-label="检测${name}" ${status === 'checking' ? 'disabled' : ''}>${icons.refresh || '<svg viewBox="0 0 24 24"><path d="M20 6v5h-5M4 18v-5h5M18.5 9A7 7 0 0 0 6 6.5L4 9M5.5 15A7 7 0 0 0 18 17.5l2-2.5"/></svg>'}</button>` : '';
  return `<section class="connection-cell has-detail ${escapeHTML(status)}" data-connection-kind="${kind}"><div class="connection-cell-head"><div><span class="connection-kind">${label}</span><strong>${name}</strong></div>${!connection && optional ? '<span class="optional-label">未启用</span>' : statusBadge(status)}</div><p class="connection-purpose">${descriptions[kind]}</p><p class="connection-status-message" title="${escapeHTML(detail)}">${escapeHTML(detail)}</p><footer><div class="connection-timing">${timing.map(text => `<small>${escapeHTML(text)}</small>`).join('')}</div><div class="connection-actions">${configure}${toggle}${check}</div></footer></section>`;
}

export function creationAccountChoices(accounts) {
  return accounts.map(account => {
    const available = (account.connections || []).filter(connection => ['icloud_web','apple_account'].includes(connection.kind) && usableStatuses.includes(connection.status)).map(connection => connection.kind);
    const description = available.includes('icloud_web') ? 'iCloud 邮箱管理可用' : available.includes('apple_account') ? '备用创建通道可用' : '请先配置可用的邮箱管理连接';
    return `<label class="choice-card"><input type="checkbox" name="account_ids" value="${escapeHTML(account.id)}" data-available-kinds="${available.join(',')}" ${available.length ? 'checked' : 'disabled'}><span class="choice-card-copy"><strong>${escapeHTML(account.display_name || account.apple_id)}</strong><small>${escapeHTML(account.apple_id)}</small><small>${description}</small></span></label>`;
  }).join('');
}

export function updateCreationChoices(form) {
  const strategy = form.elements.strategy.value;
  for (const input of form.querySelectorAll('input[name=account_ids]')) {
    const kinds = input.dataset.availableKinds.split(',').filter(Boolean);
    const available = strategy === 'auto' ? kinds.length > 0 : kinds.includes(strategy);
    if (!available) input.checked = false;
    input.disabled = !available;
  }
}
