"""Password authentication through a real local SSH server; no production changes."""
import json
import io
import pathlib
import socket
import tempfile
import threading
import unittest
from unittest import mock

import paramiko

from adapters import ReadOnlyProduction
from domain import Problem
from service import Service


class LocalSSH(paramiko.ServerInterface):
    def __init__(self):
        self.command = threading.Event()
        self.requests = []

    def get_allowed_auths(self, username):
        return 'password,publickey'

    def check_auth_password(self, username, password):
        return paramiko.AUTH_SUCCESSFUL if (username, password) == ('tester', 'local-test-secret') else paramiko.AUTH_FAILED

    def check_auth_publickey(self, username, key):
        return paramiko.AUTH_SUCCESSFUL if username == 'tester' and key == self.userkey else paramiko.AUTH_FAILED

    def check_channel_request(self, kind, channel_id):
        return paramiko.OPEN_SUCCEEDED if kind == 'session' else paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED

    def check_channel_exec_request(self, channel, command):
        self.command.set()
        return True


class SSHTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = pathlib.Path(self.temp.name)
        self.adapter = ReadOnlyProduction()

    def test_modes_validate_atomically_and_clear_unused_secrets(self):
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'password', 'ssh_password': 'local-test-secret', 'key_path': 'nonexistent'})
        with self.assertRaises(Problem):
            self.adapter.configure({'ssh_target': 'other@127.0.0.1', 'auth_mode': 'password', 'ssh_password': ''})
        self.assertEqual(self.adapter.target, 'tester@127.0.0.1')
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key'})
        self.assertEqual(self.adapter._password, '')
        self.assertEqual(self.adapter.auth_mode, 'key')

    def test_password_never_returned_persisted_or_kept_in_job_payload(self):
        service = Service(self.home / 'data', 'missing.exe', production=self.adapter)
        payload = {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'password', 'ssh_password': 'local-test-secret'}
        result = service.perform('connection', payload)
        self.assertNotIn('ssh_password', payload)
        visible = json.dumps([result, service.overview(), service.store.export_plan()])
        self.assertNotIn('local-test-secret', visible)
        self.assertTrue(service.overview()['ssh_password_set'])
        self.assertNotIn('local-test-secret', service.store.path.read_text(encoding='utf-8'))
        service.close()
        self.assertEqual(self.adapter._password, '')

    def test_key_transport_keeps_batchmode_and_never_sends_password(self):
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'ssh_password': 'unused'})
        with mock.patch('adapters.subprocess.run', return_value=mock.Mock(returncode=0, stdout=b'{"keys":[]}')) as run:
            self.assertEqual(self.adapter.snapshot(), {'keys': []})
            args, kwargs = run.call_args
            self.assertIn('BatchMode=yes', args[0])
            self.assertNotIn('unused', repr((args, kwargs)))

    def local_query(self, *, password='local-test-secret', trust='correct', uploaded=False):
        hostkey = paramiko.RSAKey.generate(2048)
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen(1)
        listener.settimeout(5)
        port = listener.getsockname()[1]
        server = LocalSSH()
        server.userkey = paramiko.RSAKey.generate(2048)
        errors = []
        def serve():
            transport = None
            try:
                conn, _ = listener.accept()
                transport = paramiko.Transport(conn)
                transport.add_server_key(hostkey)
                transport.start_server(server=server)
                channel = transport.accept(5)
                if channel is None:
                    return
                if not server.command.wait(5):
                    raise AssertionError('No exec request')
                data = b''
                while True:
                    chunk = channel.recv(65536)
                    if not chunk:
                        break
                    data += chunk
                server.requests.append(json.loads(data))
                channel.sendall(b'{"keys":[],"accounts":[],"local_test":true}')
                channel.send_exit_status(0)
                channel.shutdown_write()
                # Keep transport alive until the client consumes the result.
                channel.recv_exit_status()
            except (EOFError, OSError, paramiko.SSHException):
                pass
            except Exception as error:
                errors.append(error)
            finally:
                if transport:
                    transport.close()
                listener.close()
        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        hosts = self.home / '.ssh' / 'known_hosts'
        hosts.parent.mkdir(exist_ok=True)
        if trust != 'missing':
            key = hostkey if trust == 'correct' else paramiko.RSAKey.generate(2048)
            hosts.write_text('[127.0.0.1]:' + str(port) + ' ' + key.get_name() + ' ' + key.get_base64() + '\n')
        fields = {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'password', 'ssh_password': password}
        if uploaded:
            stream = io.StringIO()
            server.userkey.write_private_key(stream)
            fields = {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key', 'key_content': stream.getvalue()}
        self.adapter.configure(fields)
        original = paramiko.SSHClient.connect
        def connect(client, hostname, **kwargs):
            self.assertFalse(kwargs['allow_agent'])
            self.assertFalse(kwargs['look_for_keys'])
            return original(client, hostname, port=port, **kwargs)
        try:
            with mock.patch('adapters.pathlib.Path.home', return_value=self.home), mock.patch.object(paramiko.SSHClient, 'connect', connect):
                return self.adapter.snapshot(), server
        finally:
            thread.join(6)
            self.assertFalse(thread.is_alive())
            self.assertFalse(errors, errors)

    def test_real_local_password_auth_and_remote_json_roundtrip(self):
        result, server = self.local_query()
        self.assertTrue(result['local_test'])
        self.assertEqual(server.requests, [{'action': 'snapshot'}])

    def test_wrong_password_has_clear_redacted_error(self):
        with self.assertRaisesRegex(Problem, 'SSH 认证失败') as error:
            self.local_query(password='wrong-secret')
        self.assertNotIn('wrong-secret', str(error.exception))

    def test_unknown_host_is_not_auto_trusted(self):
        with self.assertRaisesRegex(Problem, '尚未验证主机密钥'):
            self.local_query(trust='missing')

    def test_changed_host_key_is_rejected(self):
        with self.assertRaisesRegex(Problem, '主机密钥与本机记录不一致'):
            self.local_query(trust='changed')

    def key_content(self):
        key = paramiko.RSAKey.generate(2048)
        stream = io.StringIO()
        key.write_private_key(stream)
        return stream.getvalue()

    def test_real_local_uploaded_key_and_readonly_request(self):
        result, server = self.local_query(uploaded=True)
        self.assertTrue(result['local_test'])
        self.assertEqual(server.requests, [{'action': 'snapshot'}])
        self.assertIsNotNone(self.adapter._private_key)
        self.assertFalse(list(self.home.rglob('*.pem')))

    def test_uploaded_private_key_is_memory_only_and_clearable(self):
        service = Service(self.home / 'data', 'missing.exe', production=self.adapter)
        content = self.key_content()
        payload = {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key', 'key_content': content}
        result = service.perform('connection', payload)
        self.assertNotIn('key_content', payload)
        self.assertTrue(service.overview()['ssh_key_loaded'])
        visible = json.dumps([result, service.overview(), service.store.export_plan()])
        self.assertNotIn('BEGIN RSA PRIVATE KEY', visible)
        self.assertNotIn(content, service.store.path.read_text(encoding='utf-8'))
        self.assertFalse(list((self.home / 'data').rglob('*.pem')))
        original = self.adapter._private_key
        service.perform('connection', {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key'})
        self.assertIs(self.adapter._private_key, original)
        service.perform('connection', {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key', 'clear_uploaded_key': True})
        self.assertFalse(service.overview()['ssh_key_loaded'])
        service.perform('connection', {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key', 'key_content': content})
        service.close()
        self.assertIsNone(self.adapter._private_key)

    def test_invalid_upload_keeps_existing_configuration_and_cleans_payload(self):
        service = Service(self.home / 'data', 'missing.exe', production=self.adapter)
        service.perform('connection', {'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'key', 'key_content': self.key_content()})
        original = self.adapter._private_key
        for value in ['synthetic-invalid-private-key', 'x' * 32769]:
            fields = {'ssh_target': 'other@127.0.0.1', 'auth_mode': 'key', 'key_content': value}
            with self.assertRaises(Problem):
                service.perform('connection', fields)
            self.assertEqual(self.adapter.target, 'tester@127.0.0.1')
            self.assertIs(self.adapter._private_key, original)
            self.assertNotIn('key_content', fields)
        service.close()

    def test_new_target_and_password_mode_clear_uploaded_key(self):
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'key_content': self.key_content()})
        self.adapter.configure({'ssh_target': 'other@127.0.0.1', 'auth_mode': 'key'})
        self.assertIsNone(self.adapter._private_key)
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'key_content': self.key_content()})
        self.adapter.configure({'ssh_target': 'tester@127.0.0.1', 'auth_mode': 'password', 'ssh_password': 'synthetic'})
        self.assertIsNone(self.adapter._private_key)


if __name__ == '__main__':
    unittest.main()
