"""Private SSH RPC for an isolated Claude OAuth worker. No public listener."""
import base64
import concurrent.futures
import datetime
import fcntl
import hashlib
import ipaddress
import json
import os
import pathlib
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import yaml

PRODUCTION = pathlib.Path.home() / 'tools' / 'cliproxyapi'
SESSIONS = pathlib.Path.home() / '.cache' / 'claude-web-login'
BACKUPS = pathlib.Path.home()
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class LoginError(Exception):
    pass


def session_dir(request):
    session = request.get('session', '')
    if not re.fullmatch(r'[a-f0-9]{32}', session):
        raise LoginError('登录实例标识无效，请重新打开页面。')
    return SESSIONS / session


def read_config():
    return yaml.safe_load((PRODUCTION / 'config.yaml').read_text())


def isolation_bindings(config):
    plugins = config.get('plugins', {})
    policy = plugins.get('configs', {}).get('key-account-bind', {})
    if plugins.get('enabled') is not True or policy.get('enabled', True) is not True:
        raise LoginError('账号隔离插件未启用，已阻止添加账号。')
    if policy.get('unbound', 'deny') != 'deny':
        raise LoginError('隔离插件必须拒绝未绑定的 SK，已阻止添加账号。')
    bindings = policy.get('bindings')
    if not isinstance(bindings, list) or not bindings:
        raise LoginError('没有有效的 SK 绑定规则，已阻止添加账号。')
    keys, owners = set(), set()
    for binding in bindings:
        if not isinstance(binding, dict) or not isinstance(binding.get('key'), str) or not binding['key'] or binding['key'] in keys:
            raise LoginError('SK 绑定规则无效或重复，已阻止添加账号。')
        keys.add(binding['key'])
        names = binding.get('allow')
        if not isinstance(names, list) or not names:
            raise LoginError('账号绑定必须包含明确文件名，空池请使用空池标记。')
        for name in names:
            if not isinstance(name, str) or not name or any(c in name for c in '*?[/\\') or not name.endswith('.json'):
                raise LoginError('账号池包含通配或非文件名规则，无法保证隔离。')
            if name == '__empty_pool__.json':
                continue
            if name.casefold() in owners:
                raise LoginError('账号被重复绑定到多个位置，无法保证账号独占。')
            owners.add(name.casefold())
    return bindings


def target_binding(config, fingerprint=None):
    bindings = isolation_bindings(config)
    if fingerprint:
        selected = [b for b in bindings if hashlib.sha256(b['key'].encode()).hexdigest() == fingerprint]
    else:
        selected = bindings if len(bindings) == 1 else []
    if len(selected) != 1:
        raise LoginError('无法唯一确认目标 SK，请填写准备添加到的业务 SK。')
    return selected[0]


def describe_target(config, fingerprint):
    binding = target_binding(config, fingerprint)
    key = binding['key']
    tail_length = min(6, max(0, len(key) // 3))
    tail = key[-tail_length:] if tail_length else ''
    suffix = key[-min(3, tail_length):] if tail_length else ''
    position = isolation_bindings(config).index(binding) + 1
    return {'masked': ('sk-' if key.startswith('sk-') else '') + '••••••' + tail,
            'suffix': suffix, 'fingerprint': hashlib.sha256(key.encode()).hexdigest(),
            'pool_label': '账号池 ' + str(position),
            'account_count': sum(name != '__empty_pool__.json' for name in binding['allow'])}


def session_target(request):
    directory = session_dir(request)
    meta = json.loads((directory / 'worker.json').read_text())
    fingerprint = meta.get('target_fingerprint')
    if not fingerprint:
        raise LoginError('本次授权缺少目标 SK 标识，请重新检查连接。')
    return describe_target(read_config(), fingerprint)


def account_usage_id(key, filename):
    scope = hashlib.sha256(('cli-proxy-api:caller-scope:v1\0' + key).encode()).hexdigest()
    return 'account-' + hashlib.sha256(('account-usage:v1\0' + scope + '\0' + filename).encode()).hexdigest()[:16]


def verify_isolation(config):
    bindings = isolation_bindings(config)
    auth = pathlib.Path(config['oauth']['auth-dir'])
    port = int(config.get('server', {}).get('port', 8317))
    if not 1 <= port <= 65535:
        raise LoginError('生产服务端口配置无效。')

    def inspect(binding):
        expected = {account_usage_id(binding['key'], name) for name in binding['allow']
                    if name != '__empty_pool__.json' and (auth / name).is_file()}
        request = urllib.request.Request('http://127.0.0.1:' + str(port) + '/v1/account-usage',
            headers={'Authorization': 'Bearer ' + binding['key']})
        try:
            with OPENER.open(request, timeout=20) as response:
                if response.status != 200:
                    raise ValueError()
                payload = json.load(response)
            rows = payload.get('accounts')
            if not isinstance(rows, list) or any(not isinstance(row, dict) or not isinstance(row.get('id'), str) for row in rows):
                raise ValueError()
            actual = [row['id'] for row in rows]
            if len(actual) != len(set(actual)) or set(actual) != expected:
                raise ValueError()
        except Exception:
            raise LoginError('运行中服务未通过账号隔离检查：账号范围与绑定表不一致，或隔离插件未加载；未继续发布。') from None

    executor = concurrent.futures.ThreadPoolExecutor(max_workers=4)
    futures = [executor.submit(inspect, binding) for binding in bindings]
    try:
        _, pending = concurrent.futures.wait(futures, timeout=25)
        if pending:
            raise LoginError('运行中账号隔离检查超时，未继续发布。')
        for future in futures:
            future.result()
    finally:
        executor.shutdown(wait=False, cancel_futures=True)


def validate_proxy(proxy):
    try:
        parts = urllib.parse.urlsplit(proxy)
        if parts.scheme not in ('http', 'https') or not parts.hostname or not parts.port or parts.path not in ('', '/') or parts.query or parts.fragment:
            raise ValueError()
        if not parts.username or not parts.password:
            raise ValueError()
        return parts
    except ValueError:
        raise LoginError('代理需要包含 HTTP 地址、端口、用户名和密码。') from None


def proxy_identity(value):
    if value == 'direct':
        return ('direct',)
    parts = validate_proxy(value)
    return (parts.scheme.lower(), parts.hostname.lower(), parts.port,
            urllib.parse.unquote(parts.username), urllib.parse.unquote(parts.password))


def proxy_matches(actual, expected):
    try:
        return proxy_identity(actual) == proxy_identity(expected)
    except LoginError:
        return False


def probe_proxy(proxy):
    parts = validate_proxy(proxy)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({'http': proxy, 'https': proxy}))
    try:
        with opener.open('https://api.ipify.org/', timeout=15) as response:
            egress = response.read(100).decode().strip()
        ipaddress.ip_address(egress)
    except Exception:
        raise LoginError('住宅代理出口检查失败，请核对用户名、密码、有效期和代理服务状态。') from None
    credentials = base64.b64encode((urllib.parse.unquote(parts.username) + ':' + urllib.parse.unquote(parts.password)).encode()).decode()
    try:
        with socket.create_connection((parts.hostname, parts.port), timeout=12) as connection:
            if parts.scheme == 'https':
                import ssl
                connection = ssl.create_default_context().wrap_socket(connection, server_hostname=parts.hostname)
            connection.settimeout(12)
            connection.sendall(('CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: Basic ' + credentials + '\r\n\r\n').encode())
            first = connection.recv(2048).split(b'\r\n', 1)[0].decode(errors='replace')
        if not re.match(r'HTTP/\d(?:\.\d)? 200\b', first):
            code = re.search(r'\b(\d{3})\b', first)
            raise LoginError('代理连接 Claude 被拒绝（CONNECT ' + (code.group(1) if code else '未知状态') + '）。')
    except LoginError:
        raise
    except Exception:
        raise LoginError('代理暂时无法连接 Claude，请稍后重新检查。') from None
    if egress != parts.hostname:
        raise LoginError('实际出口 IP 为 ' + egress + '，与填写的固定 IP 不同，请先确认代理出口。')
    return {'egress_ip': egress, 'claude_connect': 200}


def probe_direct():
    try:
        with OPENER.open('https://api.ipify.org/', timeout=15) as response:
            egress = response.read(100).decode().strip()
        ipaddress.ip_address(egress)
        import ssl
        with socket.create_connection(('api.anthropic.com', 443), timeout=12) as connection:
            with ssl.create_default_context().wrap_socket(connection, server_hostname='api.anthropic.com'):
                pass
        return {'egress_ip': egress, 'claude_connect': 200}
    except Exception:
        raise LoginError('服务器直连出口或 Claude TLS 连接检查失败，请核对服务器网络。') from None


def check_egress(request):
    directory = session_dir(request)
    meta = json.loads((directory / 'worker.json').read_text())
    probe = probe_direct() if meta['proxy'] == 'direct' else probe_proxy(meta['proxy'])
    if meta.get('egress_ip') and probe['egress_ip'] != meta['egress_ip']:
        raise LoginError('服务器实际出口发生变化，请重新检查连接并核对浏览器出口。')
    return probe


def display_exit(meta):
    return meta.get('egress_ip') if meta['proxy'] == 'direct' else urllib.parse.urlsplit(meta['proxy']).hostname


def worker_api(directory, path, method='GET', payload=None):
    meta = json.loads((directory / 'worker.json').read_text())
    key = (directory / 'management-secret').read_text()
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request('http://127.0.0.1:' + str(meta['port']) + '/v8/management' + path,
        data=data, method=method, headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
    try:
        with OPENER.open(request, timeout=20) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        if error.code in (404, 409, 410):
            raise LoginError('授权已过期或已结束，请生成新的授权链接。') from None
        raise LoginError('登录实例返回 HTTP ' + str(error.code) + '，请重新检查连接。') from None
    except (OSError, ValueError):
        raise LoginError('无法连接独立登录实例，请重新检查代理以建立新实例。') from None


def bootstrap(request):
    directory = session_dir(request)
    proxy = request['proxy']
    direct = request.get('mode') == 'direct'
    if direct and proxy != 'direct':
        raise LoginError('直连模式配置不一致。')
    parts = None if direct else validate_proxy(proxy)
    email = request['email'].strip().casefold()
    if not re.fullmatch(r'[^\s@]+@[^\s@]+\.[^\s@]+', email):
        raise LoginError('请输入完整的 Claude 账号邮箱。')
    config = read_config()
    binding = target_binding(config, request.get('target_fingerprint'))
    verify_isolation(config)
    auth = pathlib.Path(config['oauth']['auth-dir'])
    for path in auth.glob('*.json'):
        try:
            data = json.loads(path.read_text())
        except (OSError, ValueError):
            continue
        if data.get('type') in ('claude', 'anthropic') and str(data.get('email', '')).casefold() == email:
            raise LoginError('这个邮箱的 Claude 账号已经在生产服务中，请先核对现有账号，不要重复添加。')
    probe = probe_direct() if direct else probe_proxy(proxy)
    if directory.exists():
        raise LoginError('登录实例已存在，请重新检查以创建新的实例。')
    directory.mkdir(mode=0o700, parents=True)
    (directory / 'auth').mkdir(mode=0o700)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    key = secrets.token_urlsafe(36)
    worker_config = {
        'config-version': 8, 'server': {'host': '127.0.0.1', 'port': port},
        'management': {'allow-remote': False, 'secret-key': key, 'disable-control-panel': True, 'disable-auto-update-panel': True},
        'access': {'api-keys': ['private-login-' + secrets.token_hex(24)]},
        'oauth': {'auth-dir': str(directory / 'auth')},
        'requests': {'proxy-url': proxy}, 'plugins': {'enabled': False},
        'observability': {'logs': {'request-log': False, 'logging-to-file': False, 'logs-max-total-size-mb': 10}},
    }
    for name, content in [('config.yaml', yaml.safe_dump(worker_config, sort_keys=False)), ('management-secret', key)]:
        path = directory / name
        path.write_text(content)
        path.chmod(0o600)
    worker_env = {k: v for k, v in os.environ.items() if k.lower() not in ('http_proxy', 'https_proxy', 'all_proxy', 'no_proxy')}
    with open(os.devnull, 'wb') as sink:
        process = subprocess.Popen(['timeout', '--signal=TERM', '3600', str(PRODUCTION / 'cli-proxy-api'),
            '-config', str(directory / 'config.yaml'), '-local-model'], cwd=directory,
            stdin=subprocess.DEVNULL, stdout=sink, stderr=sink, start_new_session=True, env=worker_env)
    meta = {'pid': process.pid, 'port': port, 'email': email, 'proxy': proxy,
        'target_fingerprint': hashlib.sha256(binding['key'].encode()).hexdigest(),
        'egress_ip': probe['egress_ip'], 'mode': 'direct' if direct else 'residential',
        'created': time.time(), 'production_count': len(binding['allow'])}
    (directory / 'worker.json').write_text(json.dumps(meta))
    (directory / 'worker.json').chmod(0o600)
    deadline = time.monotonic() + 15
    while True:
        try:
            worker_api(directory, '/credentials')
            break
        except LoginError:
            if time.monotonic() > deadline or process.poll() is not None:
                raise LoginError('独立授权服务启动失败；生产服务未修改。') from None
            time.sleep(.25)
    return {**probe, 'original_account_count': len(binding['allow']), 'proxy_host': probe['egress_ip'] if direct else parts.hostname, 'mode': 'direct' if direct else 'residential', 'isolated': True}


def verified_credential(directory):
    meta = json.loads((directory / 'worker.json').read_text())
    files = []
    for path in (directory / 'auth').glob('*.json'):
        data = json.loads(path.read_text())
        if data.get('type') in ('claude', 'anthropic') and str(data.get('email', '')).casefold() == meta['email']:
            files.append((path, data))
    if len(files) != 1:
        raise LoginError('尚未找到唯一匹配的登录账号；若已授权，请稍后再次检查结果。')
    path, data = files[0]
    if not data.get('access_token'):
        raise LoginError('账号凭据不完整，未加入生产账号池。')
    if meta['proxy'] == 'direct' and not data.get('proxy_url'):
        worker_config = yaml.safe_load((directory / 'config.yaml').read_text())
        if worker_config.get('requests', {}).get('proxy-url') != 'direct':
            raise LoginError('独立实例不是服务器直连，未加入账号池。')
        # The OAuth API accepts proxy URLs only. Direct mode is provided by the
        # isolated worker config; persist an explicit account override before activation.
        data['proxy_url'] = 'direct'
        pending = path.with_suffix('.direct-pending')
        pending.write_text(json.dumps(data, ensure_ascii=False))
        pending.chmod(0o600)
        os.replace(pending, path)
    if not proxy_matches(data.get('proxy_url', ''), meta['proxy']):
        raise LoginError('登录凭据保存的代理绑定与本次设置不一致，账号仍在独立目录，未加入生产。请重新检查并授权；重复点击不会修改绑定。')
    return path, data, meta


def start(request):
    directory = session_dir(request)
    meta = json.loads((directory / 'worker.json').read_text())
    if (directory / 'verified.json').exists() or list((directory / 'auth').glob('*.json')):
        return status(request)
    old = directory / 'oauth.json'
    if old.exists():
        previous = json.loads(old.read_text())
        if previous.get('submitted'):
            current = status(request)
            if current.get('status') in ('processing', 'verified', 'published'):
                return current
        state = previous.get('state')
        try:
            worker_api(directory, '/oauth/session?' + urllib.parse.urlencode({'state': state}), 'DELETE')
        except LoginError:
            pass
    payload = {'provider': 'claude'}
    if meta['proxy'] != 'direct':
        payload['proxy_url'] = meta['proxy']
    result = worker_api(directory, '/oauth/auth-url', 'POST', payload)
    if not result.get('state') or not result.get('url'):
        raise LoginError('未获得有效授权链接，请重新检查连接。')
    saved = {'state': result['state'], 'deadline': time.time() + 300, 'submitted': False}
    old.write_text(json.dumps(saved))
    old.chmod(0o600)
    return {'state': saved['state'], 'url': result['url'], 'deadline': saved['deadline'], 'expires_in': 300}


def verified_snapshot(directory):
    receipt = json.loads((directory / 'verified.json').read_text())
    name = receipt['credential']
    if pathlib.Path(name).name != name or not name.endswith('.json'):
        raise LoginError('核对快照的凭据文件名无效。')
    path = directory / 'verified' / name
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != receipt['sha256']:
        raise LoginError('核对后的凭据快照发生变化，已阻止发布。')
    data = json.loads(raw)
    meta = json.loads((directory / 'worker.json').read_text())
    if str(data.get('email', '')).casefold() != meta['email'] or not data.get('access_token') or not proxy_matches(data.get('proxy_url', ''), meta['proxy']):
        raise LoginError('核对快照与本次账号或代理不一致，已阻止发布。')
    return path, data, meta


def finalize_credential(directory):
    if not (directory / 'verified.json').exists():
        stop_worker(directory)
        path, data, meta = verified_credential(directory)
        data['proxy_url'] = meta['proxy']
        snapshot = directory / 'verified'
        snapshot.mkdir(mode=0o700, exist_ok=True)
        raw = json.dumps(data, ensure_ascii=False, indent=2).encode()
        atomic_write(snapshot / path.name, raw)
        atomic_write(directory / 'verified.json', json.dumps({'credential': path.name,
                     'sha256': hashlib.sha256(raw).hexdigest()}).encode())
    return verified_snapshot(directory)


def status(request):
    directory = session_dir(request)
    if (directory / 'published.json').exists():
        return json.loads((directory / 'published.json').read_text())
    if (directory / 'verified.json').exists():
        path, data, meta = verified_snapshot(directory)
        return {'status': 'verified', 'email': meta['email'], 'proxy_host': display_exit(meta), 'credential': path.name}
    if not (directory / 'oauth.json').exists():
        return {'status': 'ready'}
    saved = json.loads((directory / 'oauth.json').read_text())
    result = worker_api(directory, '/oauth/status?' + urllib.parse.urlencode({'state': saved['state']}))
    if result.get('status') == 'ok':
        path, data, meta = finalize_credential(directory)
        return {'status': 'verified', 'email': meta['email'], 'proxy_host': display_exit(meta), 'credential': path.name}
    if result.get('status') == 'error':
        message = str(result.get('error', '')).lower()
        if any(word in message for word in ('timeout', 'expired', 'unknown')):
            return {'status': 'expired', 'expires_in': 0, 'message': '授权链接已过期，点击重新生成即可，填写信息已保留。'}
        return {'status': 'failed', 'message': '授权未完成或换取凭据失败。先核对结果，再重新生成链接。'}
    remaining = max(0, saved['deadline'] - time.time())
    if not saved['submitted'] and remaining == 0:
        return {'status': 'expired', 'expires_in': 0, 'message': '授权链接已过期，点击重新生成即可。'}
    return {'status': 'processing' if saved['submitted'] else 'waiting', 'expires_in': remaining, 'deadline': saved['deadline']}


def callback(request):
    directory = session_dir(request)
    saved = json.loads((directory / 'oauth.json').read_text())
    parts = urllib.parse.urlsplit(request['redirect_url'].strip())
    query = urllib.parse.parse_qs(parts.query)
    if parts.hostname not in ('localhost', '127.0.0.1', '::1') or query.get('state', [''])[0] != saved['state']:
        raise LoginError('回调链接不是本次授权的结果，请复制最新授权页面跳转后的完整链接。')
    if query.get('error'):
        raise LoginError('浏览器授权返回失败，请确认登录账号及授权操作。')
    if not query.get('code', [''])[0]:
        raise LoginError('回调缺少授权码，请粘贴地址栏完整链接。')
    if saved['submitted']:
        return {'status': 'processing'}
    if time.time() >= saved['deadline']:
        raise LoginError('此链接已超过五分钟，请点击重新生成，无需重填代理。')
    worker_api(directory, '/oauth/callback', 'POST', {'provider': 'claude', 'redirect_url': request['redirect_url']})
    saved['submitted'] = True
    (directory / 'oauth.json').write_text(json.dumps(saved))
    return {'status': 'processing'}


def stop_worker(directory):
    meta = json.loads((directory / 'worker.json').read_text())
    process = pathlib.Path('/proc') / str(meta['pid'])
    try:
        argv = (process / 'cmdline').read_bytes().split(b'\0')
    except FileNotFoundError:
        return
    if str(directory / 'config.yaml').encode() not in argv:
        raise LoginError('登录进程标识发生变化，没有停止其他进程。')
    os.killpg(meta['pid'], signal.SIGTERM)
    deadline = time.monotonic() + 4
    while True:
        live = False
        for entry in pathlib.Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            try:
                fields = (entry / 'stat').read_text().rsplit(')', 1)[1].split()
                if int(fields[2]) == meta['pid'] and fields[0] != 'Z':
                    live = True
                    break
            except (OSError, ValueError, IndexError):
                continue
        if not live:
            break
        if time.monotonic() >= deadline:
            raise LoginError('独立登录实例尚未停止写入，未开始最终核对，请稍后检查结果。')
        time.sleep(.05)


def atomic_write(path, content, replace=True):
    temporary = path.with_name(path.name + '.' + secrets.token_hex(8) + '.pending')
    try:
        with temporary.open('xb') as stream:
            temporary.chmod(0o600)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        if replace:
            os.replace(temporary, path)
        else:
            os.link(temporary, path)
    finally:
        if temporary.exists():
            temporary.unlink()


def restart_service():
    subprocess.run(['sudo', '-n', 'systemctl', 'restart', 'cliproxy'], check=True, timeout=40,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def wait_for_isolation(config):
    deadline = time.monotonic() + 20
    while True:
        try:
            verify_isolation(config)
            return
        except LoginError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(.3)


def rollback_publish(config_path, original_bytes, new_bytes, target, credential, marker, backup, activated, installed):
    errors = []
    # File cleanup must run independently of restore/restart failures.
    try:
        if installed and target.exists():
            if target.read_bytes() != credential:
                errors.append('生产凭据发生变化，未删除他人修改')
            else:
                target.unlink()
    except OSError:
        errors.append('生产凭据清理失败')
    try:
        if marker.exists():
            marker.unlink()
    except OSError:
        errors.append('发布标记清理失败')
    try:
        current = config_path.read_bytes()
        if current != original_bytes:
            if current != new_bytes:
                errors.append('配置被其他操作修改，未覆盖')
            else:
                atomic_write(config_path, original_bytes)
    except OSError:
        errors.append('原配置恢复失败')
    if activated:
        try:
            restart_service()
            wait_for_isolation(yaml.safe_load(original_bytes))
        except Exception:
            errors.append('原服务重启或隔离检查失败')
    if errors:
        return '加入账号池失败；恢复未完成：' + '；'.join(errors) + '。请检查服务。备份目录：' + str(backup) + '。独立目录中的登录凭据已保留。'
    return '加入账号池失败；生产凭据已清理，原配置已保留或恢复' + ('，原服务已通过隔离检查' if activated else '') + '。独立目录中的登录凭据已保留，可核对后重试。'


def publish(request):
    directory = session_dir(request)
    marker = directory / 'published.json'
    if marker.exists():
        return json.loads(marker.read_text())
    result = status(request)
    if result.get('status') != 'verified':
        raise LoginError('授权尚未完成最终核对，未开始加入账号池。请先检查登录结果。')
    path, data, meta = verified_snapshot(directory)
    marker = directory / 'published.json'
    lock_path = PRODUCTION / '.claude-web-publish.lock'
    with lock_path.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if marker.exists():
            return json.loads(marker.read_text())
        config_path = PRODUCTION / 'config.yaml'
        original_bytes = config_path.read_bytes()
        config = yaml.safe_load(original_bytes)
        binding = target_binding(config, meta['target_fingerprint'])
        verify_isolation(config)
        auth = pathlib.Path(config['oauth']['auth-dir']).resolve()
        target = auth / path.name
        if target.exists() or target.resolve().parent != auth:
            raise LoginError('生产服务已有同名账号，未覆盖现有凭据。')
        for current in auth.glob('*.json'):
            existing = json.loads(current.read_text())
            if str(existing.get('email', '')).casefold() == meta['email']:
                raise LoginError('生产服务已有这个邮箱的账号，请先核对。')
        binding['allow'] = [name for name in binding['allow'] if name != '__empty_pool__.json']
        before_count = len(binding['allow'])
        stamp = datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=9))).strftime('%Y%m%d-%H%M%S')
        backup = BACKUPS / ('cliproxy-before-claude-web-' + stamp + '-' + secrets.token_hex(3))
        backup.mkdir(mode=0o700)
        atomic_write(backup / 'config.yaml', original_bytes)
        binding['allow'].append(path.name)
        isolation_bindings(config)
        new_bytes = yaml.safe_dump(config, sort_keys=False, allow_unicode=True).encode()
        credential = json.dumps(data, ensure_ascii=False, indent=2).encode()
        activated = False
        installed = False
        try:
            if config_path.read_bytes() != original_bytes:
                raise LoginError('生产配置刚刚变化，请重新核对后再加入账号池。')
            atomic_write(target, credential, False)
            installed = True
            activated = True
            atomic_write(config_path, new_bytes)
            restart_service()
            wait_for_isolation(config)
            result = {'status': 'published', 'email': meta['email'], 'proxy_host': display_exit(meta),
                'original_account_count_before': before_count, 'original_account_count_after': len(binding['allow']), 'backup': str(backup)}
            atomic_write(marker, json.dumps(result).encode())
        except Exception:
            message = rollback_publish(config_path, original_bytes, new_bytes, target, credential, marker, backup, activated, installed)
            raise LoginError(message) from None
    try:
        stop_worker(directory)
    except Exception:
        pass
    return result


def dispatch_operation(request):
    action = request.get('action')
    if action == 'check-egress':
        return check_egress(request)
    if action == 'bootstrap':
        return bootstrap(request)
    if action == 'start':
        return start(request)
    if action == 'status':
        return status(request)
    if action == 'callback':
        return callback(request)
    if action == 'publish':
        return publish(request)
    if action == 'stop':
        stop_worker(session_dir(request))
        return {'stopped': True}
    raise LoginError('不支持的登录操作。')


def dispatch(request):
    result = dispatch_operation(request)
    if request.get('action') in ('bootstrap', 'start', 'status', 'callback', 'publish'):
        result['target_key'] = session_target(request)
    return result


if __name__ == '__main__':
    try:
        result = dispatch(json.load(sys.stdin))
        print(json.dumps({'ok': True, 'data': result}, ensure_ascii=False))
    except LoginError as error:
        print(json.dumps({'ok': False, 'error': str(error)}, ensure_ascii=False))
    except Exception:
        print(json.dumps({'ok': False, 'error': '服务器处理未完成，请检查连接后重试；不要重复提交旧回调。'}, ensure_ascii=False))
