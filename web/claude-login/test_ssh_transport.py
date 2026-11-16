"""Synthetic SSH authentication, RPC, SOCKS DNS forwarding and disconnect checks."""
import json
import io
import pathlib
import socket
import struct
import tempfile
import threading
import time
import unittest
from unittest import mock

import paramiko
from ssh_transport import SSHConnection, TunnelError, exact, load_private_key


class TestServer(paramiko.ServerInterface):
    def __init__(self):
        self.destinations = []
        self.forward_ids = set()
        self.forward_allowed = True

    def get_allowed_auths(self, username):
        return 'password,publickey'

    def check_auth_password(self, username, password):
        return paramiko.AUTH_SUCCESSFUL if username == 'tester' and password == 'synthetic-pass' else paramiko.AUTH_FAILED

    def check_auth_publickey(self, username, key):
        return paramiko.AUTH_SUCCESSFUL if key == self.userkey else paramiko.AUTH_FAILED

    def check_channel_request(self, kind, channel_id):
        return paramiko.OPEN_SUCCEEDED if kind == 'session' else paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED

    def check_channel_direct_tcpip_request(self, channel_id, origin, destination):
        if not self.forward_allowed:
            return paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED
        self.destinations.append(destination)
        self.forward_ids.add(channel_id)
        return paramiko.OPEN_SUCCEEDED

    def check_channel_exec_request(self, channel, command):
        def respond():
            data = bytearray()
            while True:
                block = channel.recv(65536)
                if not block:
                    break
                data.extend(block)
            request = json.loads(data)['request']
            channel.sendall(json.dumps({'ok': True, 'data': {'echo': request}}).encode())
            channel.send_exit_status(0)
            channel.shutdown_write()
        threading.Thread(target=respond, daemon=True).start()
        return True


class TransportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = pathlib.Path(self.temp.name)
        self.hostkey = paramiko.RSAKey.generate(2048)
        self.server = TestServer()
        self.server.userkey = paramiko.RSAKey.generate(2048)
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen()
        self.listener.settimeout(3)
        self.port = self.listener.getsockname()[1]
        self.transport = None
        self.stopped = threading.Event()
        def echo(channel):
            try:
                while True:
                    block = channel.recv(65536)
                    if not block:
                        return
                    channel.sendall(block)
            except (OSError, EOFError):
                pass
            finally:
                channel.close()
        def serve():
            try:
                connection, _ = self.listener.accept()
                self.transport = paramiko.Transport(connection)
                self.transport.add_server_key(self.hostkey)
                self.transport.start_server(server=self.server)
                while not self.stopped.is_set() and self.transport.is_active():
                    channel = self.transport.accept(.1)
                    if channel and channel.chanid in self.server.forward_ids:
                        threading.Thread(target=echo, args=(channel,), daemon=True).start()
            except (OSError, EOFError, paramiko.SSHException):
                pass
        self.thread = threading.Thread(target=serve, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop)

    def stop(self):
        self.stopped.set()
        if self.transport:
            self.transport.close()
        self.listener.close()
        self.thread.join(4)

    def connect(self, trust=True, password='synthetic-pass', key=False, uploaded=False):
        known = self.home / '.ssh' / 'known_hosts'
        known.parent.mkdir(exist_ok=True)
        if trust:
            known.write_text('[127.0.0.1]:' + str(self.port) + ' ' + self.hostkey.get_name() + ' ' + self.hostkey.get_base64() + '\n')
        settings = {'ssh_target': 'tester@127.0.0.1', 'ssh_port': self.port,
            'auth_mode': 'password', 'ssh_password': password}
        if key:
            filename = self.home / 'test.pem'
            self.server.userkey.write_private_key_file(str(filename))
            settings.update({'auth_mode': 'key', 'key_path': str(filename)})
        if uploaded:
            content = io.StringIO()
            self.server.userkey.write_private_key(content)
            settings.update({'auth_mode': 'key', 'key_content': content.getvalue()})
        with mock.patch('ssh_transport.pathlib.Path.home', return_value=self.home):
            connection = SSHConnection(settings)
        self.addCleanup(connection.close)
        return connection

    def test_password_rpc_and_server_resolved_socks_domain(self):
        connection = self.connect()
        self.assertEqual(connection.rpc('unused', {'action': 'synthetic'}), {'echo': {'action': 'synthetic'}})
        tunnel = connection.start_tunnel()
        self.assertEqual(tunnel['host'], '127.0.0.1')
        with socket.create_connection((tunnel['host'], tunnel['port']), timeout=3) as client:
            client.sendall(b'\x05\x01\x00')
            self.assertEqual(exact(client, 2), b'\x05\x00')
            domain = b'no-local-dns.invalid'
            client.sendall(b'\x05\x01\x00\x03' + bytes([len(domain)]) + domain + struct.pack('!H', 443))
            self.assertEqual(exact(client, 10)[1], 0)
            client.sendall(b'synthetic-forward-test')
            self.assertEqual(exact(client, 22), b'synthetic-forward-test')
        self.assertEqual(self.server.destinations, [('no-local-dns.invalid', 443)])
        self.transport.close()
        for _ in range(50):
            if not connection.active:
                break
            time.sleep(.02)
        with self.assertRaises(TunnelError):
            connection.rpc('unused', {})
        connection.close()
        with self.assertRaises(OSError):
            socket.create_connection((tunnel['host'], tunnel['port']), timeout=.5)

    def test_server_denied_forwarding_fails_probe_without_direct_fallback(self):
        self.server.forward_allowed = False
        connection = self.connect()
        with self.assertRaisesRegex(TunnelError, 'SOCKS5 隧道出口检查失败'):
            connection.tunnel_egress()
        self.assertEqual(self.server.destinations, [])

    def test_private_key_login(self):
        self.assertTrue(self.connect(key=True).active)

    def test_wrong_password_rejected_without_leaking_it(self):
        with self.assertRaises(TunnelError) as error:
            self.connect(password='wrong-test-secret')
        self.assertNotIn('wrong-test-secret', str(error.exception))
        self.assertIn('认证失败', str(error.exception))

    def test_unknown_host_not_automatically_trusted(self):
        with self.assertRaisesRegex(TunnelError, '尚未验证主机密钥') as error:
            self.connect(trust=False)
        self.assertEqual(error.exception.code, 'ssh_host_untrusted')
        self.assertTrue(error.exception.details['fingerprint'].startswith('SHA256:'))
        self.assertEqual(error.exception.details['host_key_algorithm'], self.hostkey.get_name())
        self.assertFalse((self.home / '.ssh' / 'known_hosts').exists())

    def test_uploaded_key_authenticates_without_key_file(self):
        self.assertTrue(self.connect(uploaded=True).active)
        self.assertFalse((self.home / 'test.pem').exists())


class UploadedKeyTests(unittest.TestCase):
    def test_pkcs8_private_key_is_supported(self):
        from cryptography.hazmat.primitives.asymmetric import rsa
        from cryptography.hazmat.primitives import serialization
        key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        content = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()).decode()
        self.assertEqual(load_private_key(content).get_bits(), 2048)

    def test_encrypted_and_malformed_files_have_redacted_errors(self):
        from cryptography.hazmat.primitives.asymmetric import rsa
        from cryptography.hazmat.primitives import serialization
        key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        content = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.BestAvailableEncryption(b'synthetic-passphrase')).decode()
        with self.assertRaisesRegex(TunnelError, '已加密'):
            load_private_key(content)
        with self.assertRaises(TunnelError) as error:
            load_private_key('synthetic-invalid-private-key')
        self.assertNotIn('synthetic-invalid-private-key', str(error.exception))


if __name__ == '__main__':
    unittest.main()
