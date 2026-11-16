"""Local adapter tests; never contact SSH or production."""
import unittest
import os
import pathlib
import tempfile
from unittest import mock

import app


class LocalAppTests(unittest.TestCase):
    def setUp(self):
        app.STATE.clear()
        patch = mock.patch.object(app, 'SSHConnection')
        self.connection = patch.start().return_value
        self.addCleanup(patch.stop)

    def tearDown(self):
        app.STATE.clear()

    def fields(self):
        return {'ssh_target': 'ubuntu@192.0.2.10', 'email': 'new@example.test',
            'proxy_host': '192.0.2.1', 'proxy_port': 12323, 'proxy_scheme': 'http',
            'proxy_user': 'test@user', 'proxy_password': 'test:p@ss', 'key_path': ''}

    def test_target_key_only_sends_fingerprint_to_server(self):
        fields = {**self.fields(), 'target_key': 'sk-synthetic-target-key'}
        _, payload = app.normalize_fields(fields)
        self.assertEqual(payload['target_fingerprint'], app.hashlib.sha256(b'sk-synthetic-target-key').hexdigest())
        self.assertNotIn('target_key', fields)
        self.assertNotIn('sk-synthetic-target-key', str(payload))

    def test_credentials_are_url_encoded(self):
        settings, payload = app.normalize_fields(self.fields())
        self.assertEqual(payload['proxy'], 'http://test%40user:test%3Ap%40ss@192.0.2.1:12323')

    def test_invalid_fields_fail_before_ssh(self):
        for update in [{'ssh_target': '-o bad'}, {'proxy_port': 65536}, {'email': 'bad'}, {'proxy_password': ''}]:
            with self.subTest(update=update), self.assertRaises(app.UserError):
                app.normalize_fields({**self.fields(), **update})

    def test_failed_new_check_preserves_existing_session(self):
        app.STATE.update({'session': 'old-session', 'settings': {'ssh_target': 'ubuntu@192.0.2.10'}})
        with mock.patch.object(app, 'remote', side_effect=app.UserError('proxy unavailable')) as remote:
            with self.assertRaises(app.UserError):
                app.operation('prepare', self.fields())
        self.assertEqual(app.STATE['session'], 'old-session')
        self.assertEqual(remote.call_args_list[0].args[1]['action'], 'bootstrap')

    def test_no_operations_before_preparation(self):
        with mock.patch.object(app, 'remote') as remote, self.assertRaises(app.UserError):
            app.operation('publish', {})
        remote.assert_not_called()

    def test_successful_preparation_exposes_no_proxy_password(self):
        result = {'egress_ip': '192.0.2.1', 'isolated': True}
        with mock.patch.object(app, 'remote', return_value=result):
            app.operation('prepare', self.fields())
        self.assertEqual(app.STATE['public'], result)
        self.assertNotIn('proxy', app.STATE)
        self.assertNotIn('password', app.STATE)

    def test_direct_password_does_not_require_residential_fields(self):
        fields = {'ssh_target': 'tester@192.0.2.10', 'email': 'new@example.test',
            'connection_mode': 'direct', 'auth_mode': 'password', 'ssh_password': 'synthetic-password'}
        settings, payload = app.normalize_fields(fields)
        self.assertEqual(payload['proxy'], 'direct')
        self.assertEqual(settings['ssh_password'], 'synthetic-password')
        self.assertNotIn('ssh_password', fields)

    def test_egress_confirmation_is_enforced_in_backend(self):
        app.STATE.update({'session': 'session', 'public': {'egress_ip': '192.0.2.1'}, 'settings': {}})
        with mock.patch.object(app, 'remote') as remote:
            with self.assertRaises(app.UserError):
                app.operation('start', {})
            with self.assertRaises(app.UserError):
                app.operation('confirm-egress', {'browser_ip': '192.0.2.2'})
            remote.assert_not_called()
            app.operation('confirm-egress', {'browser_ip': '192.0.2.1'})
            app.operation('start', {})
            self.assertEqual([c.args[1]['action'] for c in remote.call_args_list], ['check-egress', 'check-egress', 'start'])

    def test_disconnected_transport_blocks_callback(self):
        app.STATE.update({'session': 'session', 'confirmed': True, 'settings': {}})
        with self.assertRaises(app.UserError):
            app.operation('callback', {'redirect_url': 'http://localhost/test'})

    def test_failed_tunnel_check_keeps_previous_session(self):
        app.STATE.update({'session': 'previous', 'settings': {'ssh_target': 'ubuntu@192.0.2.10'}})
        self.connection.tunnel_egress.return_value = '192.0.2.2'
        with mock.patch.object(app, 'remote', return_value={'egress_ip': '192.0.2.1'}):
            with self.assertRaises(app.UserError):
                app.operation('prepare', {**self.fields(), 'connection_mode': 'direct'})
        self.assertEqual(app.STATE['session'], 'previous')
        self.connection.close.assert_called_once()

    def test_prepare_never_keeps_ssh_password_in_state(self):
        fields = {**self.fields(), 'auth_mode': 'password', 'ssh_password': 'synthetic-secret'}
        with mock.patch.object(app, 'remote', return_value={'egress_ip': '192.0.2.1'}):
            app.operation('prepare', fields)
        self.assertNotIn('ssh_password', app.STATE['settings'])
        self.assertNotIn('ssh_password', fields)

    def test_tilde_and_relative_key_paths_pass_expanded_absolute_path(self):
        with tempfile.TemporaryDirectory(dir=pathlib.Path.cwd()) as directory:
            home = pathlib.Path(directory)
            key = home / 'test.pem'
            key.write_text('synthetic-key')
            with mock.patch.dict(os.environ, {'USERPROFILE': str(home), 'HOME': str(home)}):
                settings, _ = app.normalize_fields({**self.fields(), 'key_path': '~/test.pem'})
            self.assertEqual(pathlib.Path(settings['key_path']), key.resolve())
            relative = os.path.relpath(key)
            settings, _ = app.normalize_fields({**self.fields(), 'key_path': relative})
            self.assertEqual(pathlib.Path(settings['key_path']), key.resolve())

    def test_uploaded_private_key_removed_from_payload_and_session(self):
        fields = {**self.fields(), 'key_content': 'synthetic-private-key'}
        with mock.patch.object(app, 'remote', return_value={'egress_ip': '192.0.2.1'}):
            app.operation('prepare', fields)
        self.assertNotIn('key_content', fields)
        self.assertNotIn('key_content', app.STATE['settings'])
        self.assertNotIn('synthetic-private-key', str(app.STATE))

    def test_uploaded_key_is_not_retained_on_connection_failure(self):
        captured = {}
        def fail(settings):
            captured['settings'] = settings
            raise app.TunnelError('synthetic failure')
        fields = {**self.fields(), 'key_content': 'synthetic-private-key'}
        with mock.patch.object(app, 'SSHConnection', side_effect=fail), self.assertRaises(app.UserError):
            app.operation('prepare', fields)
        self.assertNotIn('key_content', captured['settings'])
        self.assertNotIn('key_content', fields)

    def test_large_uploaded_keys_are_rejected_before_ssh(self):
        with self.assertRaises(app.UserError):
            app.normalize_fields({**self.fields(), 'key_content': 'a' * 32769})

    def test_untrusted_host_help_survives_error_wrapping(self):
        details = {'host_key_algorithm': 'ssh-ed25519', 'fingerprint': 'SHA256:synthetic'}
        error = app.TunnelError('尚未验证主机密钥', code='ssh_host_untrusted', details=details)
        with mock.patch.object(app, 'SSHConnection', side_effect=error), self.assertRaises(app.UserError) as result:
            app.operation('prepare', self.fields())
        self.assertEqual(result.exception.code, 'ssh_host_untrusted')
        self.assertEqual(result.exception.details, details)


if __name__ == '__main__':
    unittest.main()
