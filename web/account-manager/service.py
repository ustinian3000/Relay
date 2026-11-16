"""Local enrollment orchestration and management commands."""
import copy
import ipaddress
import json
import pathlib
import re
import secrets
import socket
import ssl
import threading
import time
import urllib.parse
import urllib.request

from adapters import OAuthWorker, Quotas, ReadOnlyProduction
from domain import CATALOG, Problem, Store, now_iso, provider_id, text


class Service:
    def __init__(self, directory, core, clock=time.time, production=None, worker_factory=OAuthWorker):
        self.directory = pathlib.Path(directory)
        self.store = Store(self.directory, clock=clock)
        self.clock = clock
        self.production = production or ReadOnlyProduction()
        self.quotas = Quotas(self.store, self.production, clock=clock)
        self.core = pathlib.Path(core)
        self.worker_factory = worker_factory
        self.flows = {}
        self.lock = threading.RLock()

    def overview(self):
        return {**self.store.snapshot(), 'environment': 'local_sandbox', 'production_writes_enabled': False,
            'catalog': CATALOG, 'core_available': self.core.is_file(), 'data_directory': str(self.directory),
            'ssh_target': self.production.target,
            'ssh_auth_mode': getattr(self.production, 'auth_mode', 'key'),
            'ssh_key_path': getattr(self.production, 'key_path', ''),
            'ssh_password_set': bool(getattr(self.production, '_password', '')),
            'ssh_key_loaded': getattr(self.production, '_private_key', None) is not None,
            'flows': [self.flow_public(f) for f in self.flows.values() if not f.get('saved')]}

    def flow_public(self, flow):
        return {k: flow.get(k) for k in ('id', 'provider', 'email', 'mode', 'status', 'deadline', 'url', 'state', 'user_code', 'device_flow', 'target_name', 'proxy_host', 'message')}

    def flow(self, identifier):
        if identifier not in self.flows:
            raise Problem('添加会话已失效，请重新开始。', 404)
        return self.flows[identifier]

    def fields(self, payload):
        provider = provider_id(payload.get('provider'))
        email = text(payload.get('email'), '账号邮箱或标识', 200).casefold()
        if provider in ('claude', 'codex') and not re.fullmatch(r'[^\s@]+@[^\s@]+\.[^\s@]+', email):
            raise Problem('Claude / GPT 账号请填写完整邮箱。')
        self.store.validate_target(payload.get('target'))
        with self.store.lock:
            if any(a['provider'] == provider and a['email'].casefold() == email for a in self.store.state['accounts']):
                raise Problem('这个平台下已有此账号，请在账号管理页调整绑定。')
        proxy = payload.get('proxy')
        if not isinstance(proxy, dict):
            raise Problem('请填写账号专属代理。')
        host = str(proxy.get('host', '')).strip()
        try:
            if not re.fullmatch(r'[A-Za-z0-9.-]+', host) or not host or host.startswith('-'):
                raise ValueError()
            port = int(proxy.get('port', 0))
            if not 1 <= port <= 65535:
                raise ValueError()
        except (ValueError, TypeError):
            raise Problem('代理主机或端口无效。') from None
        scheme = str(proxy.get('scheme', 'http'))
        if scheme not in ('http', 'https'):
            raise Problem('当前页面支持 HTTP / HTTPS 代理。')
        username, password = str(proxy.get('username', '')), str(proxy.get('password', ''))
        if not username or not password or len(username) > 300 or len(password) > 500 or any(ord(c) < 32 for c in username + password):
            raise Problem('代理用户名和密码不能为空、过长或包含控制字符。')
        url = scheme + '://' + urllib.parse.quote(username, safe='') + ':' + urllib.parse.quote(password, safe='') + '@' + host + ':' + str(port)
        return {'provider': provider, 'email': email, 'target': copy.deepcopy(payload['target']),
            'proxy_url': url, 'proxy_host': host, 'proxy_port': port, 'target_name': self.store.validate_target(payload['target'])}

    def probe(self, flow):
        if flow['mode'] == 'simulation':
            return {'egress_ip': flow['proxy_host'], 'simulated': True}
        url = flow['proxy_url']
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({'http': url, 'https': url}))
        try:
            with opener.open('https://api.ipify.org/', timeout=12) as response:
                egress = response.read(80).decode().strip()
            ipaddress.ip_address(egress)
        except Exception:
            raise Problem('住宅代理出口检查失败，请核对代理认证和有效期。', 502) from None
        try:
            expected_ip = str(ipaddress.ip_address(flow['proxy_host']))
        except ValueError:
            expected_ip = None
        if expected_ip and egress != expected_ip:
            raise Problem('实际出口 ' + egress + ' 与填写的固定 IP 不一致。')
        target = next(p['host'] for p in CATALOG if p['id'] == flow['provider'])
        parts = urllib.parse.urlsplit(url)
        import base64
        credential = base64.b64encode((urllib.parse.unquote(parts.username) + ':' + urllib.parse.unquote(parts.password)).encode()).decode()
        try:
            with socket.create_connection((parts.hostname, parts.port), timeout=10) as raw:
                conn = ssl.create_default_context().wrap_socket(raw, server_hostname=parts.hostname) if parts.scheme == 'https' else raw
                conn.settimeout(10)
                conn.sendall(('CONNECT ' + target + ':443 HTTP/1.1\r\nHost: ' + target + ':443\r\nProxy-Authorization: Basic ' + credential + '\r\n\r\n').encode())
                header = conn.recv(2048).split(b'\r\n')[0].decode(errors='replace')
            if not re.match(r'HTTP/\d(?:\.\d)? 200\b', header):
                status = re.search(r'\b\d{3}\b', header)
                raise Problem('代理拒绝连接所选平台（CONNECT ' + (status.group(0) if status else '未知状态') + '）。')
        except Problem:
            raise
        except Exception:
            raise Problem('代理无法连接所选平台。', 502) from None
        return {'egress_ip': egress, 'simulated': False}

    def _new_flow(self, payload, mode):
        fields = self.fields(payload)
        identifier = secrets.token_hex(16)
        flow = {'id': identifier, **fields, 'mode': mode, 'status': 'ready', 'saved': False,
            'account_id': 'acc-' + secrets.token_hex(10), 'created': self.clock()}
        return flow

    def enroll_start(self, payload):
        mode = payload.get('mode')
        if mode not in ('simulation', 'real'):
            raise Problem('请明确选择本地模拟授权或真实授权。')
        flow = self._new_flow(payload, mode)
        self.flows[flow['id']] = flow
        try:
            probe = self.probe(flow)
            if mode == 'real':
                flow['worker'] = self.worker_factory(self.directory / 'oauth' / flow['id'], self.core,
                    flow['provider'], flow['proxy_url'], flow['email'])
            result = self.restart(flow['id'])
            return {**result, 'proxy_check': probe}
        except Exception as error:
            flow['status'] = 'failed'
            flow['message'] = str(error) if isinstance(error, Problem) else '本地授权启动失败，请检查后重试。'
            raise

    def restart(self, identifier):
        flow = self.flow(identifier)
        if flow['status'] == 'verified' or flow['saved']:
            raise Problem('已有核对通过的结果，请先预览保存，不要重复授权。')
        self.store.validate_target(flow['target'])
        flow.pop('message', None)
        if flow['mode'] == 'simulation':
            flow.update({'state': secrets.token_hex(16), 'deadline': self.clock() + 300, 'status': 'waiting', 'device_flow': False})
            flow['url'] = '/test-authorize?flow=' + flow['id'] + '&state=' + flow['state']
        else:
            if not flow.get('worker'):
                self.probe(flow)
                flow['worker'] = self.worker_factory(self.directory / 'oauth' / flow['id'], self.core,
                    flow['provider'], flow['proxy_url'], flow['email'])
            result = flow['worker'].start()
            flow.update(result)
            flow['status'] = 'waiting'
        return self.flow_public(flow)

    def enroll_status(self, identifier):
        flow = self.flow(identifier)
        if flow['saved']:
            return {**self.flow_public(flow), 'saved': True}
        if flow['mode'] == 'real' and flow['status'] != 'verified' and flow.get('worker'):
            result = flow['worker'].status()
            flow.update({k: v for k, v in result.items() if k in ('status', 'message', 'credential')})
        if flow['status'] in ('ready', 'waiting') and self.clock() >= flow.get('deadline', 0):
            flow['status'] = 'expired'
        return self.flow_public(flow)

    def simulate(self, identifier, state, approved):
        flow = self.flow(identifier)
        if flow['mode'] != 'simulation' or flow.get('state') != state:
            raise Problem('这不是当前模拟授权会话。', 409)
        if flow['status'] not in ('waiting', 'expired') or self.clock() >= flow['deadline']:
            raise Problem('模拟授权已结束或已过期，请重新生成。', 409)
        flow['status'] = 'verified' if approved else 'failed'
        flow['message'] = '本地模拟授权通过；未联系真实平台。' if approved else '已模拟授权失败，没有改变账号池。'
        return self.flow_public(flow)

    def callback(self, identifier, url):
        flow = self.flow(identifier)
        if flow['mode'] != 'real':
            raise Problem('模拟授权请在本地测试授权页面确认，无需粘贴真实回调。')
        result = flow['worker'].callback(str(url))
        flow['status'] = result['status']
        return self.enroll_status(identifier)

    def enroll_import(self, payload):
        flow = self._new_flow(payload, 'import')
        raw = payload.get('credential')
        if not isinstance(raw, dict) or not isinstance(raw.get('access_token'), str) or not raw['access_token'].strip() or provider_id(raw.get('type')) != flow['provider']:
            flow['status'] = 'failed'
            raise Problem('凭据类型必须与所选平台一致，且包含有效格式的 access_token。')
        email = str(raw.get('email', '')).strip().casefold()
        if email and email != flow['email']:
            raise Problem('凭据邮箱与填写的账号不一致，未导入。')
        if raw.get('proxy_url') and raw['proxy_url'] != flow['proxy_url']:
            raise Problem('凭据中的代理与填写信息不一致，未导入。')
        credential = dict(raw)
        credential['proxy_url'] = flow['proxy_url']
        vault = self.directory / 'credentials'
        vault.mkdir(mode=0o700, exist_ok=True)
        path = vault / (flow['id'] + '.json')
        path.write_text(json.dumps(credential, ensure_ascii=False), encoding='utf-8')
        path.chmod(0o600)
        flow.update({'status': 'verified', 'credential': str(path), 'message': '凭据格式、平台、邮箱和代理一致；尚未验证上游有效性。'})
        self.flows[flow['id']] = flow
        return self.flow_public(flow)

    def enroll_preview(self, identifier):
        flow = self.flow(identifier)
        self.enroll_status(identifier)
        if flow['status'] != 'verified' or flow['saved']:
            raise Problem('请先完成授权或凭据导入并核对结果。')
        account = {'id': flow['account_id'], 'provider': flow['provider'], 'email': flow['email'], 'enabled': True,
            'key_id': None, 'proxy_host': flow['proxy_host'], 'proxy_port': flow['proxy_port'], 'proxy_mode': 'dedicated',
            'verified': True, 'origin_ref': None, 'credential_ref': flow.get('credential')}
        if flow['mode'] == 'simulation':
            account['demo_quota'] = {'status': 'ok', 'available': True, 'fiveHour': {'remainingPercent': 100, 'usedPercent': 0, 'exhausted': False},
                'weekly': {'remainingPercent': 100, 'usedPercent': 0, 'exhausted': False}, 'source': 'local_demo', 'error': None}
        result = self.store.preview('account_add', {'account': account, 'target': flow['target']})
        result['enrollment'] = self.flow_public(flow)
        return result

    def perform(self, action, payload):
        with self.lock:
            if action == 'preview':
                if payload.get('kind') not in ('key_create', 'key_update', 'key_delete', 'bindings_set', 'account_assign', 'account_toggle'):
                    raise Problem('请通过账号添加向导或只读同步生成此类预览。')
                return self.store.preview(payload['kind'], payload.get('payload') or {})
            if action == 'commit':
                result = self.store.commit(payload.get('plan_id'))
                for flow in self.flows.values():
                    if any(a['id'] == flow['account_id'] for a in self.store.state['accounts']):
                        flow['saved'] = True
                        if flow.get('worker'):
                            flow['worker'].close()
                return result
            if action == 'usage':
                return self.quotas.query(payload.get('key_id'))
            if action == 'connection':
                try:
                    self.production.configure(payload)
                finally:
                    payload.pop('ssh_password', None)
                    payload.pop('key_content', None)
                self.quotas.cache.clear()
                return {'configured': True, 'ssh_target': self.production.target, 'read_only': True}
            if action == 'sync-preview':
                return self.store.preview('source_sync', {'snapshot': self.production.snapshot()})
            if action == 'enroll-start':
                return self.enroll_start(payload)
            if action == 'enroll-import':
                return self.enroll_import(payload)
            if action == 'enroll-status':
                return self.enroll_status(payload.get('id'))
            if action == 'enroll-restart':
                return self.restart(payload.get('id'))
            if action == 'enroll-callback':
                return self.callback(payload.get('id'), payload.get('redirect_url'))
            if action == 'enroll-simulate':
                if not isinstance(payload.get('approved'), bool):
                    raise Problem('请明确选择通过或拒绝模拟授权。')
                return self.simulate(payload.get('id'), payload.get('state'), payload['approved'])
            if action == 'enroll-preview':
                return self.enroll_preview(payload.get('id'))
            raise Problem('不支持的管理操作。', 404)

    def close(self):
        if hasattr(self.production, 'close'):
            self.production.close()
        elif hasattr(self.production, '_password'):
            self.production._password = ''
        for flow in self.flows.values():
            if flow.get('worker'):
                flow['worker'].close()
