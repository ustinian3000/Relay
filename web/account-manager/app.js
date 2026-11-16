'use strict';
const $=id=>document.getElementById(id);
const escapeHtml=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const state={data:null,page:'overview',busy:false,plan:null,editor:null,wizard:{step:0,provider:'',flow:null},usage:null};
const labels={overview:['工作台','账号、入口和额度，一处查看。','先把关系看清楚，再做每一次变更。'],keys:['SK 管理','每个入口，都有明确的账号范围。','创建、启停与绑定调整，先预览再保存。'],accounts:['账号管理','账号归属和固定出口，一眼可见。','保持账号独占，未分配账号不对任何 SK 开放。'],usage:['额度查询','知道还剩多少，也知道何时恢复。','按 SK 查看实际返回的额度周期，不合并不同账号的百分比。']};
const providerName=id=>state.data?.catalog.find(p=>p.id===id)?.name||id;
const keyName=id=>state.data?.keys.find(k=>k.id===id)?.name||'待分配';
const dateText=value=>{if(!value)return '—';const d=new Date(value);return isNaN(d)?'—':new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Tokyo',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false}).format(d);};
function notice(message,error=false){$('notice').textContent=message;$('notice').hidden=false;$('notice').className='notice'+(error?' error':'');}
let connectionKeyContent='',connectionKeyReading=false,connectionKeyVersion=0,clearUploadedKey=false;
function setBusy(value){state.busy=value;document.querySelectorAll('button').forEach(b=>b.disabled=value);if(!value&&state.wizard.flow?.status!=='verified'&&state.wizard.step===3)$('wizardNext').disabled=true;$('saveConnectionBtn').disabled=value||connectionKeyReading;}
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function api(action,payload={}){
 const response=await fetch('/api/'+action,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});const first=await response.json();if(!response.ok)throw new Error(first.error||'操作失败。');
 for(;;){await pause(180);const result=await fetch('/api/jobs/'+first.job).then(r=>r.json());if(result.pending)continue;if(!result.ok)throw new Error(result.error||'操作未完成。');return result.data;}
}
async function task(work){if(state.busy)return;setBusy(true);try{await work();}catch(error){if($('wizardDialog').open){$('wizardError').textContent=error.message;$('wizardError').hidden=false;}else if($('connectionDialog').open){$('connectionError').textContent=error.message;$('connectionError').hidden=false;}else notice(error.message,true);}finally{setBusy(false);}}
function showPage(page){state.page=page;document.querySelectorAll('.page').forEach(p=>p.hidden=p.id!=='page-'+page);document.querySelectorAll('.sidebar [data-page]').forEach(b=>b.classList.toggle('selected',b.dataset.page===page));const l=labels[page];$('breadcrumb').textContent=l[0];$('pageTitle').textContent=l[1];$('pageSubtitle').textContent=l[2];}
function badge(enabled){return `<span class="badge ${enabled?'':'disabled'}">${enabled?'已启用':'已停用'}</span>`;}
function accountsFor(key){return state.data.accounts.filter(a=>a.key_id===key);}
async function refresh(){const old=$('usageKey').value;state.data=await fetch('/api/overview').then(r=>r.json());render();if(state.data.keys.some(k=>k.id===old))$('usageKey').value=old;}
function render(){
 const d=state.data,activeKeys=new Set(d.keys.filter(k=>k.enabled).map(k=>k.id));
 const metrics=[['SK 总数',d.keys.length,d.keys.filter(k=>k.enabled).length+' 个已启用','⌘'],['账号总数',d.accounts.length,new Set(d.accounts.map(a=>a.provider)).size+' 个平台','◉'],['已启用且已分配',d.accounts.filter(a=>a.enabled&&activeKeys.has(a.key_id)).length,'实际可用额度请在额度页确认','↗'],['待分配账号',d.accounts.filter(a=>!a.key_id).length,'尚不允许任何 SK 调用','◌']];
 $('metrics').innerHTML=metrics.map(([l,n,t,i])=>`<div class="metric"><span class="metric-icon">${i}</span><div class="metric-label">${l}</div><div class="metric-number">${n}</div><div class="metric-note">${t}</div></div>`).join('');
 $('sourceTag').textContent=d.source==='demo'?'示例数据':'现网只读快照 · 本地可编辑';$('revisionText').textContent='本地版本 '+d.revision+(d.synced_at?' · 同步于 '+dateText(d.synced_at)+' JST':' · 示例数据');
 $('poolCards').innerHTML=d.keys.map(k=>{const accounts=accountsFor(k.id),groups=[...new Set(accounts.map(a=>a.provider))];return `<article class="pool-card"><div class="card-head"><div><h3>${escapeHtml(k.name)}</h3><div class="key-mask">${escapeHtml(k.mask)}</div></div>${badge(k.enabled)}</div><div class="pool-stat"><strong>${accounts.length}</strong><span>个绑定账号 · ${accounts.filter(a=>a.enabled).length} 个启用</span></div><div class="account-chips">${groups.map(p=>`<span class="provider-chip">${escapeHtml(providerName(p))} · ${accounts.filter(a=>a.provider===p).length}</span>`).join('')||'<span class="hint">尚未绑定账号</span>'}</div><div class="card-actions"><button class="text-button" data-action="bindings" data-id="${k.id}">调整账号绑定</button><button class="text-button" data-action="usage" data-id="${k.id}">查看额度 →</button></div></article>`;}).join('')||'<div class="empty">还没有 SK，点击 SK 管理创建。</div>';
 $('keysBody').innerHTML=d.keys.map(k=>`<tr><td><strong>${escapeHtml(k.name)}</strong><small class="key-mask">${escapeHtml(k.mask)}</small></td><td>${badge(k.enabled)}</td><td>${accountsFor(k.id).length} 个账号</td><td><span class="hint">${k.id.startsWith('sk-')?'现网快照':'本地测试'}</span></td><td><div class="row-actions"><button class="text-button" data-action="rename" data-id="${k.id}">改名</button><button class="text-button" data-action="bindings" data-id="${k.id}">绑定</button><button class="text-button" data-action="toggle-key" data-id="${k.id}">${k.enabled?'停用':'启用'}</button><button class="text-button danger" data-action="delete-key" data-id="${k.id}">删除</button><button class="text-button" data-action="usage" data-id="${k.id}">额度</button></div></td></tr>`).join('')||'<tr><td colspan="5" class="empty">还没有 SK</td></tr>';
 $('pendingFlows').innerHTML=(d.flows||[]).map(f=>`<div class="local-note">未完成添加：${escapeHtml(f.email)} · ${escapeHtml(providerName(f.provider))} <button class="text-button" data-action="resume" data-id="${f.id}">继续处理 →</button></div>`).join('');
 $('events').innerHTML=d.events.slice(0,5).map(e=>`<div class="event">${escapeHtml(e.summary)}<time>${dateText(e.time)} JST · 仅本地</time></div>`).join('')||'<div class="empty">还没有变更。每次保存都会留下本地记录。</div>';
 const selected=$('providerFilter').value;$('providerFilter').innerHTML='<option value="">全部平台</option>'+[...new Set(d.accounts.map(a=>a.provider))].map(p=>`<option value="${escapeHtml(p)}">${escapeHtml(providerName(p))}</option>`).join('');$('providerFilter').value=selected;
 renderAccounts();$('usageKey').innerHTML='<option value="">选择一个 SK</option>'+d.keys.map(k=>`<option value="${k.id}">${escapeHtml(k.name)}${k.enabled?'':'（已停用）'}</option>`).join('');
 $('providerCards').innerHTML=d.catalog.filter(p=>p.category==='primary').map(p=>`<button type="button" class="provider-card ${state.wizard.provider===p.id?'selected':''}" data-provider="${p.id}"><strong>${escapeHtml(p.name)}</strong><span>OAuth 授权 / 凭据导入</span></button>`).join('');
 $('otherProvider').innerHTML='<option value="">请选择具体平台</option>'+d.catalog.filter(p=>p.category==='other').map(p=>`<option value="${p.id}">${escapeHtml(p.name)}</option>`).join('');
 $('sshTarget').value=d.ssh_target;
}
function renderAccounts(){const query=$('accountSearch').value.toLowerCase(),filter=$('providerFilter').value;const rows=state.data.accounts.filter(a=>(!filter||a.provider===filter)&&(!query||a.email.toLowerCase().includes(query)));
 $('accountsBody').innerHTML=rows.map(a=>`<tr><td><strong>${escapeHtml(a.email)}</strong><small>${escapeHtml(providerName(a.provider))}</small></td><td>${a.key_id?escapeHtml(keyName(a.key_id)):'<span class="badge warn">待分配</span>'}</td><td>${escapeHtml(a.proxy_host||'未设置')}${a.proxy_port?':'+a.proxy_port:''}<small>${a.proxy_mode==='inherited'?'继承全局代理':'账号专属代理'}</small></td><td>${badge(a.enabled)}</td><td><div class="row-actions"><button class="text-button" data-action="assign" data-id="${a.id}">调整所属 SK</button><button class="text-button" data-action="toggle-account" data-id="${a.id}">${a.enabled?'停用':'启用'}</button><button class="text-button" data-action="account-usage" data-id="${a.id}">额度</button></div></td></tr>`).join('')||'<tr><td colspan="5" class="empty">没有匹配账号</td></tr>';
}
function openEditor(kind,id){
 state.editor={kind,id};const key=state.data.keys.find(k=>k.id===id),account=state.data.accounts.find(a=>a.id===id);let html='';
 if(kind==='key_create'||kind==='rename'){ $('editorTitle').textContent=kind==='key_create'?'新建本地 SK':'修改 SK 名称';html=`<label>名称<input id="editorName" maxlength="60" value="${escapeHtml(key?.name||'')}" required autofocus></label><p class="hint">${kind==='key_create'?'新 Key 仅在创建成功时展示一次，不会自动在生产生效。':'改名只改变显示名称，Key 和绑定保持原样。'}</p>`;}
 if(kind==='bindings'){ $('editorTitle').textContent='调整「'+key.name+'」绑定';html=state.data.accounts.map(a=>{const occupied=a.key_id&&a.key_id!==id;return `<label class="account-option ${occupied?'occupied':''}"><input type="checkbox" name="account" value="${a.id}" ${a.key_id===id?'checked':''} ${occupied?'disabled':''}><span>${escapeHtml(a.email)}<br><span class="hint">${escapeHtml(providerName(a.provider))}</span></span><small>${occupied?'已属于 '+escapeHtml(keyName(a.key_id)):'可绑定'}</small></label>`;}).join('')||'<p>还没有可分配账号，请先添加账号。</p>';}
 if(kind==='assign'){ $('editorTitle').textContent='调整账号所属 SK';html=`<p>${escapeHtml(account.email)}</p><label>目标 SK<select id="editorKey"><option value="">暂不分配</option>${state.data.keys.map(k=>`<option value="${k.id}" ${account.key_id===k.id?'selected':''}>${escapeHtml(k.name)}${k.enabled?'':'（已停用）'}</option>`).join('')}</select></label><p class="hint">转移会先解除原所属关系。一个账号默认只绑定一个 SK。</p>`;}
 $('editorFields').innerHTML=html;$('editorDialog').showModal();
}
async function preview(kind,payload){const plan=await api('preview',{kind,payload});showPreview(plan);}
function showPreview(plan){state.plan=plan;$('previewSummary').textContent=plan.summary;$('previewCounts').textContent='SK：'+plan.before.keys+' → '+plan.after.keys+'　账号：'+plan.before.accounts+' → '+plan.after.accounts;
 $('previewDetails').textContent=plan.enrollment?'平台：'+providerName(plan.enrollment.provider)+'　账号：'+plan.enrollment.email+'　目标：'+plan.enrollment.target_name+'　代理：'+plan.enrollment.proxy_host:'';
 if(plan.changes?.length){const list=document.createElement('div');list.className='change-list';for(const change of plan.changes){const row=document.createElement('p');row.textContent=change.account+'：'+change.before+' → '+change.after+(change.enabled_before!==change.enabled_after?' · '+(change.enabled_after?'启用':'停用'):'');list.append(row);}$('previewDetails').append(list);}
 $('previewDialog').showModal();}
document.addEventListener('click',event=>{const target=event.target.closest('button');if(!target)return;
 if(target.dataset.close){$(target.dataset.close).close();return;}
 if(target.dataset.page){showPage(target.dataset.page);return;}
 if(target.dataset.provider){state.wizard.provider=target.dataset.provider;document.querySelectorAll('.provider-card').forEach(b=>b.classList.toggle('selected',b===target));$('otherProvider').value='';return;}
 const id=target.dataset.id,action=target.dataset.action;if(!action)return;
 if(action==='resume'){const flow=state.data.flows.find(f=>f.id===id);state.wizard={step:3,provider:flow.provider,flow};wizardStep(3);renderFlow();$('wizardDialog').showModal();return;}
 if(['rename','bindings','assign'].includes(action)){openEditor(action,id);return;}
 task(async()=>{
  const key=state.data.keys.find(k=>k.id===id),account=state.data.accounts.find(a=>a.id===id);
  if(action==='toggle-key')await preview('key_update',{id,enabled:!key.enabled});
  if(action==='delete-key')await preview('key_delete',{id});
  if(action==='toggle-account')await preview('account_toggle',{id,enabled:!account.enabled});
  if(action==='usage'){showPage('usage');$('usageKey').value=id;await loadUsage();}
  if(action==='account-usage'){if(!account.key_id)throw new Error('这个账号尚未分配给 SK，请先调整所属关系。');showPage('usage');$('usageKey').value=account.key_id;await loadUsage();}
 });
});
$('createKeyBtn').addEventListener('click',()=>openEditor('key_create'));
$('editorForm').addEventListener('submit',event=>{event.preventDefault();task(async()=>{const e=state.editor;let kind=e.kind,payload={id:e.id};if(e.kind==='key_create'){payload={name:$('editorName').value};}if(e.kind==='rename'){kind='key_update';payload.name=$('editorName').value;}if(e.kind==='bindings'){kind='bindings_set';payload.account_ids=[...document.querySelectorAll('#editorFields input[name=account]:checked')].map(n=>n.value);}if(e.kind==='assign'){kind='account_assign';payload.key_id=$('editorKey').value||null;}await preview(kind,payload);$('editorDialog').close();});});
$('commitBtn').addEventListener('click',()=>task(async()=>{const result=await api('commit',{plan_id:state.plan.id});$('previewDialog').close();if($('wizardDialog').open)$('wizardDialog').close();$('enrollProxyPassword').value='';$('credentialJson').value='';await refresh();state.usage=null;resetUsage();notice(result.summary+'。仅本地生效，线上服务未修改。');if(result.one_time_key){$('newSecret').value=result.one_time_key;$('secretDialog').showModal();}}));
$('copySecretBtn').addEventListener('click',()=>task(async()=>{try{await navigator.clipboard.writeText($('newSecret').value);$('copySecretBtn').textContent='已复制';}catch{$('newSecret').focus();$('newSecret').select();}}));
$('accountSearch').addEventListener('input',renderAccounts);$('providerFilter').addEventListener('change',renderAccounts);
function connectionKeyStatus(){
 const loaded=state.data?.ssh_key_loaded&&!clearUploadedKey;
 $('sshKeyStatus').textContent=connectionKeyContent?'已选择新私钥文件':loaded?'本次运行已加载私钥，可以继续查询；重启后需重新选择。':'支持 PEM / OpenSSH，最大 32 KiB';
 $('sshClearKey').hidden=!connectionKeyContent&&!loaded;
}
function resetConnectionFile(){
 connectionKeyVersion++;connectionKeyReading=false;connectionKeyContent='';$('sshKeyFile').value='';connectionKeyStatus();setBusy(state.busy);
}
async function selectConnectionKey(file){
 if(state.busy)return;
 resetConnectionFile();$('connectionError').hidden=true;
 if(!file)return;
 if(!file.size||file.size>32768)throw new Error('请选择非空私钥文件，最大 32 KiB。');
 const version=connectionKeyVersion;connectionKeyReading=true;setBusy(state.busy);$('sshKeyStatus').textContent='正在读取私钥文件……';
 try{
  const content=await file.text();if(version!==connectionKeyVersion)return;
  if(!/-----BEGIN (?:RSA |EC |OPENSSH |ENCRYPTED )?PRIVATE KEY-----/.test(content))throw new Error('这不是 PEM / OpenSSH 私钥，请不要选择公钥或证书。');
  connectionKeyContent=content;clearUploadedKey=false;$('sshKeyPath').value='';
  $('sshKeyStatus').textContent='已选择：'+file.name;$('sshClearKey').hidden=false;
 }finally{if(version===connectionKeyVersion){connectionKeyReading=false;setBusy(state.busy);if(!connectionKeyContent)connectionKeyStatus();}}
}
function connectionFileError(error){$('connectionError').textContent=error.message||'无法读取私钥文件，请重新选择。';$('connectionError').hidden=false;}
$('sshChooseKey').addEventListener('click',()=>$('sshKeyFile').click());
$('sshKeyFile').addEventListener('change',event=>selectConnectionKey(event.target.files[0]).catch(connectionFileError));
$('sshClearKey').addEventListener('click',()=>{clearUploadedKey=true;resetConnectionFile();});
$('sshKeyPath').addEventListener('input',()=>{if($('sshKeyPath').value){clearUploadedKey=true;resetConnectionFile();}});
for(const type of ['dragover','dragleave','drop'])$('sshKeyDrop').addEventListener(type,event=>{
 event.preventDefault();event.stopPropagation();$('sshKeyDrop').classList.toggle('dragover',type==='dragover');
 if(type==='drop'){
  if(event.dataTransfer.files.length!==1){connectionFileError(new Error('请一次只拖入一个私钥文件。'));return;}
  selectConnectionKey(event.dataTransfer.files[0]).catch(connectionFileError);
 }
});
for(const type of ['dragover','drop'])document.addEventListener(type,event=>{if(Array.from(event.dataTransfer.types).includes('Files'))event.preventDefault();});
function connectionFields(){const password=$('sshAuthMode').value==='password';$('sshKeyField').hidden=password;$('sshKeyPath').disabled=password;$('sshPasswordField').hidden=!password;$('sshPasswordHint').hidden=!password;$('sshPassword').disabled=!password;$('sshPassword').required=password;if(!password)$('sshPassword').value='';else resetConnectionFile();}
$('sshAuthMode').addEventListener('change',connectionFields);
$('connectionDialog').addEventListener('close',()=>{$('sshPassword').value='';resetConnectionFile();});
$('connectionBtn').addEventListener('click',()=>{$('sshTarget').value=state.data.ssh_target;$('sshAuthMode').value=state.data.ssh_auth_mode||'key';$('sshKeyPath').value=state.data.ssh_key_path||'';$('sshPassword').value='';clearUploadedKey=false;resetConnectionFile();$('connectionError').hidden=true;connectionFields();$('connectionDialog').showModal();});
$('connectionForm').addEventListener('submit',event=>{event.preventDefault();if(connectionKeyReading)return;task(async()=>{
 $('connectionError').hidden=true;
 const payload=Object.fromEntries(new FormData(event.target));
 if(payload.auth_mode==='key'&&connectionKeyContent)payload.key_content=connectionKeyContent;
 payload.clear_uploaded_key=clearUploadedKey;
 try{await api('connection',payload);}finally{delete payload.key_content;delete payload.ssh_password;}
 $('connectionDialog').close();await refresh();state.usage=null;resetUsage();notice('本次只读连接设置已保存。私钥仅保留在本次运行内存中，可同步账号或查询额度。');
});});
$('syncBtn').addEventListener('click',()=>task(async()=>{notice('正在只读获取现网账号关系……');const plan=await api('sync-preview');showPreview(plan);}));
$('exportBtn').addEventListener('click',()=>task(async()=>{const data=await fetch('/api/export').then(r=>r.json());const url=URL.createObjectURL(new Blob([JSON.stringify(data,null,2)],{type:'application/json'}));const a=document.createElement('a');a.href=url;a.download='本地SK账号绑定方案.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}));
function quotaCell(window,status){if(!window||typeof window.remainingPercent!=='number')return '<span class="hint">'+(status==='unsupported'?'不支持':'未知')+'</span>';const value=Math.max(0,Math.min(100,window.remainingPercent));const kind=value===0?'zero':value<20?'low':'';return `<div class="quota-value ${kind}">${value}%</div><progress class="quota-track ${kind}" value="${value}" max="100" aria-label="剩余 ${value}%"></progress>`;}
function resetInfo(row){if(row.error)return escapeHtml(row.error.message||row.error.code||'查询失败');const periods=[['5h',row.fiveHour],['周',row.weekly],['30 天',row.monthly]].filter(([,v])=>v&&v.resetsAt);return periods.map(([name,v])=>`${name}：${dateText(v.resetsAt)} JST`).join('<br>')||'<span class="hint">未返回恢复时间</span>';}
function resetUsage(){$('usageSummary').innerHTML='';$('usageBody').innerHTML='<tr><td colspan="5" class="empty">本地绑定已变化，请选择 SK 后刷新额度。</td></tr>';$('usageMeta').textContent='请选择 SK，点击刷新。查询沿用 60 秒缓存。';}
async function loadUsage(){const key=$('usageKey').value;if(!key)throw new Error('请先选择要查询的 SK。');$('usageMeta').textContent='正在查询所选 SK 的账号额度……';const data=await api('usage',{key_id:key});if($('usageKey').value!==key)return;state.usage=data;
 const s=data.summary;$('usageSummary').innerHTML=[['账号',s.total],['可用',s.available],['冷却',s.cooling],['未知',s.unknown]].map(([name,n])=>`<span>${name}<strong>${n}</strong></span>`).join('');
 $('usageMeta').textContent=(data.cached?'使用缓存':'本次查询')+' · 更新于 '+dateText(data.queried_at)+' JST · '+data.refresh_after_seconds+' 秒缓存';
 const statuses={ok:['正常',''],quota_exhausted:['额度耗尽','warn'],query_failed:['查询失败','error'],unsupported:['暂不支持','disabled'],disabled:['已停用','disabled']};
 $('usageBody').innerHTML=data.accounts.map(a=>{const status=!a.enabled?['已停用','disabled']:(statuses[a.status]||['未知','disabled']);return `<tr><td><strong>${escapeHtml(a.email)}</strong><small>${escapeHtml(providerName(a.provider))}　<span class="badge ${status[1]}">${status[0]}</span>${a.source==='local_demo'?' · 模拟额度':''}</small></td><td>${quotaCell(a.fiveHour,a.status)}</td><td>${quotaCell(a.weekly,a.status)}</td><td>${quotaCell(a.monthly,a.status)}</td><td class="quota-reset">${resetInfo(a)}</td></tr>`;}).join('')||'<tr><td colspan="5" class="empty">这个 SK 暂未绑定账号，额度查询为空。</td></tr>';
}
$('refreshUsageBtn').addEventListener('click',()=>task(loadUsage));$('usageKey').addEventListener('change',()=>{state.usage=null;resetUsage();});
function wizardStep(step){state.wizard.step=step;$('wizardError').hidden=true;for(let i=0;i<5;i++)$('wizardStep'+i).hidden=i!==step;document.querySelectorAll('.wizard-steps span').forEach((n,i)=>n.classList.toggle('active',i===step));$('wizardBack').hidden=step===0||step===3;$('wizardNext').textContent=step===2?'检查并开始 →':step===3?'核对并预览 →':step===4?'生成保存预览 →':'下一步 →';$('wizardNext').disabled=step===3&&state.wizard.flow?.status!=='verified';}
function targetValue(){const value=$('targetKey').value;if(value==='__unassigned__')return {mode:'unassigned'};if(value==='__new__')return {mode:'new',name:$('newTargetName').value};if(!value)throw new Error('请明确选择目标 SK、新建 SK 或暂不分配。');return {mode:'existing',key_id:value};}
function openWizard(){state.wizard={step:0,provider:'',flow:null};$('enrollEmail').value='';$('enrollProxyPassword').value='';$('enrollProxyUser').value='';$('credentialJson').value='';$('credentialFile').value='';$('enrollMethod').value='simulation';$('enrollMethod').dispatchEvent(new Event('change'));$('credentialImport').hidden=true;$('newTargetName').value='';$('targetKey').innerHTML='<option value="">请明确选择，不会自动使用第一组 SK</option>'+state.data.keys.map(k=>`<option value="${k.id}" ${k.enabled?'':'disabled'}>${escapeHtml(k.name)}${k.enabled?'':'（已停用）'}</option>`).join('')+'<option value="__new__">＋ 新建 SK</option><option value="__unassigned__">暂不分配</option>';$('newTargetNameLabel').hidden=true;renderProviderChoice();wizardStep(0);$('wizardDialog').showModal();}
function renderProviderChoice(){document.querySelectorAll('.provider-card').forEach(b=>b.classList.toggle('selected',b.dataset.provider===state.wizard.provider));$('otherProvider').value=state.data.catalog.find(p=>p.id===state.wizard.provider)?.category==='other'?state.wizard.provider:'';}
$('addAccountBtn').addEventListener('click',openWizard);$('otherProvider').addEventListener('change',()=>{state.wizard.provider=$('otherProvider').value;renderProviderChoice();});$('targetKey').addEventListener('change',()=>$('newTargetNameLabel').hidden=$('targetKey').value!=='__new__');
$('enrollMethod').addEventListener('change',()=>{const value=$('enrollMethod').value;$('credentialImport').hidden=value!=='import';$('methodHint').textContent=value==='simulation'?'模拟模式不会连接真实平台，也不会核实代理的真实出口；结果会明确标为模拟。':value==='real'?'将使用独立本地内核通过所填代理授权。凭据只保存本机，浏览器也应使用同一住宅出口。':'仅核对凭据格式、平台、邮箱与代理，不会据此宣称上游账号可用。凭据只保存在本机私密目录。';});
$('parseProxy').addEventListener('click',()=>{const p=$('proxyPaste').value.trim().split(':');if(p.length<4){$('wizardError').textContent='请按 IP:端口:用户名:密码 粘贴。';$('wizardError').hidden=false;return;}$('enrollProxyHost').value=p[0];$('enrollProxyPort').value=p[1];$('enrollProxyUser').value=p[2];$('enrollProxyPassword').value=p.slice(3).join(':');$('proxyPaste').value='';});
$('credentialFile').addEventListener('change',async()=>{const file=$('credentialFile').files[0];if(!file)return;if(file.size>1024*1024){$('wizardError').textContent='凭据文件超过 1 MiB，请检查文件类型。';$('wizardError').hidden=false;return;}$('credentialJson').value=await file.text();});
$('wizardBack').addEventListener('click',()=>wizardStep(Math.max(0,state.wizard.step-1)));
$('wizardNext').addEventListener('click',()=>task(async()=>{
 const step=state.wizard.step;if(step===0){if(!state.wizard.provider)throw new Error('第一步请选择具体账号平台。');wizardStep(1);return;}
 if(step===1){const target=targetValue();if(target.mode==='new'&&!target.name.trim())throw new Error('请输入新 SK 名称。');wizardStep(2);return;}
 if(step===2){const payload={provider:state.wizard.provider,email:$('enrollEmail').value,target:targetValue(),mode:$('enrollMethod').value,proxy:{host:$('enrollProxyHost').value,port:$('enrollProxyPort').value,scheme:$('enrollProxyScheme').value,username:$('enrollProxyUser').value,password:$('enrollProxyPassword').value}};let action='enroll-start';if(payload.mode==='import'){action='enroll-import';try{payload.credential=JSON.parse($('credentialJson').value);}catch{throw new Error('凭据不是有效 JSON。');}}state.wizard.flow=await api(action,payload);$('credentialJson').value='';$('callbackUrl').value='';wizardStep(3);renderFlow();return;}
 if(step===3){state.wizard.flow=await api('enroll-status',{id:state.wizard.flow.id});if(state.wizard.flow.status!=='verified')throw new Error('请先完成授权并核对结果。');$('wizardReview').innerHTML=flowDetails(state.wizard.flow);wizardStep(4);return;}
 if(step===4){const plan=await api('enroll-preview',{id:state.wizard.flow.id});showPreview(plan);}
}));
function flowDetails(f){return [['平台',providerName(f.provider)],['账号',f.email],['目标',f.target_name],['专属代理',f.proxy_host],['模式',f.mode==='simulation'?'本地模拟授权':f.mode==='import'?'导入本地凭据':'真实授权 · 仅本机']].map(([name,v])=>`<div><span>${name}</span><strong>${escapeHtml(v)}</strong></div>`).join('');}
function renderFlow(){const f=state.wizard.flow;if(!f)return;$('flowSummary').innerHTML=flowDetails(f);const messages={ready:'已准备好，可以开始授权。',waiting:f.mode==='simulation'?'打开本地模拟授权页，选择通过或失败；此过程不登录真实平台。':'请在配置同一住宅代理的浏览器中完成授权。',processing:'回调已提交，正在等待凭据；不要重复提交。',verified:f.mode==='import'?'凭据格式、平台、邮箱和代理一致。上游有效性尚未实测。':f.mode==='simulation'?'模拟授权已通过，可继续预览保存。':'本地凭据与账号、代理已核对，可继续预览保存。',expired:'链接已过期，点击重新生成，无需重填代理。',failed:'授权未完成。账号池没有变化，可以检查后重试。'};$('flowNotice').textContent=f.message||messages[f.status]||f.status;$('flowNotice').className='notice'+(['failed','expired'].includes(f.status)?' error':'');$('authLinkSection').hidden=f.status==='verified';$('authLink').value=f.url?new URL(f.url,location.origin).href:'';$('openSimulationBtn').hidden=f.mode!=='simulation';$('realCallback').hidden=f.mode!=='real'||f.device_flow;$('deviceCode').hidden=!f.user_code;$('deviceCode').textContent=f.user_code?'设备授权码：'+f.user_code:'';$('wizardNext').disabled=f.status!=='verified';}
async function pollFlow(){if(!state.wizard.flow)return;state.wizard.flow=await api('enroll-status',{id:state.wizard.flow.id});renderFlow();}
$('pollAuthBtn').addEventListener('click',()=>task(pollFlow));$('restartAuthBtn').addEventListener('click',()=>task(async()=>{state.wizard.flow=await api('enroll-restart',{id:state.wizard.flow.id});$('callbackUrl').value='';renderFlow();}));
$('copyAuthBtn').addEventListener('click',()=>task(async()=>{try{await navigator.clipboard.writeText($('authLink').value);$('copyAuthBtn').textContent='已复制';}catch{$('authLink').focus();$('authLink').select();}}));
$('openSimulationBtn').addEventListener('click',()=>{if(state.wizard.flow?.mode==='simulation')window.open($('authLink').value,'_blank','noopener');});
$('submitCallbackBtn').addEventListener('click',()=>task(async()=>{state.wizard.flow=await api('enroll-callback',{id:state.wizard.flow.id,redirect_url:$('callbackUrl').value});$('callbackUrl').value='';renderFlow();}));
$('closeWizard').addEventListener('click',()=>{if(state.wizard.flow&&state.wizard.flow.status==='verified'&&!confirm('核对结果尚未保存到本地账号池。关闭后仍不会自动绑定，确认关闭？'))return;$('wizardDialog').close();});
$('secretDialog').addEventListener('close',()=>{$('newSecret').value='';$('copySecretBtn').textContent='复制 Key';});
$('wizardDialog').addEventListener('close',()=>{for(const id of ['enrollProxyPassword','enrollProxyUser','proxyPaste','credentialJson','credentialFile','callbackUrl','authLink'])$(id).value='';refresh().catch(error=>notice(error.message,true));});
setInterval(()=>{const f=state.wizard.flow;if(!$('wizardDialog').open||!f||state.wizard.step!==3)return;if(f.status==='verified'){$('authTimer').textContent='已核对';return;}const n=Math.max(0,Math.ceil((f.deadline||0)-Date.now()/1000));$('authTimer').textContent=n?'剩余 '+Math.floor(n/60)+':'+String(n%60).padStart(2,'0'):'链接已过期';},500);
setInterval(()=>{if($('wizardDialog').open&&state.wizard.step===3&&state.wizard.flow&&!['verified','failed','expired'].includes(state.wizard.flow.status)&&!state.busy)task(pollFlow);},5000);
refresh().then(()=>showPage('overview')).catch(error=>notice(error.message,true));
