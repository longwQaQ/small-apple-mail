import {closeDialog, emptyState, escapeHTML as esc, formatTime, openDialog, statusBadge, toast} from './components.js?v=1.0.2';

const selected = new Map();
let resultWait = null;
const views = [['all','全部邮箱'],['shared','临时分配'],['dedicated','长期专用'],['unused','尚未分配'],['results','已有收件结果'],['attention','远端缺失']];
const button = (action,label,attrs='',style='') => `<button class="button compact ${style}" type="button" data-action="${action}" ${attrs}>${label}</button>`;

export function mailboxPurposeFields(mailbox={}) {
  return `<label class="field"><span>用途类型</span><select name="allocation_mode"><option value="shared" ${mailbox.allocation_mode!=='dedicated'?'selected':''}>临时分配 · 可进入租约池</option><option value="dedicated" ${mailbox.allocation_mode==='dedicated'?'selected':''}>长期专用 · 不自动分配</option></select></label><label class="field"><span>用途说明</span><input name="purpose" maxlength="120" value="${esc(mailbox.purpose||'')}" placeholder="例如：Claude 日常账号"></label>`;
}

export function renderMailboxWorkspace({rows,accounts,overview,filters,page,next}) {
  const ids=new Set(rows.map(m=>m.id));for(const id of selected.keys())if(!ids.has(id))selected.delete(id);
  const stats=[['邮箱总数',overview.total],['可分配',overview.available],['长期专用',overview.dedicated],['租用中',overview.leased]];
  const current=filters.view||'all';
  const header=`<section class="pool-heading"><div><h2>邮箱管理</h2><p>管理用途、分配资格与收件结果</p></div><div class="toolbar">${button('sync-mailboxes','同步远端')}${button('create-mailboxes','创建邮箱','','primary')}</div></section><section class="pool-stats">${stats.map(([label,value])=>`<div><strong>${value||0}</strong><span>${label}</span></div>`).join('')}</section>`;
  const tabs=`<div class="pool-views" aria-label="常用邮箱视图">${views.map(([id,name])=>`<button type="button" data-action="mailbox-view" data-view="${id}" aria-pressed="${id===current}">${name}</button>`).join('')}<button type="button" data-action="mailbox-saved-view">我的筛选</button></div>`;
  const filter=`<form id="mailbox-filter-form" class="pool-filters"><input id="mailbox-search" type="search" aria-label="搜索邮箱" placeholder="搜索邮箱、标签或用途" value="${esc(filters.query||'')}"><select id="mailbox-account" aria-label="账号范围"><option value="">全部账号</option>${accounts.map(a=>`<option value="${esc(a.id)}" ${a.id===filters.account_id?'selected':''}>${esc(a.display_name||a.apple_id)}</option>`).join('')}</select><select id="mailbox-status" aria-label="邮箱状态"><option value="">全部状态</option>${[['available','可用'],['leased','租用中'],['disabled','暂停分配'],['remote_missing','远端缺失']].map(([id,name])=>`<option value="${id}" ${filters.status===id?'selected':''}>${name}</option>`).join('')}</select><input id="mailbox-tag" aria-label="精确标签" placeholder="按完整标签筛选" value="${esc(filters.tag||'')}"><button class="button" type="submit">筛选</button>${button('mailbox-save-view','保存筛选')}</form>`;
  const toolbar=`<div class="pool-toolbar"><label><input type="checkbox" id="mailbox-select-all" aria-label="选择本页全部邮箱" ${rows.length&&selected.size===rows.length?'checked':''}> 本页全选</label><span id="mailbox-selected-count">已选 ${selected.size} 项</span><div class="toolbar">${button('mailbox-batch-copy','复制地址')}${button('mailbox-batch-edit','批量管理')}${button('export-mailboxes','导出筛选结果')}${button('add-mailbox','登记已有邮箱')}</div></div>`;
  const accountNames=new Map(accounts.map(a=>[a.id,a.display_name||a.apple_id]));
  const list=rows.length?`<div class="pool-list"><div class="pool-column-head" aria-hidden="true"><span></span><span>邮箱 / 账号</span><span>用途 / 标签 / 备注</span><span>状态 / 时间</span><span>收件结果</span><span>操作</span></div>${rows.map(m=>mailboxRow(m,accountNames.get(m.account_id))).join('')}</div>`:emptyState('没有匹配的邮箱','调整筛选条件，或同步已有的 Hide My Email 地址。',button('mailbox-filter-reset','重置筛选'));
  return header+tabs+`<section class="pool-panel">${filter}${toolbar}${list}<div class="pool-pagination"><span>第 ${page+1} 页 · 本页 ${rows.length} 个邮箱</span><div>${button('mailbox-prev','上一页',page===0?'disabled':'')}${button('mailbox-next','下一页',`data-cursor="${esc(next)}" ${next?'':'disabled'}`)}</div></div></section>`;
}

function mailboxRow(m,account) {
  const attrs=`data-mailbox="${esc(m.id)}" data-email="${esc(m.email)}" data-account="${esc(m.account_id)}"`;
  const latest=m.last_login_url_at>m.last_code_at?m.last_login_url_at:m.last_code_at;
  const result=(m.last_code||m.last_login_url)?`<div class="pool-result">${m.last_code?button('copy-mailbox-code',esc(m.last_code),`data-code="${esc(m.last_code)}"`,'code-chip'):''}${m.last_login_url?button('copy-login-link','复制登录链接',`data-login-url="${esc(m.last_login_url)}"`):''}<small>历史结果 · ${formatTime(latest)}</small></div>`:'<span class="pool-empty-result">还没有收件结果</span>';
  return `<article class="pool-row"><label class="pool-check"><input type="checkbox" data-mailbox-select="${esc(m.id)}" data-email="${esc(m.email)}" aria-label="选择 ${esc(m.email)}" ${selected.has(m.id)?'checked':''}></label>
    <div class="pool-address"><div><strong>${esc(m.email)}</strong></div><p>${esc(account||'—')}</p><span class="purpose-pill ${m.allocation_mode==='dedicated'?'dedicated':''}">${m.allocation_mode==='dedicated'?'长期专用':'临时分配'}</span></div>
    <dl class="pool-details"><dt>用途</dt><dd>${esc(m.purpose||'未填写')}</dd><dt>标签</dt><dd>${esc(m.label||'未填写')}</dd><dt>标签组</dt><dd class="pool-tags">${m.tags?.length?m.tags.map(t=>`<span class="tag">${esc(t)}</span>`).join(''):'未设置'}</dd><dt>备注</dt><dd class="pool-note">${esc(m.note||'未填写')}</dd></dl>
    <div class="pool-state">${statusBadge(m.status)}<small>${m.remote_status==='active'?'远端转发已开启':m.remote_status==='disabled'?'远端转发已停用':'远端状态需核对'}</small><small class="pool-created">创建 ${formatTime(m.created_at)}</small><small class="pool-sync">同步 ${formatTime(m.last_synced_at)}</small></div>
    ${result}<div class="pool-actions">${button('wait-mailbox-result','等待新结果',attrs,'primary')}${button('copy-mailbox','复制邮箱',`data-email="${esc(m.email)}"`)}${button('mailbox-more','更多',attrs)}</div></article>`;
}

export function mailboxViewFilter(view) {
  return {view,allocation_mode:['shared','dedicated'].includes(view)?view:'',unused:view==='unused'?'true':'',with_result:view==='results'?'true':'',status:view==='attention'?'remote_missing':''};
}

function updateSelection() {
  const count=document.querySelector('#mailbox-selected-count');if(count)count.textContent=`已选 ${selected.size} 项`;
  const all=document.querySelector('#mailbox-select-all'),rows=[...document.querySelectorAll('[data-mailbox-select]')];if(all){all.checked=!!rows.length&&rows.every(n=>n.checked);all.indeterminate=selected.size>0&&!all.checked;}
}
document.addEventListener('change',event=>{
  if(event.target.id==='mailbox-select-all')for(const box of document.querySelectorAll('[data-mailbox-select]')){box.checked=event.target.checked;if(box.checked)selected.set(box.dataset.mailboxSelect,box.dataset.email);else selected.delete(box.dataset.mailboxSelect)}
  else if(event.target.matches('[data-mailbox-select]')){const box=event.target;if(box.checked)selected.set(box.dataset.mailboxSelect,box.dataset.email);else selected.delete(box.dataset.mailboxSelect)}else return;
  updateSelection();
});
document.querySelector('#app-dialog')?.addEventListener('close',()=>{resultWait?.abort();resultWait=null});

export async function handleMailboxAction(action,target,{api,refresh,filter,filters}) {
  if(action==='mailbox-view'){selected.clear();await filter({...filters,...mailboxViewFilter(target.dataset.view)});return true}
  if(action==='mailbox-save-view'){localStorage.setItem('sam.mailbox-filter',JSON.stringify(filters));toast('当前筛选已保存','good');return true}
  if(action==='mailbox-saved-view'){let saved;try{saved=JSON.parse(localStorage.getItem('sam.mailbox-filter')||'null')}catch{}if(!saved){toast('先设置筛选条件，再点击“保存筛选”');return true}selected.clear();await filter(saved);return true}
  if(action==='mailbox-batch-copy'){if(!selected.size)throw new Error('请先选择本页邮箱');await navigator.clipboard.writeText([...selected.values()].join('\n'));toast(`已复制 ${selected.size} 个邮箱地址`,'good');return true}
  if(action==='mailbox-batch-edit'){
    if(!selected.size)throw new Error('请先选择本页邮箱');openDialog({title:`管理选中的 ${selected.size} 个邮箱`,eyebrow:'BATCH UPDATE',content:`<form id="mailbox-batch-form" class="dialog-form"><label class="field"><span>要执行的操作</span><select name="operation"><option value="tags">替换标签组</option><option value="dedicated">设为长期专用</option><option value="shared">设为临时分配</option><option value="pause">暂停新的租约分配</option><option value="resume">恢复租约分配资格</option></select></label><label class="field"><span>标签组 / 用途说明</span><input name="value" maxlength="120" placeholder="标签用逗号分隔；调整用途时作为说明"></label><p class="muted">逐项处理并展示结果。调整用途时，有活动租约的邮箱会保留原配置。</p></form>`,actions:button('close-dialog','取消')+button('mailbox-batch-submit','应用修改','','primary')});return true;
  }
  if(action==='mailbox-batch-submit'){
    const form=document.querySelector('#mailbox-batch-form');if(!form.reportValidity())return true;const data=new FormData(form),operation=data.get('operation'),value=String(data.get('value')||'').trim();
    const patch=operation==='tags'?{tags:value.split(/[,，]/).map(s=>s.trim()).filter(Boolean)}:['dedicated','shared'].includes(operation)?{allocation_mode:operation,...(value?{purpose:value}:{})}:{local_enabled:operation==='resume'};
    const result=await api.batchMailboxes([...selected.keys()],patch),failures=result.items.filter(item=>!item.success);const names=new Map(selected);selected.clear();closeDialog();await refresh();
    if(failures.length)openDialog({title:'部分邮箱未完成修改',content:`<p>成功 ${result.items.length-failures.length} 项，失败 ${failures.length} 项。</p><div class="stack">${failures.map(item=>`<div class="inline-alert warning"><strong>${esc(names.get(item.id)||item.id)}</strong><p>${esc(item.error)}</p></div>`).join('')}</div>`,actions:button('close-dialog','知道了')});else toast(`已更新 ${result.items.length} 个邮箱`,'good');return true;
  }
  if(action==='mailbox-more'){
    const attrs=`data-mailbox="${esc(target.dataset.mailbox)}" data-email="${esc(target.dataset.email)}" data-account="${esc(target.dataset.account)}"`;
    openDialog({title:'邮箱管理',eyebrow:'MAILBOX',content:`<p class="mailbox-menu-address">${esc(target.dataset.email)}</p><div class="mailbox-menu">${button('view-mailbox-messages','查看历史邮件',attrs)}${button('edit-mailbox','用途与标签',attrs)}${button('prepare-mailbox-api','API 凭据',attrs)}${button('sync-mailbox-messages','同步新邮件',attrs)}${button('mailbox-backfill','补同步历史邮件',attrs)}${button('delete-mailbox','删除本地记录',attrs,'danger')}</div>`,actions:button('close-dialog','关闭')});return true;
  }
  if(action==='mailbox-backfill'){openDialog({title:'补同步历史邮件',eyebrow:'MAIL RECOVERY',content:`<form id="mailbox-backfill-form" data-account="${esc(target.dataset.account)}"><label class="field"><span>补同步范围</span><select name="days"><option value="7">最近 7 天</option><option value="30" selected>最近 30 天</option><option value="90">最近 90 天</option></select></label><p class="muted">会同步该 Apple 账号下已登记邮箱的邮件，已有邮件自动去重。本地留存期限仍按设置执行。</p></form>`,actions:button('close-dialog','取消')+button('mailbox-backfill-submit','开始补同步','','primary')});return true}
  if(action==='mailbox-backfill-submit'){const form=document.querySelector('#mailbox-backfill-form');await api.createJob('mailbox.message_sync',{account_id:form.dataset.account,lookback_days:Number(form.elements.days.value)});closeDialog();toast('补同步任务已创建，可在自动化中查看结果','good');return true}
  if(action==='wait-mailbox-result'){
    resultWait?.abort();openDialog({title:'等待新收件结果',eyebrow:'LIVE INBOX',content:`<form id="mailbox-result-form" class="dialog-form" data-mailbox="${esc(target.dataset.mailbox)}"><p class="mailbox-menu-address">${esc(target.dataset.email)}</p><div class="dialog-form-grid"><label class="field"><span>需要的结果</span><select name="kind"><option value="all">验证码或登录链接</option><option value="code">仅验证码</option><option value="link">仅登录链接</option></select></label><label class="field"><span>查询范围</span><select name="range"><option value="0">从点击开始等待起</option><option value="5" selected>最近 5 分钟及后续邮件</option><option value="30">最近 30 分钟及后续邮件</option></select></label><label class="field full"><span>发件人或主题关键词</span><input name="keyword" maxlength="120" placeholder="可选，例如 Claude"></label></div><div id="mailbox-wait-result" class="waiting-result"><strong>准备接收</strong><p>开始后最多等待 2 分钟，新结果到达会自动显示。</p></div><small class="muted">登录链接只从邮件内容提取，不加载或访问链接。</small></form>`,actions:button('close-dialog','关闭')+button('mailbox-result-start','开始等待','','primary')});return true;
  }
  if(action==='mailbox-result-start'){
    const form=document.querySelector('#mailbox-result-form');if(!form.reportValidity())return true;resultWait?.abort();const controller=new AbortController();resultWait=controller;
    const after=new Date(Date.now()-Number(form.elements.range.value)*60000).toISOString(),kind=form.elements.kind.value,keyword=form.elements.keyword.value;
    const output=form.querySelector('#mailbox-wait-result');output.innerHTML='<span class="spinner"></span><strong>正在等待新邮件</strong><p>保持此窗口打开，收到结果后会自动显示。</p>';
    for(const field of form.querySelectorAll('input,select'))field.disabled=true;
    try {for(let attempt=0;attempt<4;attempt++){
      const result=await api.mailboxResult(form.dataset.mailbox,{kind,keyword,after,wait_seconds:30},{signal:controller.signal});
      if(controller.signal.aborted||!form.isConnected)return true;
      if(result.message){const m=result.message;output.innerHTML=`<span class="status healthy">已收到结果</span><p>${esc(m.sender)} · ${formatTime(m.received_at)}</p><strong>${esc(m.subject)}</strong>${m.code&&kind!=='link'?`<div class="received-code"><b>${esc(m.code)}</b>${button('copy-mailbox-code','复制验证码',`data-code="${esc(m.code)}"`)}</div>`:''}${m.login_url&&kind!=='code'?`<label class="field"><span>登录链接</span><textarea readonly rows="3">${esc(m.login_url)}</textarea></label>${button('copy-login-link','复制登录链接',`data-login-url="${esc(m.login_url)}"`,'primary')}`:''}`;return true}
    }output.innerHTML='<strong>暂未收到匹配结果</strong><p>可以继续等待，或查看历史邮件。请确认收件连接正常。</p>';
    }catch(error){if(error.name!=='AbortError'){output.innerHTML='<strong>等待已中断</strong><p>检查连接后可以重新开始。</p>';throw error}}
    finally{if(form.isConnected)for(const field of form.querySelectorAll('input,select'))field.disabled=false;if(target.isConnected)target.textContent='再次等待'}return true;
  }
  return false;
}
