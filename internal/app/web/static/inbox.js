import {emptyState, escapeHTML as esc, formatTime} from './components.js?v=1.0.2';

export function renderInboxWorkspace(data, accounts, filters) {
  return `<section class="inbox-workspace">
    <div class="panel-header"><div><h2>所有邮箱的来信</h2><p>按收件时间排列 · 邮件和正文按保留设置自动清理</p></div><button class="button primary" type="button" data-action="sync-inbox">同步收件</button></div>
    <form id="inbox-filter-form" class="inbox-filters">
      <input type="search" name="query" aria-label="搜索邮件" placeholder="搜索发件人、主题、收件邮箱或邮箱标签" value="${esc(filters.query)}">
      <select name="account_id" aria-label="收件账号"><option value="">全部账号</option>${accounts.map(a=>`<option value="${esc(a.id)}" ${a.id===filters.account_id?'selected':''}>${esc(a.display_name||a.apple_id)}</option>`).join('')}</select>
      <select name="kind" aria-label="邮件类型">${[['','全部邮件'],['code','包含验证码'],['link','包含登录链接'],['ordinary','普通邮件']].map(([value,label])=>`<option value="${value}" ${value===filters.kind?'selected':''}>${label}</option>`).join('')}</select>
      <button class="button" type="submit">筛选</button><button class="button" type="button" data-action="reset-inbox">重置</button>
    </form>
    <p class="inbox-count">匹配 ${data.total} 封 · 第 ${data.page} / ${data.pages} 页 · 每页 ${data.page_size} 封</p>
    ${data.items.length?`<div class="inbox-table-wrap"><table class="inbox-table"><thead><tr><th scope="col">邮件 / 发件人</th><th scope="col">收件邮箱 / 账号</th><th scope="col">收件时间</th><th scope="col">提取结果</th><th scope="col">操作</th></tr></thead><tbody>${data.items.map(m=>`<tr class="inbox-message">
      <td class="inbox-mail"><strong title="${esc(m.subject||'无主题')}">${esc(m.subject||'无主题')}</strong><small>${esc(m.sender||'未知发件人')}</small></td>
      <td class="inbox-recipient"><span>${esc(m.mailbox_email)}</span><small>${esc(m.account_name)}</small></td>
      <td class="inbox-time"><time>${formatTime(m.received_at)}</time></td>
      <td class="inbox-message-results">${m.code?`<button class="button compact code-chip" type="button" data-action="copy-mailbox-code" data-code="${esc(m.code)}" aria-label="复制验证码 ${esc(m.code)}">${esc(m.code)} · 复制</button>`:''}${m.login_url?`<button class="button compact" type="button" data-action="copy-login-link" data-login-url="${esc(m.login_url)}">复制登录链接</button>`:''}${!m.code&&!m.login_url?'<span class="muted">普通邮件</span>':''}</td>
      <td class="inbox-operation"><button class="button compact" type="button" data-action="view-inbox-message" data-mailbox="${esc(m.mailbox_id)}" data-message="${esc(m.id)}" data-email="${esc(m.mailbox_email)}">查看正文</button></td>
    </tr>`).join('')}</tbody></table></div>`:emptyState('没有匹配的邮件','可以调整筛选条件，或同步已登记邮箱的邮件。')}
    <div class="inbox-pagination"><span>共 ${data.total} 封</span><div><button class="button" type="button" data-action="global-inbox-prev" ${data.page<=1?'disabled':''}>上一页</button><button class="button" type="button" data-action="global-inbox-next" ${data.page>=data.pages?'disabled':''}>下一页</button></div></div>
  </section>`;
}
