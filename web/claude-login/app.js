'use strict';
const el = id => document.getElementById(id);
let busy = false, deadline = 0, currentState = '', submitted = false, verified = false, egressConfirmed = false, tunnelActive = true, directMode = true;
function note(message, kind = '') { el('notice').textContent = message; el('notice').className = 'notice ' + kind; }
let prepared = false, published = false, generation = 0, contextPolling = false, targetKey = null;
function setBusy(value) {
  busy = value;
  document.querySelectorAll('button').forEach(button => button.disabled = value);
  const canAuthorize = prepared && egressConfirmed && tunnelActive && !verified && !published;
  el('startBtn').disabled = value || !canAuthorize;
  el('regenerateBtn').disabled = value || !canAuthorize;
  el('callbackBtn').disabled = value || !canAuthorize || !currentState || deadline <= Date.now() / 1000;
  el('publishBtn').disabled = value || !verified || published || !tunnelActive || !targetKey;
  el('prepareBtn').disabled = value || keyReading;
  el('copyHostCommand').disabled = value || !el('hostTrustCommand').value;
  el('oauthActionHint').textContent = value ? '正在处理，请稍候；若连接超时会保留信息并允许重试。' :
    published ? '账号已加入，无需重复授权。' : verified ? '授权已完成，无需重新生成链接，请确认加入账号池。' :
    !tunnelActive ? 'SSH 连接已断开，请点击“重新检查填写信息”恢复连接。' :
    !egressConfirmed ? '先核对浏览器出口 IP，一致后即可生成或重新生成链接。' :
    deadline && deadline <= Date.now() / 1000 ? '链接已过期，可点击“链接过期 / 重新生成”，无需重填信息。' :
    '链接过期可直接重新生成，邮箱、代理和 SSH 连接均保留。';
}
let selectedKeyContent = '', keyReading = false, keyReadVersion = 0;
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
async function fetchJson(url, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 15000);
  try {
    const response = await fetch(url, {...options, signal: controller.signal});
    const result = await response.json();
    if (!response.ok) throw apiError(result);
    return result;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('本机助手响应超时。填写信息已保留；已提交回调时先检查结果，再决定是否重新生成。');
    throw error;
  } finally { clearTimeout(timer); }
}
async function api(action, payload = {}) {
  const first = await fetchJson('/api/' + action, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)});
  const limit = Date.now() + 330000;
  for (;;) {
    await wait(500);
    const result = await fetchJson('/api/jobs/' + first.job);
    if (result.pending) {
      if (Date.now() > limit) throw new Error('操作仍未返回。请先检查结果，避免重复提交回调或发布。');
      continue;
    }
    if (!result.ok) throw apiError(result);
    return result.data;
  }
}

async function task(work) {
  if (busy) return;
  setBusy(true);
  try { await work(); } catch (error) { note(error.message, 'error'); if (error.code === 'ssh_host_untrusted') showHostHelp(error.details); else el('hostTrustHelp').hidden = true; } finally { setBusy(false); }
}
function apiError(result) {
  const error = new Error(result.error || '操作没有完成。');
  error.code = result.code; error.details = result.details || {};
  return error;
}
let observedHost = null;
function updateHostCommand() {
  const target = el('ssh_target').value.trim();
  const port = Number(document.querySelector('[name=ssh_port]').value);
  const valid = /^[A-Za-z_][A-Za-z0-9_.-]*@[A-Za-z0-9.-]+$/.test(target) && Number.isInteger(port) && port >= 1 && port <= 65535;
  let algorithm = '';
  const name = observedHost?.host_key_algorithm;
  if (name === 'ssh-rsa') algorithm = ' -o HostKeyAlgorithms=rsa-sha2-512,rsa-sha2-256,ssh-rsa';
  else if (['ssh-ed25519', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521'].includes(name)) algorithm = ' -o HostKeyAlgorithms=' + name;
  el('hostTrustCommand').value = valid ? 'ssh -p ' + port + ' -o StrictHostKeyChecking=ask -o UserKnownHostsFile="$env:USERPROFILE/.ssh/known_hosts"' + algorithm + ' ' + target : '';
  el('copyHostCommand').disabled = busy || !valid;
  el('hostObserved').hidden = !observedHost?.fingerprint;
  el('hostObserved').textContent = observedHost?.fingerprint ? '本次连接看到的主机密钥（' + observedHost.host_key_algorithm + '）：' + observedHost.fingerprint + '。请与服务器控制台或已核实的同种密钥指纹比对；此显示本身不代表已经信任。' : '';
}
function showHostHelp(details = {}) {
  observedHost = details;
  updateHostCommand(); el('hostTrustHelp').hidden = false; el('hostTrustHelp').open = true;
  el('hostTrustHelp').scrollIntoView({behavior:'smooth', block:'center'});
}
el('showHostHelp').addEventListener('click', () => showHostHelp(observedHost || {}));
for (const input of [el('ssh_target'), document.querySelector('[name=ssh_port]')]) input.addEventListener('input', () => { observedHost = null; updateHostCommand(); });
el('copyHostCommand').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText(el('hostTrustCommand').value); note('命令已复制，请在本机 Windows PowerShell 中执行。'); }
  catch { el('hostTrustCommand').focus(); el('hostTrustCommand').select(); note('请按 Ctrl+C 复制选中的命令，再在本机 PowerShell 执行。'); }
});
function checkCallback(text) {
  let url;
  try { url = new URL(text); } catch { throw new Error('请粘贴完整的 localhost 回调链接，不是单独的授权码。'); }
  if (!['localhost','127.0.0.1','[::1]'].includes(url.hostname)) throw new Error('回调地址应为 localhost，请复制授权完成后地址栏里的完整链接。');
  if (url.searchParams.get('state') !== currentState) throw new Error('这是旧链接或其他登录的回调，请使用本页最新授权链接对应的回调。');
  if (!url.searchParams.get('code')) throw new Error('回调中没有授权码，请确认已完成浏览器授权。');
  if (Date.now() / 1000 >= deadline) throw new Error('授权链接已过期，点击“重新生成”即可，不需要重填代理。');
}
function renderTarget(info) {
  if (!info || typeof info.masked !== 'string' || !/^[a-f0-9]{64}$/.test(info.fingerprint || '')) {
    targetKey = null; el('verifiedTargetKey').textContent = '正在读取服务器确认的目标 SK…';
    el('targetKeyIdentity').textContent = ''; el('targetPoolCount').textContent = '';
    return false;
  }
  targetKey = info;
  el('verifiedTargetKey').textContent = info.masked;
  el('targetKeyIdentity').textContent = (info.pool_label || '目标账号池') + ' · SK 尾号 ' + info.suffix + ' · 校验标识 ' + info.fingerprint.slice(0, 12);
  el('targetPoolCount').textContent = '服务器确认：当前池内 ' + info.account_count + ' 个账号';
  el('publishBtn').textContent = '确认加入 SK 尾号 ' + info.suffix + ' 的账号池';
  return true;
}
function showVerified(data) {
  renderTarget(data.target_key);
  verified = true; submitted = false;
  el('linkPanel').hidden = true;
  el('finished').hidden = false;
  el('verifiedEmail').textContent = data.email;
  el('verifiedProxy').textContent = data.proxy_host;
  el('callbackForm').hidden = true;
  el('step3').classList.add('active');
  el('timer').textContent = '授权已完成';
  note('登录已成功，账号邮箱与出口配置已核对。新号仍在独立目录，确认加入前不会接到业务请求。', 'success');
}
function applyStatus(data) {
  if (data.status === 'verified' || data.status === 'published') {
    showVerified(data);
    if (data.status === 'published') {
      published = true; el('publishBtn').hidden = true;
      el('publishResult').textContent = '目标 SK 账号数：' + data.original_account_count_before + ' → ' + data.original_account_count_after + '。配置已备份，服务器本机检查通过；请在客户端测试公网地址。';
      note('账号已加入目标 SK，服务检查正常。', 'success');
    }
    setBusy(busy); return;
  }
  if (typeof data.expires_in === 'number') deadline = Date.now() / 1000 + Math.max(0, data.expires_in);
  if (data.status === 'expired' || data.status === 'failed') {
    submitted = false;
    if (data.status === 'expired') deadline = 0;
    note(data.message, 'error'); setBusy(busy); return;
  }
  if (data.status === 'processing') {
    submitted = true;
    note('回调已提交，正在等待授权完成、停止独立实例并核对代理快照；请稍候，可检查结果。');
  }
  setBusy(busy);
}
el('prepareForm').addEventListener('submit', event => {
  event.preventDefault();
  task(async () => {
    if (currentState && !confirm('重新检查将关闭当前授权实例。已经提交回调时，请先检查登录结果。继续吗？')) return;
    generation++;
    note('正在通过 SSH 检查服务器和网络出口，请稍候……');
    const fields = Object.fromEntries(new FormData(event.target));
    if (fields.auth_mode === 'key' && selectedKeyContent) fields.key_content = selectedKeyContent;
    const result = await api('prepare', fields);
    el('hostTrustHelp').hidden = true;
    document.querySelector('[name=target_key]').value = '';
    delete fields.target_key; delete fields.ssh_password; delete fields.proxy_password; delete fields.key_content; clearSelectedKey();
    el('ssh_password').value = ''; el('proxy_password').value = '';
    deadline = 0; currentState = ''; submitted = false; verified = false; published = false; prepared = true; targetKey = null;
    el('verifiedTargetKey').textContent = '正在读取目标账号池…'; el('targetKeyIdentity').textContent = ''; el('targetPoolCount').textContent = ''; renderTarget(result.target_key);
    el('publishBtn').hidden = false; el('publishResult').textContent = '';
    egressConfirmed = false; renderNetwork(result);
    el('authorization').hidden = false; el('linkPanel').hidden = true; el('callbackForm').hidden = true; el('finished').hidden = true;
    el('setup').hidden = true;
    el('egress').textContent = result.egress_ip;
    el('proxyResult').textContent = '出口 ' + result.egress_ip + ' · Claude 连接正常';
    el('step2').classList.add('active');
    el('authorization').scrollIntoView({behavior:'smooth', block:'start'});
    note('连接检查通过。请按页面说明配置浏览器并核对实际出口，然后生成授权链接。', 'success');
  });
});
async function generate() {
  note('正在检查授权状态并生成新链接……');
  const result = await api('start');
  if (result.status) { applyStatus(result); return; }
  deadline = typeof result.expires_in === 'number' ? Date.now() / 1000 + result.expires_in : result.deadline;
  currentState = result.state; submitted = false;
  el('authUrl').value = result.url; el('redirectUrl').value = '';
  el('linkPanel').hidden = false; el('callbackForm').hidden = false;
  note('新链接已生成。请在五分钟内完成浏览器授权，再粘贴完整回调。');
}
el('startBtn').addEventListener('click', () => task(generate));
el('regenerateBtn').addEventListener('click', () => task(generate));
el('copyBtn').addEventListener('click', () => task(async () => {
  try { await navigator.clipboard.writeText(el('authUrl').value); note('已复制授权链接，请粘贴到已核对出口的浏览器。', 'success'); }
  catch { el('authUrl').focus(); el('authUrl').select(); note('请按 Ctrl+C 复制选中的授权链接。'); }
}));
el('callbackForm').addEventListener('submit', event => {
  event.preventDefault();
  task(async () => {
    checkCallback(el('redirectUrl').value.trim());
    note('正在提交回调，请勿关闭页面……');
    await api('callback', {redirect_url:el('redirectUrl').value.trim()});
    submitted = true; el('redirectUrl').value = '';
    applyStatus({status:'processing'});
    applyStatus(await api('status'));
  });
});
el('statusBtn').addEventListener('click', () => task(async () => applyStatus(await api('status'))));
el('publishBtn').addEventListener('click', () => task(async () => {
  const latest = await api('status');
  if (latest.status === 'published') { applyStatus(latest); return; }
  if (latest.status !== 'verified' || !renderTarget(latest.target_key)) throw new Error('无法确认当前目标 SK，请先检查登录结果。');
  if (!confirm('确认将 ' + latest.email + ' 加入 ' + targetKey.masked + '（SK 尾号 ' + targetKey.suffix + '，校验标识 ' + targetKey.fingerprint.slice(0,12) + '）？\n其他账号池保持原样，服务将短暂重启。')) return;
  note('正在备份配置并加入目标 SK 账号池，请勿重复操作……');
  const result = await api('publish');
  renderTarget(result.target_key);
  published = true; el('publishBtn').hidden = true;
  el('publishResult').textContent = '目标 SK 账号数：' + result.original_account_count_before + ' → ' + result.original_account_count_after + '。配置已备份，服务器本机检查通过；请在客户端测试公网地址。';
  note('添加完成。新号出口配置为 ' + result.proxy_host + '，原账号出口保持原样。', 'success');
}));
el('recheckBtn').addEventListener('click', () => { el('setup').hidden = false; el('setup').scrollIntoView({behavior:'smooth'}); el('email').focus(); });
el('parseProxy').addEventListener('click', () => {
  const parts = el('proxyPaste').value.trim().split(':');
  if (parts.length < 4 || !parts[0] || !parts[1] || !parts[2] || !parts.slice(3).join(':')) { note('请按 IP:端口:用户名:密码 的格式粘贴。', 'error'); return; }
  el('proxy_host').value = parts[0]; el('proxy_port').value = parts[1];
  el('proxy_user').value = parts[2]; el('proxy_password').value = parts.slice(3).join(':'); el('proxyPaste').value = '';
  note('代理信息已填入，请核对账号邮箱后检查连接。', 'success');
});
el('showPassword').addEventListener('click', () => { const show = el('proxy_password').type === 'password'; el('proxy_password').type = show ? 'text' : 'password'; el('showPassword').textContent = show ? '隐藏' : '显示'; });
async function restoreContext() {
  try {
    const data = await fetchJson('/api/context');
    if (!data.ready) return;
    prepared = true; generation++; renderTarget(data.proxy?.target_key);
    el('email').value = data.email; el('setup').hidden = true; el('authorization').hidden = false; el('step2').classList.add('active');
    egressConfirmed = data.confirmed; renderNetwork(data.proxy); tunnelActive = data.tunnel_active; setBusy(false);
    el('egress').textContent = data.proxy.egress_ip;
    el('proxyResult').textContent = '已检查出口 ' + data.proxy.egress_ip + ' · 可直接继续授权';
    if (data.oauth) { currentState = data.oauth.state; deadline = typeof data.oauth.expires_in === 'number' ? Date.now() / 1000 + data.oauth.expires_in : data.oauth.deadline; el('authUrl').value = data.oauth.url; el('linkPanel').hidden = false; el('callbackForm').hidden = false; }
    note('已恢复本次登录进度，可继续授权或检查结果；无需重新填写代理。', 'success');
    if (data.published) {
      applyStatus(data.published);
      if (!data.published.target_key) {
        await task(async () => applyStatus(await api('status')));
      }
      note('此账号已加入目标 SK，服务检查正常。', 'success');
    } else {
      task(async () => { const result = await api('status'); submitted = result.status === 'processing'; applyStatus(result); });
    }
  } catch { /* Initial setup remains available if there is no resumable context. */ }
}
function clearSelectedKey() {
  keyReadVersion++; keyReading = false; selectedKeyContent = ''; el('key_file').value = '';
  el('keyFileStatus').textContent = '支持 PEM / OpenSSH，最大 32 KiB'; el('clearKey').hidden = true; setBusy(busy);
}
async function selectKey(file) {
  if (busy) return;
  clearSelectedKey();
  if (!file) return;
  if (file.size > 32768 || file.size === 0) { note('请选择非空私钥文件，最大 32 KiB。', 'error'); return; }
  const version = keyReadVersion;
  keyReading = true; setBusy(busy); el('keyFileStatus').textContent = '正在读取私钥文件……';
  try {
    const content = await file.text();
    if (version !== keyReadVersion) return;
    if (!/-----BEGIN (?:RSA |EC |OPENSSH |ENCRYPTED )?PRIVATE KEY-----/.test(content)) {
      el('keyFileStatus').textContent = '文件无效，请重新选择';
      note('这不是 PEM / OpenSSH 私钥文件，请不要选择公钥或证书。', 'error'); return;
    }
    selectedKeyContent = content; el('key_path').value = '';
    el('keyFileStatus').textContent = '已选择：' + file.name; el('clearKey').hidden = false;
    note('私钥文件已选择，将在本机内存中用于 SSH 认证。', 'success');
  } finally {
    if (version === keyReadVersion) { keyReading = false; setBusy(busy); }
  }
}
el('chooseKey').addEventListener('click', () => el('key_file').click());
el('key_file').addEventListener('change', event => selectKey(event.target.files[0]).catch(() => note('无法读取文件，请重新选择。', 'error')));
el('clearKey').addEventListener('click', clearSelectedKey);
el('key_path').addEventListener('input', () => { if (selectedKeyContent) clearSelectedKey(); });
for (const type of ['dragover', 'dragleave', 'drop']) el('keyDrop').addEventListener(type, event => {
  event.preventDefault(); event.stopPropagation();
  el('keyDrop').classList.toggle('dragover', type === 'dragover');
  if (type === 'drop') {
    if (event.dataTransfer.files.length !== 1) { note('请一次只拖入一个私钥文件。', 'error'); return; }
    selectKey(event.dataTransfer.files[0]).catch(() => note('无法读取文件，请重新选择。', 'error'));
  }
});
// Dropping a file outside the target must not navigate away from this wizard.
document.addEventListener('dragover', event => { if (event.dataTransfer.types.includes('Files')) event.preventDefault(); });
document.addEventListener('drop', event => { if (event.dataTransfer.types.includes('Files')) event.preventDefault(); });
function toggleFields() {
  directMode = el('connection_mode').value === 'direct';
  document.querySelectorAll('[data-proxy]').forEach(label => {
    label.hidden = directMode;
    label.querySelectorAll('input,select').forEach(input => { input.disabled = directMode; });
  });
  el('quickImport').hidden = directMode;
  const password = el('auth_mode').value === 'password';
  el('sshPasswordLabel').hidden = !password; el('keyPathLabel').hidden = password;
  if (password) clearSelectedKey();
  el('ssh_password').required = password; el('ssh_password').disabled = !password; el('key_path').disabled = password;
}
function renderNetwork(result) {
  directMode = result.mode === 'direct';
  el('tunnelPanel').hidden = !directMode; el('residentialHint').hidden = directMode;
  if (result.tunnel) el('tunnelPort').textContent = result.tunnel.port;
  tunnelActive = result.tunnel ? result.tunnel.active : true;
  el('browserIp').value = '';
  el('tunnelStatus').textContent = '隧道已建立；仅供本机浏览器使用。';
  setBusy(busy);
}
el('connection_mode').addEventListener('change', toggleFields);
el('auth_mode').addEventListener('change', toggleFields);
el('confirmEgressBtn').addEventListener('click', () => task(async () => {
  await api('confirm-egress', {browser_ip: el('browserIp').value.trim()});
  egressConfirmed = true; note('已记录你核对的浏览器出口，可以开始授权。', 'success');
}));
el('disconnectBtn').addEventListener('click', () => task(async () => {
  await api('disconnect'); location.reload();
}));
toggleFields(); setBusy(false); restoreContext();
setInterval(async () => {
  if (busy || contextPolling || el('authorization').hidden || published) return;
  const activeGeneration = generation;
  contextPolling = true;
  try {
    const context = await fetchJson('/api/context');
    if (busy || generation !== activeGeneration) return;
    tunnelActive = context.tunnel_active;
    if (!tunnelActive) {
      egressConfirmed = false;
      el('tunnelStatus').textContent = 'SSH 已断开，请重新检查连接。';
      note('SSH 已断开，请暂停浏览器授权，重新检查连接后再继续。', 'error');
    }
    setBusy(busy);
  } catch {
    if (!busy && generation === activeGeneration) {
      tunnelActive = false; setBusy(busy);
      note('无法连接本机助手，请检查启动窗口后重试。', 'error');
    }
  } finally { contextPolling = false; }
}, 3000);
setInterval(() => {
  setBusy(busy);
  if (verified || published) return;
  if (!deadline) { el('timer').textContent = currentState ? '链接已过期 · 可重新生成' : '尚未开始计时'; return; }
  const seconds = Math.max(0, Math.ceil(deadline - Date.now() / 1000));
  el('timer').textContent = seconds ? '剩余 ' + Math.floor(seconds / 60) + ':' + String(seconds % 60).padStart(2,'0') : '链接已过期 · 可重新生成';
}, 500);
setInterval(() => { if (submitted && !verified && !busy) task(async () => applyStatus(await api('status'))); }, 4000);
