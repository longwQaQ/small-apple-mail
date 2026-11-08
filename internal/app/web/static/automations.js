import {closeDialog, emptyState, escapeHTML as esc, formatTime, openDialog, statusBadge, toast} from './components.js?v=1.0.2';

export const scheduleTypes=[['mailbox.remote_sync','同步远端邮箱'],['mailbox.message_sync','同步收件'],['mailbox.create','定时创建隐私邮箱'],['message.retention_cleanup','清理过期本地邮件'],['database.integrity_check','检查数据库'],['account.connection.check','检测账号连接'],['mailbox.remote_clean','将远端验证码邮件移至废纸篓'],['account.keepalive','维护备用创建通道（已有计划）']];
export const jobTypeLabel=value=>scheduleTypes.find(([id])=>id===value)?.[1]||value;
export function formatInterval(value){const n=Number(value)||0;return n%86400===0?`${n/86400} 天`:n%3600===0?`${n/3600} 小时`:n%60===0?`${n/60} 分钟`:`${n} 秒`}
export function schedulePayloadText(value){try{return JSON.stringify(JSON.parse(value||'{}'),null,2)}catch{return '{}'}}
const button=(action,label,attrs='',style='')=>`<button class="button compact ${style}" type="button" data-action="${action}" ${attrs}>${label}</button>`;
const descriptions={
 'mailbox.remote_sync':'同步 Apple 上的邮箱列表，更新远端状态；保留本地用途、标签和备注。',
 'mailbox.message_sync':'从上次同步进度继续接收邮件，提取验证码和登录链接。IMAP 已实时监听时，通常无需设置很短的间隔。',
 'mailbox.create':'每次按选定账号和数量创建新邮箱。遇到请求结果不明确时停止该账号，并提示核对远端。',
 'message.retention_cleanup':'按设置中的留存天数清理本地邮件记录，不操作远端收件箱。',
 'database.integrity_check':'检查数据库结构、数据完整性和外键约束。',
 'account.connection.check':'检查指定连接。网页连接已自动维护，一般无需额外设置高频检查。',
 'mailbox.remote_clean':'将已经记录的远端验证码邮件移至废纸篓。该计划会改变远端收件箱，请确认用途后启用。',
 'account.keepalive':'兼容已有备用创建通道维护计划。系统已内置自动维护。'
};

export function scheduleForm(schedule=null,accounts=[]) {
 const type=schedule?.type||'mailbox.remote_sync',seconds=Number(schedule?.interval_seconds||3600),unit=seconds%86400===0?86400:seconds%3600===0?3600:seconds%60===0?60:1;
 return `<form id="schedule-form" class="dialog-form dialog-form-wide" data-schedule="${esc(schedule?.id||'')}" data-original-type="${esc(type)}" data-original-payload="${esc(schedule?.payload||'{}')}" data-accounts="${esc(JSON.stringify(accounts.map(a=>({id:a.id,name:a.display_name||a.apple_id}))))}"><div class="schedule-form-fields"><label class="field required full"><span>计划名称</span><input name="name" required maxlength="80" value="${esc(schedule?.name||'')}" placeholder="例如：每小时同步邮箱列表"></label><label class="field required full"><span>任务模板</span><select name="type">${scheduleTypes.filter(([id])=>id!=='account.keepalive'||id===type).map(([id,name])=>`<option value="${id}" ${id===type?'selected':''}>${name}</option>`).join('')}</select></label><div class="field full"><span>执行频率</span><div class="schedule-interval"><input type="number" name="interval_value" value="${seconds/unit}" min="1" max="31536000" required aria-label="间隔数值"><select name="interval_unit" aria-label="间隔单位">${[[60,'分钟'],[3600,'小时'],[86400,'天'],[1,'秒']].map(([id,name])=>`<option value="${id}" ${id===unit?'selected':''}>${name}</option>`).join('')}</select></div><small id="schedule-next-preview">保存后按此间隔执行；最短 1 分钟。</small></div></div><div id="schedule-template-fields">${templateFields(type,accounts.map(a=>({id:a.id,name:a.display_name||a.apple_id})),JSON.parse(schedule?.payload||'{}'))}</div><div id="schedule-explanation" class="schedule-explanation">${descriptions[type]||''}</div><label class="toggle-field"><input name="enabled" type="checkbox" ${!schedule||schedule.enabled?'checked':''}><span><strong>启用此计划</strong><small>停用时保留配置；连续失败暂停后，保存启用即可恢复。</small></span></label></form>`;
}

function templateFields(type,accounts,payload={}) {
 if(['message.retention_cleanup','database.integrity_check'].includes(type))return '';
 const multi=['mailbox.remote_sync','mailbox.remote_clean','mailbox.create','mailbox.message_sync'].includes(type);
 const accountFields=multi?`<div class="field full"><span>执行账号</span><div class="schedule-account-select">${accounts.length?accounts.map(a=>`<label><input type="checkbox" name="account_ids" value="${esc(a.id)}" ${(payload.account_ids?.length?payload.account_ids.includes(a.id):payload.account_id?payload.account_id===a.id:true)?'checked':''}>${esc(a.name)}</label>`).join(''):'<p>请先添加账号连接。</p>'}</div></div>`:`<label class="field full"><span>执行账号</span><select name="account_id"><option value="">全部账号</option>${accounts.map(a=>`<option value="${esc(a.id)}" ${payload.account_id===a.id?'selected':''}>${esc(a.name)}</option>`).join('')}</select></label>`;
 let fields='';
 if(type==='mailbox.create')fields=`<label class="field"><span>每个账号每次创建</span><input name="count" type="number" required min="1" max="100" value="${Number(payload.count||1)}"></label><label class="field"><span>创建通道</span><select name="strategy">${[['auto','自动选择，优先 iCloud'],['icloud_web','仅 iCloud 邮箱管理'],['apple_account','仅备用创建通道']].map(([id,name])=>`<option value="${id}" ${payload.strategy===id?'selected':''}>${name}</option>`).join('')}</select></label><label class="field"><span>用途类型</span><select name="allocation_mode"><option value="shared">临时分配</option><option value="dedicated" ${payload.allocation_mode==='dedicated'?'selected':''}>长期专用</option></select></label><label class="field"><span>用途说明</span><input name="purpose" maxlength="120" value="${esc(payload.purpose||'')}"></label><label class="field"><span>显示标签</span><input name="label" maxlength="120" value="${esc(payload.label||'')}"></label><label class="field"><span>备注</span><input name="note" value="${esc(payload.note||'')}"></label>`;
 if(type==='account.connection.check')fields=`<label class="field full"><span>连接范围</span><select name="kind">${[['','全部已配置连接'],['icloud_web','iCloud 邮箱管理'],['imap','IMAP 收件'],['apple_account','备用创建通道']].map(([id,name])=>`<option value="${id}" ${payload.kind===id?'selected':''}>${name}</option>`).join('')}</select></label>`;
 if(type==='mailbox.message_sync'&&payload.lookback_days)fields=`<label class="field full"><span>每次补同步最近天数</span><input name="lookback_days" type="number" min="1" max="365" value="${Number(payload.lookback_days)}"><small>已有计划的补同步范围会保留；日常收件建议使用增量同步。</small></label>`;
 return `<div class="schedule-form-fields">${accountFields}${fields}</div>`;
}

export function scheduleInterval(form){const n=Number(form.elements.interval_value.value)*Number(form.elements.interval_unit.value);if(!Number.isSafeInteger(n)||n<60||n>31536000)throw new Error('执行间隔应为 1 分钟至 365 天');return n}
export function schedulePayloadFromForm(form){
 const data=new FormData(form),type=data.get('type');let payload={};if(type===form.dataset.originalType)try{payload=JSON.parse(form.dataset.originalPayload)||{}}catch{}
 if(['mailbox.remote_sync','mailbox.remote_clean','mailbox.create','mailbox.message_sync'].includes(type)){payload.account_ids=data.getAll('account_ids');if(!payload.account_ids.length)throw new Error('请至少选择一个执行账号');delete payload.account_id}
 else if(data.has('account_id')){payload.account_id=data.get('account_id');delete payload.account_ids}
 if(type==='mailbox.create'){Object.assign(payload,{count:Number(data.get('count')),strategy:data.get('strategy'),allocation_mode:data.get('allocation_mode'),purpose:data.get('purpose'),label:data.get('label'),note:data.get('note')})}
 if(type==='account.connection.check')payload.kind=data.get('kind');
 if(data.has('lookback_days'))payload.lookback_days=Number(data.get('lookback_days'));
 return payload;
}

document.addEventListener('change',event=>{
 const form=event.target.closest('#schedule-form');if(!form)return;
 if(event.target.name==='type'){const type=event.target.value,payload=type===form.dataset.originalType?JSON.parse(form.dataset.originalPayload):{};form.querySelector('#schedule-template-fields').innerHTML=templateFields(type,JSON.parse(form.dataset.accounts),payload);form.querySelector('#schedule-explanation').textContent=descriptions[type]||''}
 try{const seconds=scheduleInterval(form);form.querySelector('#schedule-next-preview').textContent=`每 ${formatInterval(seconds)} 执行 · 保存后首次预计 ${formatTime(new Date(Date.now()+seconds*1000).toISOString())}`}catch(error){form.querySelector('#schedule-next-preview').textContent=error.message}
});

export function renderAutomationWorkspace(jobs,schedules,page,next) {
 const plans=schedules.length?schedules.map(scheduleCard).join(''):emptyState('让重复操作按时完成','从同步、创建、清理和数据库检查模板开始。',button('add-schedule','创建第一个计划','','primary'));
 return `<section class="automation-heading"><div><h2>自动化工作台</h2><p>设置执行规则，逐项查看结果，集中处理需要关注的任务。</p></div><div class="toolbar">${button('integrity-check','运行数据库检查')}${button('add-schedule','新建计划','','primary')}</div></section><div class="automation-layout"><section><header class="automation-section-head"><div><h3>执行记录</h3><small>保留最近 24 小时 · 创建结果待核对时，请先同步远端</small></div>${button('clear-jobs','清空记录','','quiet danger')}</header><div class="stack">${jobs.length?jobs.map(jobCard).join(''):emptyState('最近没有执行记录','手动操作和定时计划的执行结果会显示在这里。')}</div><div class="job-pagination"><span>第 ${page+1} 页</span><div>${button('jobs-prev','上一页',page===0?'disabled':'')}${button('jobs-next','下一页',`data-cursor="${esc(next)}" ${next?'':'disabled'}`)}</div></div></section><aside><header class="automation-section-head"><div><h3>我的计划</h3><small>${schedules.filter(s=>s.enabled&&!s.paused_reason).length} 个正在启用</small></div></header><div class="stack">${plans}</div></aside></div>`;
}

function jobCard(job){return `<article class="job-card"><div class="row-between"><strong>${esc(jobTypeLabel(job.type))}</strong>${statusBadge(job.status)}</div><p class="job-time">${formatTime(job.created_at)} · 第 ${job.attempts||0} 次执行</p>${job.error?`<div class="inline-alert ${job.status==='partially_succeeded'?'warning':'danger'}">${esc(job.error)}</div>`:''}<progress class="progress" max="100" value="${Math.min(100,Math.max(0,job.progress))}" aria-label="任务进度 ${job.progress}%">${job.progress}%</progress><footer>${button('view-job','查看明细',`data-job="${esc(job.id)}"`)}${['queued','running'].includes(job.status)?button('cancel-job','取消任务',`data-job="${esc(job.id)}"`):''}</footer></article>`}
function scheduleCard(s){const paused=!!s.paused_reason;return `<article class="schedule-card"><header><div><strong>${esc(s.name)}</strong><p>${esc(jobTypeLabel(s.type))}</p></div>${statusBadge(!s.enabled?'disabled':paused?'paused':'active')}</header><dl class="schedule-meta"><div><dt>执行频率</dt><dd>每 ${formatInterval(s.interval_seconds)}</dd></div><div><dt>下次执行</dt><dd>${!s.enabled?'已停用':paused?'处理后恢复':formatTime(s.next_run_at)}</dd></div><div><dt>最近执行</dt><dd>${formatTime(s.last_run_at)}</dd></div></dl>${paused?`<div class="inline-alert warning">${esc(s.paused_reason)}</div>`:''}<footer class="schedule-actions">${button('edit-schedule','编辑',`data-schedule="${esc(s.id)}"`)}${button('run-schedule','立即运行',`data-schedule="${esc(s.id)}"`)}${button('toggle-schedule',s.enabled&&!paused?'停用':'启用',`data-schedule="${esc(s.id)}" data-enabled="${!s.enabled||paused}"`)}${button('delete-schedule','删除',`data-schedule="${esc(s.id)}" data-name="${esc(s.name)}"`,'quiet danger')}</footer></article>`}

export async function handleAutomationAction(action,target,{api,refresh}) {
 if(action==='cancel-job'){await api.cancelJob(target.dataset.job);toast('已请求取消任务');await refresh();return true}
 if(action==='retry-job'){await api.retryJob(target.dataset.job);closeDialog();toast('失败账号已重新加入队列','good');await refresh();return true}
 if(action!=='view-job')return false;
 const [job,items,events]=await Promise.all([api.job(target.dataset.job),api.jobItems(target.dataset.job),api.jobEvents(target.dataset.job)]);
 const labels={succeeded:'成功',failed:'失败',uncertain:'待核对',skipped:'跳过'};const counts=Object.keys(labels).map(status=>`<span>${labels[status]} <strong>${items.filter(i=>i.status===status).length}</strong></span>`).join('');
 const list=items.length?items.map(item=>`<article class="job-result-item"><header><strong>${esc(item.target)}</strong><span class="status ${item.status}">${labels[item.status]||esc(item.status)}</span></header><p>${esc(item.message)}</p></article>`).join(''):events.map(event=>`<article class="job-result-item"><strong>${esc(event.message)}</strong><p>${formatTime(event.created_at)}</p></article>`).join('');
 const retry=['mailbox.remote_sync','mailbox.message_sync'].includes(job.type)&&['failed','partially_succeeded'].includes(job.status)&&items.some(i=>i.status==='failed');
 openDialog({title:jobTypeLabel(job.type),eyebrow:'EXECUTION DETAILS',size:'wide',content:`<p>${statusBadge(job.status)} <span class="muted">${formatTime(job.created_at)}</span></p>${job.error?`<div class="inline-alert warning">${esc(job.error)}</div>`:''}${items.length?`<div class="job-result-summary">${counts}</div>`:''}<div class="job-result-list">${list||'<p>任务尚未开始执行。</p>'}</div>${items.some(i=>i.status==='uncertain')?'<p class="muted">待核对表示请求已发出但结果未确认。先同步远端邮箱，避免重复创建。</p>':''}`,actions:button('close-dialog','关闭')+button('view-job','刷新明细',`data-job="${esc(job.id)}"`)+(retry?button('retry-job','重试失败账号',`data-job="${esc(job.id)}"`,'primary'):'')});return true;
}
