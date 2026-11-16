"""Run on Linux. Mock OAuth and restart operations; use synthetic credentials."""
import json
import os
import sys
import pathlib
import tempfile
import time
import subprocess
import urllib.error
import unittest
from unittest import mock

import yaml
import remote_helper as h


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.production = self.root / 'production'
        self.production.mkdir()
        self.auth = self.root / 'production-auth'
        self.auth.mkdir()
        self.sessions = self.root / 'sessions'
        self.sid = 'a' * 32
        self.directory = self.sessions / self.sid
        (self.directory / 'auth').mkdir(parents=True)
        self.config = {'oauth': {'auth-dir': str(self.auth)}, 'routing': {'session-affinity': True},
            'observability': {'logs': {'logs-max-total-size-mb': 100}},
            'plugins': {'enabled': True, 'configs': {'key-account-bind': {'enabled': True, 'unbound': 'deny', 'bindings': [
                {'key': 'synthetic-original-key', 'allow': ['old.json']},
                {'key': 'synthetic-other-key', 'allow': ['other.json']}]}}}}
        (self.production / 'config.yaml').write_text(yaml.safe_dump(self.config))
        self.old = {'type': 'claude', 'email': 'old@example.test', 'proxy_url': 'http://old-proxy:1234'}
        (self.auth / 'old.json').write_text(json.dumps(self.old))
        self.proxy = 'http://test-user:test-password@192.0.2.1:12323'
        self.new = {'type': 'claude', 'email': 'new@example.test', 'proxy_url': self.proxy, 'access_token': 'synthetic-token'}
        self.meta = {'email': 'new@example.test', 'proxy': self.proxy,
            'target_fingerprint': h.hashlib.sha256(b'synthetic-original-key').hexdigest(), 'pid': 2147483647}
        (self.directory / 'worker.json').write_text(json.dumps(self.meta))
        for attr, value in [('PRODUCTION', self.production), ('SESSIONS', self.sessions), ('BACKUPS', self.root)]:
            patch = mock.patch.object(h, attr, value)
            patch.start()
            self.addCleanup(patch.stop)

        api_patch = mock.patch.object(h, 'worker_api', return_value={'status': 'ok'})
        api_patch.start()
        self.addCleanup(api_patch.stop)
        patch = mock.patch.object(h.OPENER, 'open', side_effect=self.runtime_response)
        patch.start()
        self.addCleanup(patch.stop)

    def runtime_response(self, request, **kwargs):
        config = yaml.safe_load((self.production / 'config.yaml').read_text())
        key = request.get_header('Authorization').split(' ', 1)[1]
        binding = next(b for b in config['plugins']['configs']['key-account-bind']['bindings'] if b['key'] == key)
        rows = [{'id': h.account_usage_id(key, name)} for name in binding['allow'] if name != '__empty_pool__.json' and (self.auth / name).is_file()]
        response = mock.MagicMock()
        response.__enter__.return_value.status = 200
        response.__enter__.return_value.read.return_value = json.dumps({'accounts': rows}).encode()
        return response

    def save_credential(self):
        if not (self.directory / 'oauth.json').exists():
            self.oauth()
        (self.directory / 'auth' / 'new.json').write_text(json.dumps(self.new))

    def oauth(self, deadline=None):
        data = {'state': 'current-state', 'deadline': deadline or time.time() + 300, 'submitted': False}
        (self.directory / 'oauth.json').write_text(json.dumps(data))

    def test_callback_wrong_state_never_submits(self):
        self.oauth()
        with mock.patch.object(h, 'worker_api') as api, self.assertRaises(h.LoginError):
            h.callback({'session': self.sid, 'redirect_url': 'http://localhost:54545/callback?code=x&state=old'})
        api.assert_not_called()

    def test_expired_callback_never_submits(self):
        self.oauth(time.time() - 1)
        with mock.patch.object(h, 'worker_api') as api, self.assertRaises(h.LoginError):
            h.callback({'session': self.sid, 'redirect_url': 'http://localhost:54545/callback?code=x&state=current-state'})
        api.assert_not_called()

    def test_callback_is_idempotent(self):
        self.oauth()
        with mock.patch.object(h, 'worker_api', return_value={'status': 'ok'}) as api:
            for _ in range(2):
                h.callback({'session': self.sid, 'redirect_url': 'http://localhost:54545/callback?code=x&state=current-state'})
        self.assertEqual(api.call_count, 1)

    def test_retry_cancels_previous_state_and_keeps_proxy(self):
        self.oauth()
        with mock.patch.object(h, 'worker_api', side_effect=[{'cancelled': True}, {'state': 'new-state', 'url': 'https://claude.ai/test'}]) as api:
            result = h.start({'session': self.sid})
        self.assertEqual(result['state'], 'new-state')
        self.assertEqual(api.call_args_list[1].args[3]['proxy_url'], self.proxy)
        self.assertEqual(api.call_args_list[0].args[2], 'DELETE')

    def test_wrong_proxy_cannot_publish(self):
        self.new['proxy_url'] = 'http://wrong.example:80'
        self.save_credential()
        with self.assertRaises(h.LoginError):
            h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())

    def test_multiple_pools_require_explicit_target(self):
        with self.assertRaises(h.LoginError):
            h.target_binding(self.config)
        digest = h.hashlib.sha256(b'synthetic-other-key').hexdigest()
        self.assertEqual(h.target_binding(self.config, digest)['key'], 'synthetic-other-key')

    def test_first_account_replaces_empty_pool_marker(self):
        self.config['plugins']['configs']['key-account-bind']['bindings'] = [
            {'key': 'synthetic-original-key', 'allow': ['__empty_pool__.json']}]
        (self.production / 'config.yaml').write_text(yaml.safe_dump(self.config))
        self.save_credential()
        response = mock.MagicMock()
        response.__enter__.return_value.status = 200
        with mock.patch.object(h.subprocess, 'run'):
            result = h.publish({'session': self.sid})
        current = yaml.safe_load((self.production / 'config.yaml').read_text())
        self.assertEqual(current['plugins']['configs']['key-account-bind']['bindings'][0]['allow'], ['new.json'])
        self.assertEqual(result['original_account_count_before'], 0)

    def test_wildcard_binding_is_rejected(self):
        self.config['plugins']['configs']['key-account-bind']['bindings'][0]['allow'] = ['*.json']
        with self.assertRaises(h.LoginError):
            h.target_binding(self.config)

    def test_publish_preserves_existing_account_and_other_pool(self):
        self.save_credential()
        response = mock.MagicMock()
        response.__enter__.return_value.status = 200
        with mock.patch.object(h.subprocess, 'run'):
            result = h.publish({'session': self.sid})
        config = yaml.safe_load((self.production / 'config.yaml').read_text())
        self.assertEqual(config['plugins']['configs']['key-account-bind']['bindings'][0]['allow'], ['old.json', 'new.json'])
        self.assertEqual(config['plugins']['configs']['key-account-bind']['bindings'][1], self.config['plugins']['configs']['key-account-bind']['bindings'][1])
        self.assertEqual(json.loads((self.auth / 'old.json').read_text()), self.old)
        self.assertEqual(json.loads((self.auth / 'new.json').read_text())['proxy_url'], self.proxy)
        self.assertEqual(config['observability'], self.config['observability'])
        self.assertEqual(result['original_account_count_after'], 2)
        with mock.patch.object(h.subprocess, 'run') as restart:
            again = h.publish({'session': self.sid})
        restart.assert_not_called()
        self.assertEqual(again, result)

    def test_restart_failure_restores_original_configuration(self):
        self.save_credential()
        before = (self.production / 'config.yaml').read_bytes()
        with mock.patch.object(h.subprocess, 'run', side_effect=[RuntimeError('restart failed'), mock.MagicMock()]):
            with self.assertRaises(h.LoginError):
                h.publish({'session': self.sid})
        self.assertEqual((self.production / 'config.yaml').read_bytes(), before)
        self.assertFalse((self.auth / 'new.json').exists())
        self.assertTrue((self.directory / 'auth' / 'new.json').exists())

    def direct_meta(self):
        self.meta.update({'proxy': 'direct', 'egress_ip': '192.0.2.50'})
        (self.directory / 'worker.json').write_text(json.dumps(self.meta))
        (self.directory / 'config.yaml').write_text(yaml.safe_dump({'requests': {'proxy-url': 'direct'}}))

    def test_direct_oauth_uses_worker_config_not_rejected_override(self):
        self.direct_meta()
        with mock.patch.object(h, 'worker_api', return_value={'state': 's', 'url': 'https://example.test'}) as api:
            h.start({'session': self.sid})
        self.assertEqual(api.call_args.args[3], {'provider': 'claude'})

    def test_direct_credential_gets_explicit_override_in_isolation(self):
        self.direct_meta()
        self.new.pop('proxy_url')
        self.save_credential()
        path, data, meta = h.verified_credential(self.directory)
        self.assertEqual(data['proxy_url'], 'direct')
        self.assertEqual(json.loads(path.read_text())['proxy_url'], 'direct')
        self.assertFalse((self.auth / 'new.json').exists())

    def test_direct_wrong_worker_or_credential_is_rejected(self):
        self.direct_meta()
        self.save_credential()
        with self.assertRaises(h.LoginError):
            h.verified_credential(self.directory)
        self.new.pop('proxy_url')
        self.save_credential()
        (self.directory / 'config.yaml').write_text(yaml.safe_dump({'requests': {'proxy-url': 'http://wrong'}}))
        with self.assertRaises(h.LoginError):
            h.verified_credential(self.directory)

    def test_changed_egress_rejected(self):
        self.direct_meta()
        with mock.patch.object(h, 'probe_direct', return_value={'egress_ip': '192.0.2.51'}), self.assertRaises(h.LoginError):
            h.check_egress({'session': self.sid})

    def test_disabled_or_passthrough_plugin_blocked_before_write(self):
        self.save_credential()
        for global_enabled, local_enabled, unbound in [(False, True, 'deny'), (True, False, 'deny'), (True, True, 'passthrough')]:
            self.config['plugins']['enabled'] = global_enabled
            self.config['plugins']['configs']['key-account-bind'].update(enabled=local_enabled, unbound=unbound)
            (self.production / 'config.yaml').write_text(yaml.safe_dump(self.config))
            with mock.patch.object(h, 'restart_service') as restart, self.assertRaises(h.LoginError):
                h.publish({'session': self.sid})
            restart.assert_not_called()
            self.assertFalse((self.auth / 'new.json').exists())

    def test_runtime_plugin_missing_or_wrong_scope_blocks_before_write(self):
        self.save_credential()
        for payload in [{'accounts': [{'id': 'account-outside'}]}, {'accounts': []}, {'not_accounts': []}]:
            response = mock.MagicMock()
            response.__enter__.return_value.status = 200
            response.__enter__.return_value.read.return_value = json.dumps(payload).encode()
            with mock.patch.object(h.OPENER, 'open', return_value=response), mock.patch.object(h, 'restart_service') as restart, self.assertRaises(h.LoginError):
                h.publish({'session': self.sid})
            restart.assert_not_called()
            self.assertFalse((self.auth / 'new.json').exists())
        with mock.patch.object(h.OPENER, 'open', side_effect=OSError()), self.assertRaises(h.LoginError):
            h.publish({'session': self.sid})

    def test_runtime_all_keys_are_checked(self):
        h.verify_isolation(self.config)
        headers = [call.args[0].get_header('Authorization') for call in h.OPENER.open.call_args_list]
        self.assertEqual(set(headers), {'Bearer synthetic-original-key', 'Bearer synthetic-other-key'})

    def test_shared_future_filename_is_rejected_before_install(self):
        self.save_credential()
        self.config['plugins']['configs']['key-account-bind']['bindings'][1]['allow'].append('new.json')
        (self.production / 'config.yaml').write_text(yaml.safe_dump(self.config))
        with mock.patch.object(h, 'restart_service') as restart, self.assertRaises(h.LoginError):
            h.publish({'session': self.sid})
        restart.assert_not_called()
        self.assertFalse((self.auth / 'new.json').exists())

    def test_rollback_timeout_still_removes_new_credential(self):
        self.save_credential()
        original = (self.production / 'config.yaml').read_bytes()
        with mock.patch.object(h.subprocess, 'run', side_effect=[subprocess.CalledProcessError(1, 'restart'), subprocess.TimeoutExpired('restart', 40)]):
            with self.assertRaisesRegex(h.LoginError, '恢复未完成'):
                h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())
        self.assertEqual((self.production / 'config.yaml').read_bytes(), original)
        self.assertFalse((self.directory / 'published.json').exists())

    def test_rollback_nonzero_not_reported_as_recovered(self):
        self.save_credential()
        with mock.patch.object(h.subprocess, 'run', side_effect=[subprocess.CalledProcessError(1, 'restart'), subprocess.CalledProcessError(1, 'restore')]):
            with self.assertRaisesRegex(h.LoginError, '原服务重启或隔离检查失败'):
                h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())

    def test_rollback_verifies_service_after_successful_restart(self):
        self.save_credential()
        with mock.patch.object(h, 'restart_service', side_effect=[RuntimeError(), None]), mock.patch.object(h, 'wait_for_isolation', side_effect=h.LoginError('not ready')):
            with self.assertRaisesRegex(h.LoginError, '恢复未完成'):
                h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())

    def test_marker_failure_rolls_back_and_retry_does_not_lie(self):
        self.save_credential()
        original_write = h.atomic_write
        def fail_marker(path, data, *args):
            if path.name == 'published.json':
                raise OSError('synthetic disk error')
            return original_write(path, data, *args)
        with mock.patch.object(h, 'atomic_write', side_effect=fail_marker), mock.patch.object(h, 'restart_service'):
            with self.assertRaises(h.LoginError):
                h.publish({'session': self.sid})
        self.assertFalse((self.directory / 'published.json').exists())
        self.assertFalse((self.auth / 'new.json').exists())
        with mock.patch.object(h, 'restart_service'):
            self.assertEqual(h.publish({'session': self.sid})['status'], 'published')

    def test_atomic_restore_failure_still_cleans_credential(self):
        self.save_credential()
        original_write = h.atomic_write
        config_path = self.production / 'config.yaml'
        original_bytes = config_path.read_bytes()
        def fail_restore(path, data, *args):
            if path == config_path and data == original_bytes:
                raise OSError()
            return original_write(path, data, *args)
        with mock.patch.object(h, 'atomic_write', side_effect=fail_restore), mock.patch.object(h, 'restart_service', side_effect=RuntimeError()):
            with self.assertRaisesRegex(h.LoginError, '原配置恢复失败'):
                h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())

    def test_concurrent_target_creation_is_not_overwritten_or_removed(self):
        self.save_credential()
        target = self.auth / 'new.json'
        other = b'{"email":"other@example.test","access_token":"synthetic-other"}'
        original_write = h.atomic_write
        def concurrent_write(path, data, *args):
            if path == target:
                target.write_bytes(other)
            return original_write(path, data, *args)
        with mock.patch.object(h, 'atomic_write', side_effect=concurrent_write), mock.patch.object(h, 'restart_service') as restart:
            with self.assertRaises(h.LoginError):
                h.publish({'session': self.sid})
        restart.assert_not_called()
        self.assertEqual(target.read_bytes(), other)
        self.assertFalse((self.directory / 'published.json').exists())

    def test_credentials_are_not_read_before_oauth_completes(self):
        self.oauth()
        (self.directory / 'auth' / 'new.json').write_text('{partial')
        with mock.patch.object(h, 'worker_api', return_value={'status': 'wait'}):
            self.assertEqual(h.status({'session': self.sid})['status'], 'waiting')
        self.assertFalse((self.directory / 'verified.json').exists())

    def test_expiry_is_explicit_and_regeneration_preserves_settings(self):
        self.oauth(time.time() - 1)
        with mock.patch.object(h, 'worker_api', return_value={'status': 'wait'}):
            self.assertEqual(h.status({'session': self.sid})['status'], 'expired')
        with mock.patch.object(h, 'worker_api', side_effect=[{'cancelled': True}, {'state': 'new', 'url': 'https://example.test'}]):
            self.assertEqual(h.start({'session': self.sid})['expires_in'], 300)
        self.assertEqual(json.loads((self.directory / 'worker.json').read_text())['proxy'], self.proxy)

    def test_processing_callback_cannot_be_replaced_by_new_oauth(self):
        self.oauth()
        saved = json.loads((self.directory / 'oauth.json').read_text())
        saved['submitted'] = True
        (self.directory / 'oauth.json').write_text(json.dumps(saved))
        with mock.patch.object(h, 'worker_api', return_value={'status': 'wait'}) as api:
            self.assertEqual(h.start({'session': self.sid})['status'], 'processing')
        self.assertEqual(api.call_count, 1)
        self.assertTrue(api.call_args.args[1].startswith('/oauth/status?'))

    def test_stop_writer_then_finalize_snapshot(self):
        self.save_credential()
        with mock.patch.object(h, 'stop_worker') as stop:
            result = h.status({'session': self.sid})
        stop.assert_called_once()
        self.assertEqual(result['status'], 'verified')
        self.new['proxy_url'] = 'http://changed.example:80'
        (self.directory / 'auth' / 'new.json').write_text(json.dumps(self.new))
        self.assertEqual(h.verified_snapshot(self.directory)[1]['proxy_url'], self.proxy)
        with mock.patch.object(h, 'restart_service'):
            self.assertEqual(h.publish({'session': self.sid})['status'], 'published')
        self.assertEqual(json.loads((self.auth / 'new.json').read_text())['proxy_url'], self.proxy)

    def test_equivalent_proxy_encoding_and_slash_do_not_mismatch(self):
        self.new['proxy_url'] = 'http://test%2Duser:test%2Dpassword@192.0.2.1:12323/'
        self.save_credential()
        self.assertEqual(h.status({'session': self.sid})['status'], 'verified')
        self.assertEqual(h.verified_snapshot(self.directory)[1]['proxy_url'], self.proxy)

    def test_true_proxy_or_password_mismatch_never_finalizes(self):
        for proxy in ['http://test-user:wrong@192.0.2.1:12323', 'http://test-user:test-password@192.0.2.2:12323']:
            self.new['proxy_url'] = proxy
            self.save_credential()
            with mock.patch.object(h.time, 'sleep'), self.assertRaises(h.LoginError):
                h.status({'session': self.sid})
            self.assertFalse((self.directory / 'verified.json').exists())

    def test_modified_snapshot_never_publishes(self):
        self.save_credential()
        h.status({'session': self.sid})
        snapshot = self.directory / 'verified' / 'new.json'
        snapshot.write_text('{}')
        with self.assertRaises(h.LoginError):
            h.publish({'session': self.sid})
        self.assertFalse((self.auth / 'new.json').exists())

    @unittest.skipUnless(os.name == 'posix', 'Requires Linux process groups')
    def test_real_isolated_writer_is_stopped_before_snapshot(self):
        config = self.directory / 'config.yaml'
        config.write_text('synthetic')
        process = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(30)', str(config)], start_new_session=True)
        self.addCleanup(lambda: (process.kill(), process.wait()) if process.poll() is None else None)
        meta = dict(self.meta, pid=process.pid)
        (self.directory / 'worker.json').write_text(json.dumps(meta))
        h.stop_worker(self.directory)
        process.wait(timeout=5)
        self.assertIsNotNone(process.returncode)

    def test_public_target_matches_actual_server_binding_without_key(self):
        fingerprint = h.hashlib.sha256(b'synthetic-other-key').hexdigest()
        result = h.describe_target(self.config, fingerprint)
        self.assertEqual(result['pool_label'], '账号池 2')
        self.assertEqual(result['fingerprint'], fingerprint)
        self.assertNotIn('synthetic-other-key', json.dumps(result))
        self.assertNotIn('synthetic-original-key', json.dumps(result))

    def test_target_display_missing_binding_does_not_fall_back(self):
        with self.assertRaises(h.LoginError):
            h.describe_target(self.config, '0' * 64)
        del self.meta['target_fingerprint']
        (self.directory / 'worker.json').write_text(json.dumps(self.meta))
        with self.assertRaises(h.LoginError):
            h.session_target({'session': self.sid})

    def test_same_suffix_targets_have_distinct_fingerprints(self):
        bindings = self.config['plugins']['configs']['key-account-bind']['bindings']
        bindings[0]['key'], bindings[1]['key'] = 'sk-test-alpha396', 'sk-test-bravo396'
        results = [h.describe_target(self.config, h.hashlib.sha256(b['key'].encode()).hexdigest()) for b in bindings]
        self.assertEqual(results[0]['suffix'], results[1]['suffix'])
        self.assertNotEqual(results[0]['fingerprint'], results[1]['fingerprint'])

    def test_legacy_published_record_gets_actual_target_for_restore(self):
        result = {'status': 'published', 'email': 'new@example.test', 'original_account_count_before': 0, 'original_account_count_after': 1}
        (self.directory / 'published.json').write_text(json.dumps(result))
        actual = h.dispatch({'action': 'status', 'session': self.sid})
        self.assertEqual(actual['target_key']['fingerprint'], self.meta['target_fingerprint'])
        self.assertNotIn(self.config['plugins']['configs']['key-account-bind']['bindings'][0]['key'], json.dumps(actual))


if __name__ == '__main__':
    unittest.main()
