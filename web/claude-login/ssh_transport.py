"""Verified SSH connection and loopback SOCKS5 forwarding, with no direct fallback."""
import json
import base64
import hashlib
import http.client
import ipaddress
import ssl
import pathlib
import sys
import select
import shlex
import socket
import socketserver
import struct
import threading
import time

import paramiko
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from ssh_keys import PrivateKeyError, load_private_key as parse_private_key


class TunnelError(Exception):
    def __init__(self, message, code=None, details=None):
        super().__init__(message)
        self.code = code
        self.details = details or {}


class RequireHostTrust(paramiko.MissingHostKeyPolicy):
    def missing_host_key(self, client, hostname, key):
        fingerprint = 'SHA256:' + base64.b64encode(hashlib.sha256(key.asbytes()).digest()).decode().rstrip('=')
        raise TunnelError('尚未验证主机密钥，请按下方步骤在本机 PowerShell 核对并保存，再返回重试。',
                         code='ssh_host_untrusted', details={'host_key_algorithm': key.get_name(), 'fingerprint': fingerprint})


def load_private_key(content):
    try:
        return parse_private_key(content)
    except PrivateKeyError as error:
        raise TunnelError(str(error)) from None


def close_stream(connection):
    try:
        connection.close()
    except (EOFError, OSError, paramiko.SSHException):
        pass


def exact(connection, size):
    result = b''
    while len(result) < size:
        block = connection.recv(size - len(result))
        if not block:
            raise EOFError()
        result += block
    return result


class Forwarder(socketserver.BaseRequestHandler):
    def handle(self):
        client = self.request
        owner = self.server.owner
        channel = None
        try:
            owner.track(client)
            client.settimeout(15)
            if exact(client, 1) != b'\x05':
                return
            methods = exact(client, exact(client, 1)[0])
            if 0 not in methods:
                client.sendall(b'\x05\xff')
                return
            client.sendall(b'\x05\x00')
            version, command, reserved, kind = exact(client, 4)
            if version != 5 or command != 1 or reserved != 0:
                raise TunnelError('SOCKS5 仅支持 TCP CONNECT。')
            if kind == 1:
                host = socket.inet_ntop(socket.AF_INET, exact(client, 4))
            elif kind == 3:
                host = exact(client, exact(client, 1)[0]).decode('idna')
            elif kind == 4:
                host = socket.inet_ntop(socket.AF_INET6, exact(client, 16))
            else:
                raise TunnelError('地址类型无效。')
            port = struct.unpack('!H', exact(client, 2))[0]
            if not owner.active:
                raise TunnelError('SSH 隧道已断开。')
            # Domain names are sent to SSH server unchanged; no local DNS lookup.
            channel = owner.transport.open_channel('direct-tcpip', (host, port), client.getpeername(), timeout=15)
            owner.track(channel)
            client.sendall(b'\x05\x00\x00\x01' + b'\x00' * 6)
            while owner.active:
                ready, _, _ = select.select([client, channel], [], [], 2)
                for source in ready:
                    block = source.recv(65536)
                    if not block:
                        return
                    (channel if source is client else client).sendall(block)
        except (OSError, EOFError, ValueError, paramiko.SSHException, TunnelError):
            try:
                client.sendall(b'\x05\x01\x00\x01' + b'\x00' * 6)
            except OSError:
                pass
        finally:
            owner.untrack(client)
            if channel is not None:
                owner.untrack(channel)
                close_stream(channel)


class SocksServer(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False
    allow_reuse_address = False


class SSHConnection:
    def __init__(self, settings):
        self.client = paramiko.SSHClient()
        self.server = None
        self.closed = False
        self.connections = set()
        self.lock = threading.Lock()
        self.client.load_system_host_keys()
        known = pathlib.Path.home() / '.ssh' / 'known_hosts'
        if known.is_file():
            self.client.load_host_keys(str(known))
        self.client.set_missing_host_key_policy(RequireHostTrust())
        user, host = settings['ssh_target'].split('@', 1)
        password_mode = settings.get('auth_mode') == 'password'
        try:
            content = settings.get('key_content', '') if not password_mode else ''
            private_key = load_private_key(content) if content else None
            self.client.connect(host, port=settings.get('ssh_port', 22), username=user,
                password=settings.get('ssh_password') if password_mode else None,
                key_filename=(settings.get('key_path') or None) if not password_mode and not content else None,
                pkey=private_key, allow_agent=not password_mode and not content, look_for_keys=not password_mode and not content,
                timeout=12, banner_timeout=12, auth_timeout=12)
            self.transport = self.client.get_transport()
            self.transport.set_keepalive(15)
        except TunnelError:
            self.close()
            raise
        except paramiko.BadHostKeyException:
            self.close()
            raise TunnelError('服务器主机密钥与本机记录不一致，请核实服务器身份。') from None
        except paramiko.AuthenticationException:
            self.close()
            raise TunnelError('SSH 认证失败，请核对用户名、密码或私钥。') from None
        except Exception as error:
            self.close()
            if 'known_hosts' in str(error):
                raise TunnelError('尚未验证主机密钥，请按下方步骤在本机 PowerShell 核对并保存。', code='ssh_host_untrusted') from None
            raise TunnelError('SSH 连接失败，请检查服务器地址、端口、私钥和网络。') from None

    @property
    def active(self):
        return not self.closed and self.transport.is_active() and self.transport.is_authenticated()

    def track(self, connection):
        with self.lock:
            if self.closed:
                close_stream(connection)
                raise TunnelError('SSH 隧道已关闭。')
            self.connections.add(connection)

    def untrack(self, connection):
        with self.lock:
            self.connections.discard(connection)

    def start_tunnel(self):
        if not self.active:
            raise TunnelError('SSH 隧道已断开，请重新检查连接。')
        if self.server is None:
            self.server = SocksServer(('127.0.0.1', 0), Forwarder)
            self.server.owner = self
            threading.Thread(target=self.server.serve_forever, daemon=True).start()
        return {'host': '127.0.0.1', 'port': self.server.server_address[1], 'protocol': 'SOCKS5', 'active': self.active}

    def tunnel_egress(self):
        tunnel = self.start_tunnel()
        try:
            with socket.create_connection((tunnel['host'], tunnel['port']), timeout=20) as stream:
                stream.sendall(b'\x05\x01\x00')
                if exact(stream, 2) != b'\x05\x00':
                    raise ValueError()
                domain = b'api.ipify.org'
                stream.sendall(b'\x05\x01\x00\x03' + bytes([len(domain)]) + domain + struct.pack('!H', 443))
                if exact(stream, 10)[1] != 0:
                    raise ValueError()
                with ssl.create_default_context().wrap_socket(stream, server_hostname='api.ipify.org') as tls:
                    tls.sendall(b'GET / HTTP/1.1\r\nHost: api.ipify.org\r\nConnection: close\r\n\r\n')
                    response = http.client.HTTPResponse(tls)
                    response.begin()
                    if response.status != 200:
                        raise ValueError()
                    result = response.read(100).decode().strip()
                    ipaddress.ip_address(result)
                    return result
        except (OSError, EOFError, ValueError, http.client.HTTPException):
            raise TunnelError('SOCKS5 隧道出口检查失败；请检查服务器是否允许 SSH TCP 转发及访问外网。') from None

    def rpc(self, program, request):
        try:
            return self._rpc(program, request)
        except (OSError, EOFError, paramiko.SSHException):
            raise TunnelError('SSH 连接中断或超时，请重新检查连接；已提交回调时先检查结果。') from None

    def _rpc(self, program, request):
        if not self.active:
            raise TunnelError('SSH 隧道已断开，请重新检查连接；不会切换到本机直连。')
        expression = "import io,json,sys;e=json.load(sys.stdin);sys.stdin=io.StringIO(json.dumps(e['request']));exec(compile(e['program'],'claude_web_helper','exec'))"
        channel = self.transport.open_session(timeout=12)
        try:
            timeout = 300 if request.get('action') == 'publish' else 115
            channel.settimeout(timeout)
            channel.exec_command('python3 -c ' + shlex.quote(expression))
            channel.sendall(json.dumps({'program': program, 'request': request}).encode())
            channel.shutdown_write()
            output = bytearray()
            deadline = time.monotonic() + timeout
            while True:
                while channel.recv_ready():
                    output.extend(channel.recv(65536))
                    if len(output) > 1048576:
                        raise TunnelError('服务器响应过长。')
                while channel.recv_stderr_ready():
                    channel.recv_stderr(65536)
                if channel.exit_status_ready() and not channel.recv_ready():
                    break
                if time.monotonic() >= deadline or not self.active:
                    raise TunnelError('SSH 请求超时或连接断开；已提交回调时请先检查结果。')
                time.sleep(.02)
            if channel.recv_exit_status() != 0:
                raise TunnelError('服务器命令失败，请确认已安装 Python3、PyYAML 和服务。')
            try:
                response = json.loads(output)
            except ValueError:
                raise TunnelError('服务器未返回有效结果。') from None
            if not response.get('ok'):
                raise TunnelError(response.get('error', '服务器操作失败。'))
            return response['data']
        finally:
            close_stream(channel)

    def close(self):
        self.closed = True
        if self.server:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        with self.lock:
            for connection in self.connections:
                close_stream(connection)
            self.connections.clear()
        self.client.close()
