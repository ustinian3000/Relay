"""Read-only production inspection and isolated local OAuth clients."""
import hashlib
import json
import os
import pathlib
import sys
import re
import secrets
import shlex
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from ssh_keys import PrivateKeyError, load_private_key
from domain import CATALOG, Problem, now_iso, provider_id

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
READ_PROGRAM = r'''
import pathlib,json,yaml,hashlib,sys,urllib.request,urllib.parse,datetime
r=json.load(sys.stdin)
config=yaml.safe_load((pathlib.Path.home() / 'tools' / 'cliproxyapi' / 'config.yaml').read_text())
bindings=config['plugins']['configs']['key-account-bind']['bindings']
auth=pathlib.Path(config['oauth']['auth-dir'])
def fingerprint(v):return hashlib.sha256(v.encode()).hexdigest()
def account_id(name):return 'acc-'+fingerprint(name)[:20]
def key_id(key):return 'sk-'+fingerprint(key)[:20]
if r['action']=='snapshot':
 keys=[];accounts=[];owners={}
 for i,b in enumerate(bindings):
  key=b['key'];identifier=key_id(key)
  keys.append({'id':identifier,'name':'SK '+str(i+1),'mask':key[:3]+'••••'+key[-4:],'enabled':True,'key_hash':fingerprint(key),'origin_ref':fingerprint(key)})
  for name in b['allow']:
   if any(x in name for x in '*?['):raise ValueError('wildcard bindings require explicit review')
   if name in owners:raise ValueError('shared account binding requires explicit review')
   owners[name]=identifier
 global_proxy=config.get('requests',{}).get('proxy-url','')
 for path in auth.glob('*.json'):
  d=json.loads(path.read_text());dedicated=d.get('proxy_url','');proxy=urllib.parse.urlsplit(dedicated or global_proxy)
  accounts.append({'id':account_id(path.name),'provider':d.get('type','unknown'),'email':d.get('email') or path.stem,'enabled':not d.get('disabled',False),'key_id':owners.get(path.name),'proxy_host':proxy.hostname,'proxy_port':proxy.port,'proxy_mode':'dedicated' if dedicated else 'inherited','origin_ref':fingerprint(path.name),'verified':bool(d.get('access_token') or d.get('api_key')),'credential_ref':None})
 result={'keys':keys,'accounts':accounts,'synced_at':datetime.datetime.now(datetime.timezone.utc).isoformat()}
elif r['action']=='usage':
 b=next((b for b in bindings if fingerprint(b['key'])==r['key_ref']),None)
 if b is None:raise ValueError('key no longer exists')
 request=urllib.request.Request('http://127.0.0.1:8317/v1/account-usage',headers={'Authorization':'Bearer '+b['key']})
 with urllib.request.urlopen(request,timeout=50) as response:result=json.load(response)
 scope=fingerprint('cli-proxy-api:caller-scope:v1'+chr(0)+b['key'])
 mapping={'account-'+fingerprint('account-usage:v1'+chr(0)+scope+chr(0)+name)[:16]:account_id(name) for name in b['allow']}
 for row in result['accounts']:
  if row['id'] not in mapping:raise ValueError('usage response escaped authorized pool')
  row['account_id']=mapping[row['id']]
else:raise ValueError('read-only action required')
print(json.dumps(result))
'''


class ReadOnlyProduction:
    def __init__(self):
        self.target = ''
        self.key_path = ''
        self.auth_mode = 'key'
        self._password = ''
        self._private_key = None

    def configure(self, payload):
        content = payload.pop('key_content', '')
        target = str(payload.get('ssh_target', '')).strip()
        if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_.-]*@[A-Za-z0-9.-]+', target):
            raise Problem('SSH 地址应为 ubuntu@服务器IP。')
        mode = payload.get('auth_mode', 'key')
        if mode not in ('key', 'password'):
            raise Problem('请选择私钥文件或 SSH 密码登录。')
        key = str(payload.get('key_path', '')).strip() if mode == 'key' else ''
        password = payload.get('ssh_password', '') if mode == 'password' else ''
        if not isinstance(password, str) or (mode == 'password' and (not password or len(password) > 4096 or '\x00' in password)):
            raise Problem('请输入有效的 SSH 登录密码。')
        if not isinstance(content, str) or len(content.encode('utf-8')) > 32768:
            raise Problem('私钥文件无效或超过 32 KiB。')
        if mode == 'password':
            self.ssh_library()
            content = ''
        if key and content:
            raise Problem('请选择私钥文件或填写路径，两种方式只用一种。')
        if key:
            path = pathlib.Path(key).expanduser().resolve()
            if not path.is_file():
                raise Problem('PEM 私钥路径不存在。')
            key = str(path)
        private_key = None
        if content:
            try:
                private_key = load_private_key(content)
            except PrivateKeyError as error:
                raise Problem(str(error)) from None
        elif mode == 'key' and not key and target == self.target and not payload.get('clear_uploaded_key'):
            private_key = self._private_key
        self.target, self.key_path, self.auth_mode, self._password = target, key, mode, password
        self._private_key = private_key

    def close(self):
        self._password = ''
        self._private_key = None

    @staticmethod
    def ssh_library():
        try:
            import paramiko
            return paramiko
        except ImportError:
            raise Problem('SSH 连接依赖未安装，请运行 python -m pip install -r requirements.txt。') from None

    def _paramiko_call(self, request):
        paramiko = self.ssh_library()
        client = paramiko.SSHClient()
        class VerifyHost(paramiko.MissingHostKeyPolicy):
            def missing_host_key(self, client, hostname, key):
                raise Problem('此服务器尚未验证主机密钥，请先在终端执行 ssh 用户名@服务器IP，核对主机指纹后连接一次。', 502)
        try:
            client.load_system_host_keys()
            known = pathlib.Path.home() / '.ssh' / 'known_hosts'
            if known.is_file():
                client.load_host_keys(str(known))
            client.set_missing_host_key_policy(VerifyHost())
            username, hostname = self.target.split('@', 1)
            client.connect(hostname, username=username,
                password=self._password if self.auth_mode == 'password' else None,
                pkey=self._private_key if self.auth_mode == 'key' else None,
                allow_agent=False, look_for_keys=False, timeout=12, auth_timeout=15, banner_timeout=12)
            stdin, stdout, stderr = client.exec_command('python3 -c ' + shlex.quote(READ_PROGRAM), timeout=20)
            stdin.write(json.dumps(request))
            stdin.flush()
            stdin.channel.shutdown_write()
            channel, chunks, size = stdout.channel, [], 0
            deadline = time.monotonic() + 75
            while True:
                if time.monotonic() >= deadline:
                    raise Problem('服务器只读查询超时，请稍后重试。', 502)
                if channel.recv_ready():
                    chunk = channel.recv(65536)
                    chunks.append(chunk)
                    size += len(chunk)
                if channel.recv_stderr_ready():
                    size += len(channel.recv_stderr(65536))
                if size > 8 * 1024 * 1024:
                    raise Problem('服务器只读结果过大，请检查服务状态。', 502)
                if channel.exit_status_ready() and not channel.recv_ready() and not channel.recv_stderr_ready():
                    break
                time.sleep(.01)
            if channel.recv_exit_status() != 0:
                raise Problem('SSH 已连接，但只读检查失败，请核对服务器部署路径和账号权限。', 502)
            return b''.join(chunks)
        except paramiko.AuthenticationException:
            raise Problem('SSH 认证失败，请检查用户名、密码或私钥，以及服务器允许的登录方式。', 502) from None
        except paramiko.BadHostKeyException:
            raise Problem('服务器主机密钥与本机记录不一致，请核对服务器身份后再连接。', 502) from None
        except (paramiko.SSHException, OSError):
            raise Problem('SSH 连接失败，请检查服务器地址、22 端口和网络。', 502) from None
        finally:
            client.close()

    def call(self, request):
        raw = self._paramiko_call(request) if self.auth_mode == 'password' or self._private_key is not None else self._key_call(request)
        try:
            return json.loads(raw)
        except ValueError:
            raise Problem('服务器未返回有效的只读结果。', 502) from None

    def _key_call(self, request):
        options = ['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=12', '-o', 'StrictHostKeyChecking=yes']
        if self.key_path:
            options += ['-i', str(pathlib.Path(self.key_path).expanduser())]
        try:
            result = subprocess.run(['ssh', *options, self.target, 'python3 -c ' + shlex.quote(READ_PROGRAM)],
                input=json.dumps(request).encode(), capture_output=True, timeout=75,
                creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        except (OSError, subprocess.TimeoutExpired):
            raise Problem('只读服务器连接未完成，请检查 SSH 和网络。', 502) from None
        if result.returncode:
            raise Problem('服务器只读检查失败，请核对 SSH 权限、账号绑定或接口状态。', 502)
        return result.stdout

    def snapshot(self):
        return self.call({'action': 'snapshot'})

    def usage(self, key_ref):
        return self.call({'action': 'usage', 'key_ref': key_ref})


class Quotas:
    def __init__(self, store, adapter, clock=time.time):
        self.store, self.adapter, self.clock = store, adapter, clock
        self.cache = {}
        self.lock = threading.Lock()

    def query(self, key_id):
        with self.lock, self.store.lock:
            key = next((k for k in self.store.state['keys'] if k['id'] == key_id), None)
            if not key:
                raise Problem('请选择有效的 SK。', 404)
            accounts = [dict(a) for a in self.store.state['accounts'] if a.get('key_id') == key_id]
            signature = key_id, key['enabled'], tuple(sorted((a['id'], a['enabled']) for a in accounts))
            cached = self.cache.get(signature)
            if cached and self.clock() - cached['time'] < 60:
                return {**cached['result'], 'cached': True, 'refresh_after_seconds': max(0, int(60 - (self.clock() - cached['time'])))}
            live = {}
            fetch_error = False
            if key.get('origin_ref') and any(a.get('origin_ref') for a in accounts):
                try:
                    result = self.adapter.usage(key['origin_ref'])
                    live = {r['account_id']: r for r in result['accounts']}
                except Problem:
                    fetch_error = True
            rows = []
            for account in accounts:
                provider = next((p for p in CATALOG if p['id'] == account['provider']), None)
                if not provider or not provider['quota']:
                    quota = {'status': 'unsupported', 'available': None, 'error': {'message': '此平台暂不支持额度查询。'}}
                elif account.get('demo_quota'):
                    quota = dict(account['demo_quota'])
                elif account['id'] in live:
                    quota = {k: v for k, v in live[account['id']].items() if k not in ('id', 'account_id')}
                else:
                    message = '额度服务暂时无法连接，请稍后重试。' if fetch_error else '本地测试账号尚未在线上绑定到这个 SK，无法查询真实额度。'
                    quota = {'status': 'query_failed', 'available': None, 'error': {'message': message}}
                rows.append({'account_id': account['id'], 'email': account['email'], 'provider': account['provider'],
                    'enabled': account['enabled'], 'fiveHour': None, 'weekly': None, 'monthly': None, 'queriedAt': now_iso(), **quota})
            result = {'key_id': key_id, 'accounts': rows, 'queried_at': now_iso(), 'cached': False,
                'key_enabled': key['enabled'],
                'refresh_after_seconds': 60, 'summary': {'total': len(rows), 'available': sum(r.get('available') is True and r['enabled'] and key['enabled'] for r in rows),
                'cooling': sum(r.get('status') == 'quota_exhausted' for r in rows), 'unknown': sum(r.get('available') is None for r in rows)}}
            self.cache[signature] = {'time': self.clock(), 'result': result}
            return result


class OAuthWorker:
    """Run the existing core on loopback with a private local auth directory."""
    def __init__(self, root, binary, provider, proxy, email, api=None):
        self.root = pathlib.Path(root)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        (self.root / 'auth').mkdir(mode=0o700, exist_ok=True)
        self.provider, self.proxy, self.email = provider_id(provider), proxy, email
        self.key = secrets.token_urlsafe(32)
        self.process = None
        self.state = None
        self.deadline = 0
        self.submitted = False
        self.mock_api = api
        if api:
            self.port = 0
            return
        if not pathlib.Path(binary).is_file():
            raise Problem('本地测试内核不存在，请检查随附的 Windows 内核路径。')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        config = {'config-version': 8, 'server': {'host': '127.0.0.1', 'port': self.port},
            'management': {'allow-remote': False, 'secret-key': self.key, 'disable-control-panel': True, 'disable-auto-update-panel': True},
            'access': {'api-keys': ['private-local-' + secrets.token_hex(24)]}, 'oauth': {'auth-dir': str(self.root / 'auth')},
            'requests': {'proxy-url': proxy}, 'plugins': {'enabled': False},
            'observability': {'logs': {'request-log': False, 'logging-to-file': False, 'logs-max-total-size-mb': 10}}}
        path = self.root / 'config.json'
        path.write_text(json.dumps(config), encoding='utf-8')
        path.chmod(0o600)
        self.process = subprocess.Popen([str(binary), '-config', str(path), '-local-model'], cwd=self.root,
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        deadline = time.monotonic() + 15
        while True:
            try:
                self.call('/credentials')
                break
            except Problem:
                if self.process.poll() is not None or time.monotonic() > deadline:
                    self.close()
                    raise Problem('独立本地授权实例启动失败；线上服务未修改。') from None
                time.sleep(.2)

    def call(self, path, payload=None, method=None):
        if self.mock_api:
            return self.mock_api(path, payload, method)
        request = urllib.request.Request('http://127.0.0.1:' + str(self.port) + '/v8/management' + path,
            data=json.dumps(payload).encode() if payload is not None else None,
            method=method or ('POST' if payload is not None else 'GET'),
            headers={'Authorization': 'Bearer ' + self.key, 'Content-Type': 'application/json'})
        try:
            with OPENER.open(request, timeout=20) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code in (404, 409, 410):
                raise Problem('授权已过期或该接入方式不可用，请检查结果后重新生成。') from None
            raise Problem('本地授权接口返回 HTTP ' + str(error.code) + '。') from None
        except (OSError, ValueError):
            raise Problem('本地授权实例没有返回有效结果。') from None

    def start(self):
        if self.state:
            try:
                self.call('/oauth/session?' + urllib.parse.urlencode({'state': self.state}), method='DELETE')
            except Problem:
                pass
        result = self.call('/oauth/auth-url', {'provider': self.provider, 'proxy_url': self.proxy})
        if not result.get('state') or not (result.get('url') or result.get('user_code')):
            raise Problem('平台没有返回有效的授权链接或设备授权码。')
        self.state = result['state']
        self.deadline = time.time() + int(result.get('expires_in') or (300 if self.provider == 'claude' else 600))
        self.submitted = False
        return {'url': result.get('url') or result.get('verification_uri') or result.get('verification_url'),
            'user_code': result.get('user_code'), 'state': self.state, 'deadline': self.deadline,
            'device_flow': bool(result.get('user_code'))}

    def callback(self, url):
        parts = urllib.parse.urlsplit(url)
        query = urllib.parse.parse_qs(parts.query)
        if not self.state or query.get('state', [''])[0] != self.state:
            raise Problem('这不是本次授权的回调，请使用最新链接。')
        if parts.hostname not in ('localhost', '127.0.0.1', '::1') or not query.get('code') or query.get('error'):
            raise Problem('回调地址、授权码或授权结果无效。')
        if self.submitted:
            return {'status': 'processing'}
        if time.time() >= self.deadline:
            raise Problem('授权已过期，重新生成即可，不必重填代理。')
        self.call('/oauth/callback', {'provider': self.provider, 'redirect_url': url})
        self.submitted = True
        return {'status': 'processing'}

    def status(self):
        matches = []
        for path in (self.root / 'auth').glob('*.json'):
            data = json.loads(path.read_text(encoding='utf-8'))
            if str(data.get('email', '')).casefold() == self.email.casefold() and provider_id(data.get('type')) == self.provider:
                if data.get('proxy_url') != self.proxy or not data.get('access_token'):
                    raise Problem('保存的账号与专属代理不匹配，未加入任何账号池。')
                matches.append((path, data))
        if len(matches) > 1:
            raise Problem('发现多个匹配凭据，请核对后再保存。')
        if matches:
            return {'status': 'verified', 'credential': str(matches[0][0]), 'email': self.email}
        if not self.state:
            return {'status': 'ready'}
        result = self.call('/oauth/status?' + urllib.parse.urlencode({'state': self.state}))
        if result.get('status') == 'error':
            message = str(result.get('error', '')).lower()
            return {'status': 'expired' if any(s in message for s in ('timeout', 'expired', 'unknown')) else 'failed',
                'message': '授权未完成，请检查结果后重试。'}
        if result.get('status') == 'ok':
            return {'status': 'processing', 'message': '授权完成，正在等待本地凭据落盘。'}
        return {'status': 'processing' if self.submitted else 'waiting'}

    def close(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=3)
