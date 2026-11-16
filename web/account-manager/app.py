"""Loopback web server. All mutations target the local sandbox only."""
import argparse
import concurrent.futures
import http.cookies
import http.server
import json
import os
import pathlib
import re
import secrets
import sys
import threading
import urllib.parse
import urllib.request
import webbrowser

from domain import Problem
from service import Service

ROOT = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT.parent))
from runtime_contract import health, require_compatible
DEFAULT_CORE = ROOT.parents[1] / 'runtime' / 'cli-proxy-api.exe'


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, service):
        self.service = service
        self.cookie = secrets.token_urlsafe(32)
        self.executor = concurrent.futures.ThreadPoolExecutor(max_workers=4)
        self.jobs = {}
        self.job_lock = threading.Lock()
        super().__init__(address, Handler)

    def submit(self, action, payload):
        with self.job_lock:
            if sum(not job.done() for job in self.jobs.values()) >= 8:
                raise Problem('操作较多，请等待已有任务完成。', 429)
            identifier = secrets.token_hex(16)
            self.jobs[identifier] = self.executor.submit(self.service.perform, action, payload)
            for key in list(self.jobs)[:-50]:
                if self.jobs[key].done():
                    del self.jobs[key]
            return identifier


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send(self, status, data, content_type='application/json; charset=utf-8', cookie=False):
        body = data.encode('utf-8') if isinstance(data, str) else json.dumps(data, ensure_ascii=False).encode('utf-8')
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Referrer-Policy', 'no-referrer')
        self.send_header('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
        if cookie:
            self.send_header('Set-Cookie', 'manager_local=' + self.server.cookie + '; HttpOnly; SameSite=Strict; Path=/')
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def valid_host(self):
        return self.headers.get('Host') in ('127.0.0.1:' + str(self.server.server_port), 'localhost:' + str(self.server.server_port))

    def authorized(self):
        try:
            cookies = http.cookies.SimpleCookie(self.headers.get('Cookie', ''))
            return cookies.get('manager_local') and secrets.compare_digest(cookies['manager_local'].value, self.server.cookie)
        except http.cookies.CookieError:
            return False

    def do_GET(self):
        if not self.valid_host():
            self.send(403, {'error': '仅允许本机访问。'})
            return
        path = urllib.parse.urlsplit(self.path).path
        assets = {'/': ('index.html', 'text/html'), '/app.js': ('app.js', 'application/javascript'),
            '/style.css': ('style.css', 'text/css'), '/icon.svg': ('icon.svg', 'image/svg+xml'), '/test-authorize': ('test-authorize.html', 'text/html'), '/test-authorize.js': ('test-authorize.js', 'application/javascript')}
        if path == '/api/health':
            self.send(200, health('local-sk-account-manager', mode='local_sandbox'))
            return
        if path in assets:
            name, kind = assets[path]
            self.send(200, (ROOT / name).read_text(encoding='utf-8'), kind + '; charset=utf-8', cookie=kind == 'text/html')
            return
        if not self.authorized():
            self.send(403, {'error': '请先打开本机管理台页面。'})
            return
        if path == '/api/overview':
            self.send(200, self.server.service.overview())
        elif path == '/api/export':
            self.send(200, self.server.service.store.export_plan())
        elif path.startswith('/api/jobs/'):
            with self.server.job_lock:
                job = self.server.jobs.get(path.rsplit('/', 1)[-1])
            if job is None:
                self.send(404, {'error': '操作记录已过期，请重新检查。'})
            elif not job.done():
                self.send(200, {'pending': True})
            else:
                try:
                    with self.server.job_lock:
                        result = dict(job.result())
                        if 'one_time_key' in result:
                            job.result().pop('one_time_key', None)
                            job.result()['secret_already_shown'] = True
                    self.send(200, {'pending': False, 'ok': True, 'data': result})
                except Problem as error:
                    self.send(200, {'pending': False, 'ok': False, 'error': str(error), 'status': error.status})
                except Exception:
                    self.send(200, {'pending': False, 'ok': False, 'error': '操作未完成，请刷新本地状态后重试。'})
        else:
            self.send(404, {'error': '页面不存在。'})

    def do_POST(self):
        expected = 'http://' + self.headers.get('Host', '')
        if not self.valid_host() or not self.authorized() or self.headers.get('Origin') != expected:
            self.send(403, {'error': '请从本机管理台操作。'})
            return
        path = urllib.parse.urlsplit(self.path).path
        if not re.fullmatch(r'/api/[a-z-]+', path):
            self.send(404, {'error': '接口不存在。'})
            return
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 < length <= 2 * 1024 * 1024:
                raise Problem('请求内容为空或超过 2 MiB。', 413)
            payload = json.loads(self.rfile.read(length))
            if not isinstance(payload, dict):
                raise Problem('请求必须为 JSON 对象。')
            self.send(202, {'job': self.server.submit(path.rsplit('/', 1)[-1], payload)})
        except (ValueError, Problem) as error:
            self.send(getattr(error, 'status', 400), {'error': str(error)})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=18766)
    parser.add_argument('--data-dir', default=str(ROOT / '.local-data'))
    parser.add_argument('--core', default=str(DEFAULT_CORE))
    parser.add_argument('--no-open', action='store_true')
    args = parser.parse_args()
    url = 'http://127.0.0.1:' + str(args.port)
    service = Service(args.data_dir, args.core)
    try:
        server = Server(('127.0.0.1', args.port), service)
    except OSError:
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(url + '/api/health', timeout=2) as response:
                require_compatible(json.load(response), 'local-sk-account-manager')
        except RuntimeError as error:
            service.close()
            raise SystemExit(str(error)) from None
        except Exception:
            service.close()
            raise SystemExit('端口已被其他程序占用，请指定另一个 --port。') from None
        service.close()
        if not args.no_open:
            webbrowser.open(url)
        return
    print('账号与 SK 管理台（本地测试）：' + url, flush=True)
    print('所有写入只影响本地测试副本，不修改线上服务。', flush=True)
    if not args.no_open:
        webbrowser.open(url)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        service.close()
        server.executor.shutdown(wait=False, cancel_futures=True)


if __name__ == '__main__':
    main()
