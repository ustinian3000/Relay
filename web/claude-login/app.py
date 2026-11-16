"""Loopback-only HTML wizard backed by the user's existing SSH authentication."""
import argparse
import concurrent.futures
import hashlib
import http.server
import ipaddress
import json
import os
import pathlib
import re
import secrets
import sys
import socket
import threading
import time
import urllib.parse
import urllib.request
import webbrowser

from ssh_transport import SSHConnection, TunnelError

ROOT = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT.parent))
from runtime_contract import health, require_compatible
EXECUTOR = concurrent.futures.ThreadPoolExecutor(max_workers=4)
JOBS = {}
JOBS_LOCK = threading.Lock()
STATE = {}
STATE_LOCK = threading.RLock()


class UserError(Exception):
    def __init__(self, message, code=None, details=None):
        super().__init__(message)
        self.code = code
        self.details = details or {}



def remote(settings, request):
    connection = settings.get('_connection')
    if connection is None:
        raise UserError('SSH 连接不存在，请重新检查连接。')
    try:
        return connection.rpc((ROOT / 'remote_helper.py').read_text(encoding='utf-8'), request)
    except TunnelError as error:
        raise UserError(str(error), code=error.code, details=error.details) from None
    except (OSError, EOFError):
        raise UserError('SSH 连接中断，请重新检查连接；已提交回调时先检查结果。') from None


def normalize_fields(data):
    target = str(data.get('ssh_target', '')).strip()
    if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_.-]*@[A-Za-z0-9.-]+', target):
        raise UserError('SSH 地址格式应为 ubuntu@服务器IP。')
    email = str(data.get('email', '')).strip().casefold()
    if not re.fullmatch(r'[^\s@]+@[^\s@]+\.[^\s@]+', email):
        raise UserError('请填写新 Claude 账号的完整邮箱。')
    mode = str(data.get('connection_mode', 'residential'))
    if mode not in ('direct', 'residential'):
        raise UserError('请选择服务器直连或住宅代理模式。')
    proxy = 'direct'
    if mode == 'residential':
        host = str(data.get('proxy_host', '')).strip()
        try:
            ipaddress.IPv4Address(host)
            port = int(data.get('proxy_port', ''))
            if not 1 <= port <= 65535:
                raise ValueError()
        except ValueError:
            raise UserError('请填写有效的住宅 IPv4 和端口。') from None
        scheme = str(data.get('proxy_scheme', 'http'))
        if scheme not in ('http', 'https'):
            raise UserError('当前向导支持 HTTP/HTTPS 住宅代理。')
        user, password = str(data.get('proxy_user', '')), str(data.pop('proxy_password', ''))
        if not user or not password:
            raise UserError('请填写代理用户名和密码。')
        proxy = scheme + '://' + urllib.parse.quote(user, safe='') + ':' + urllib.parse.quote(password, safe='') + '@' + host + ':' + str(port)
    auth_mode = str(data.get('auth_mode', 'key'))
    if auth_mode not in ('key', 'password'):
        raise UserError('请选择 SSH 密码或私钥。')
    key_content = str(data.pop('key_content', ''))
    if len(key_content.encode('utf-8')) > 32768:
        raise UserError('私钥文件不能超过 32 KiB。')
    if auth_mode != 'key':
        key_content = ''
    ssh_password = str(data.pop('ssh_password', ''))
    if auth_mode == 'password' and not ssh_password:
        raise UserError('请输入 SSH 登录密码。')
    try:
        ssh_port = int(data.get('ssh_port', 22))
        if not 1 <= ssh_port <= 65535:
            raise ValueError()
    except ValueError:
        raise UserError('SSH 端口无效。') from None
    key_path = str(data.get('key_path', '')).strip() if auth_mode == 'key' else ''
    if key_path and key_content:
        raise UserError('请选择私钥文件或填写路径，两种方式只用一种。')
    if key_path:
        expanded = pathlib.Path(key_path).expanduser().resolve()
        if not expanded.is_file():
            raise UserError('PEM 文件不存在，请填写本机完整路径。')
        key_path = str(expanded)
    target_key = str(data.pop('target_key', '')).strip()
    settings = {'ssh_target': target, 'ssh_port': ssh_port, 'key_path': key_path,
        'key_content': key_content, 'auth_mode': auth_mode, 'ssh_password': ssh_password if auth_mode == 'password' else ''}
    return settings, {'email': email, 'proxy': proxy, 'mode': mode,
        'target_fingerprint': hashlib.sha256(target_key.encode()).hexdigest() if target_key else None}



def operation(action, data):
    with STATE_LOCK:
        if action == 'prepare':
            settings, payload = normalize_fields(data)
            previous = STATE.get('session')
            session = secrets.token_hex(16)
            connection = None
            try:
                connection = SSHConnection(settings)
                settings['_connection'] = connection
                settings.pop('ssh_password', None)
                settings.pop('key_content', None)
                result = remote(settings, {'action': 'bootstrap', 'session': session, **payload})
                if payload['mode'] == 'direct':
                    result['tunnel'] = connection.start_tunnel()
                    if connection.tunnel_egress() != result['egress_ip']:
                        raise UserError('SSH 隧道实际出口与服务器检查出口不同，请核对服务器路由。')
            except (UserError, TunnelError) as error:
                if connection:
                    try:
                        remote(settings, {'action': 'stop', 'session': session})
                    except Exception:
                        pass
                    connection.close()
                raise UserError(str(error), code=error.code, details=error.details) from None
            finally:
                settings.pop('ssh_password', None)
                settings.pop('key_content', None)
            if previous and STATE.get('settings'):
                try:
                    remote(STATE['settings'], {'action': 'stop', 'session': previous})
                except UserError:
                    pass
                old = STATE['settings'].get('_connection')
                if old:
                    old.close()
            STATE.clear()
            STATE.update({'settings': settings, 'session': session, 'email': payload['email'],
                'mode': payload['mode'], 'confirmed': False, 'public': result})
            return result
        if not STATE.get('session'):
            raise UserError('请先完成第一步，检查服务器连接。')
        if action == 'disconnect':
            try:
                remote(STATE['settings'], {'action': 'stop', 'session': STATE['session']})
            finally:
                STATE['settings']['_connection'].close()
                STATE.clear()
            return {'disconnected': True}
        if action == 'confirm-egress':
            if str(data.get('browser_ip', '')).strip() != STATE['public']['egress_ip']:
                raise UserError('浏览器出口与检查出口不同，请核对浏览器代理设置。')
            remote(STATE['settings'], {'action': 'check-egress', 'session': STATE['session']})
            if STATE.get('mode') == 'direct':
                try:
                    actual = STATE['settings']['_connection'].tunnel_egress()
                except TunnelError as error:
                    raise UserError(str(error), code=error.code, details=error.details) from None
                if actual != STATE['public']['egress_ip']:
                    raise UserError('隧道出口变化，请重新检查连接。')
            STATE['confirmed'] = True
            return {'confirmed': True}
        if action in ('start', 'callback', 'publish'):
            if not STATE.get('confirmed'):
                raise UserError('请先填写浏览器查询到的出口 IP 并确认一致。')
            remote(STATE['settings'], {'action': 'check-egress', 'session': STATE['session']})
        if action not in ('start', 'status', 'callback', 'publish'):
            raise UserError('不支持的操作。')
        request = {'action': action, 'session': STATE['session']}
        if action == 'callback':
            request['redirect_url'] = str(data.get('redirect_url', '')).strip()
        result = remote(STATE['settings'], request)
        if result.get('target_key'):
            STATE['public']['target_key'] = result['target_key']
        if action == 'start' and result.get('state'):
            result['expires_at_local'] = time.time() + result.get('expires_in', 300)
            STATE['oauth'] = result
        if action == 'publish':
            STATE['published'] = result
        return result


def submit(action, payload):
    with JOBS_LOCK:
        for job in JOBS.values():
            if not job['future'].done():
                if action == 'status':
                    raise UserError('上一个操作仍在处理，请稍后检查结果。')
                raise UserError('正在处理上一个操作，请勿重复提交。')
        job_id = secrets.token_hex(16)
        JOBS[job_id] = {'future': EXECUTOR.submit(operation, action, payload)}
        for key in list(JOBS)[:-40]:
            if JOBS[key]['future'].done():
                del JOBS[key]
    return {'job': job_id}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def response(self, code, body, content_type='application/json; charset=utf-8'):
        raw = body.encode() if isinstance(body, str) else json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(raw)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Referrer-Policy', 'no-referrer')
        self.send_header('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
        if content_type.startswith('text/html'):
            self.send_header('Set-Cookie', 'claude_local=' + self.server.cookie + '; HttpOnly; SameSite=Strict; Path=/')
        self.end_headers()
        self.wfile.write(raw)

    def valid_host(self):
        return self.headers.get('Host') in {'127.0.0.1:' + str(self.server.server_port), 'localhost:' + str(self.server.server_port)}

    def authorized(self):
        return 'claude_local=' + self.server.cookie in self.headers.get('Cookie', '').split('; ')

    def do_GET(self):
        if not self.valid_host():
            self.response(403, {'error': '仅允许本机访问。'})
            return
        path = urllib.parse.urlsplit(self.path).path
        if path == '/api/health':
            self.response(200, health('claude-account-wizard', ready=True))
            return
        if path in ('/', '/app.js', '/style.css'):
            name, content_type = {'/': ('index.html', 'text/html; charset=utf-8'), '/app.js': ('app.js', 'application/javascript; charset=utf-8'), '/style.css': ('style.css', 'text/css; charset=utf-8')}[path]
            self.response(200, (ROOT / name).read_text(encoding='utf-8'), content_type)
            return
        if path.startswith('/api/jobs/') and self.authorized():
            job_id = path.rsplit('/', 1)[-1]
            with JOBS_LOCK:
                job = JOBS.get(job_id)
            if not job:
                self.response(404, {'error': '操作记录已失效，请重新检查结果。'})
            elif not job['future'].done():
                self.response(200, {'pending': True})
            else:
                try:
                    self.response(200, {'pending': False, 'ok': True, 'data': job['future'].result()})
                except UserError as error:
                    self.response(200, {'pending': False, 'ok': False, 'error': str(error), 'code': error.code, 'details': error.details})
                except Exception:
                    self.response(200, {'pending': False, 'ok': False, 'error': '操作未完成，请先检查结果，不要重复提交旧回调。'})
            return
        if path == '/api/context' and self.authorized():
            with STATE_LOCK:
                self.response(200, {'ready': bool(STATE.get('session')), 'email': STATE.get('email'),
                    'session': STATE.get('session'), 'proxy': STATE.get('public'), 'confirmed': STATE.get('confirmed', False),
                    'tunnel_active': bool(STATE.get('settings', {}).get('_connection') and STATE['settings']['_connection'].active), 'oauth': ({**STATE['oauth'], 'expires_in': max(0, STATE['oauth']['expires_at_local'] - time.time())} if STATE.get('oauth', {}).get('expires_at_local') else STATE.get('oauth')), 'published': STATE.get('published')})
            return
        self.response(404, {'error': '页面不存在。'})

    def do_POST(self):
        origin = self.headers.get('Origin')
        expected = 'http://' + self.headers.get('Host', '')
        if not self.valid_host() or not self.authorized() or origin != expected:
            self.response(403, {'error': '请从本机登录助手页面操作。'})
            return
        path = urllib.parse.urlsplit(self.path).path
        if not re.fullmatch(r'/api/(prepare|start|status|callback|publish|confirm-egress|disconnect)', path):
            self.response(404, {'error': '不支持的操作。'})
            return
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 < length <= 65536:
                raise UserError('填写内容过长或为空。')
            payload = json.loads(self.rfile.read(length))
            if not isinstance(payload, dict):
                raise UserError('填写内容无效。')
            self.response(202, submit(path.rsplit('/', 1)[-1], payload))
        except (ValueError, UserError) as error:
            self.response(400, {'error': str(error)})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=18765)
    parser.add_argument('--no-open', action='store_true')
    args = parser.parse_args()
    try:
        server = http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    except OSError:
        url = 'http://127.0.0.1:' + str(args.port)
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(url + '/api/health', timeout=2) as response:
                existing = json.load(response)
            require_compatible(existing, 'claude-account-wizard')
        except RuntimeError as error:
            raise SystemExit(str(error)) from None
        except Exception:
            raise SystemExit('本机端口已被其他程序占用，请指定另一个 --port。') from None
        print('助手已在运行：' + url, flush=True)
        if not args.no_open:
            webbrowser.open(url)
        return
    server.cookie = secrets.token_urlsafe(32)
    url = 'http://127.0.0.1:' + str(server.server_port)
    print('Claude 账号添加助手：' + url, flush=True)
    print('无需生产管理密码。保持此窗口运行，关闭后重新启动需要重新检查连接。', flush=True)
    if not args.no_open:
        webbrowser.open(url)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        with STATE_LOCK:
            if STATE.get('settings', {}).get('_connection'):
                STATE['settings']['_connection'].close()
            STATE.clear()
        server.server_close()
        EXECUTOR.shutdown(wait=False, cancel_futures=True)


if __name__ == '__main__':
    main()
