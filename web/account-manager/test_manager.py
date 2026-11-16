"""Local tests of authorization, previews, enrollment, persistence and quotas."""
import concurrent.futures
import hashlib
import http.cookiejar
import json
import pathlib
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from unittest import mock

import adapters
import app
from domain import CATALOG, Problem, Store
from service import Service


class Clock:
    def __init__(self):
        self.value = time.time()
    def __call__(self):
        return self.value


class FakeProduction:
    target = 'ubuntu@192.0.2.10'
    def __init__(self):
        self.calls = 0
        self.fail = False
    def configure(self, payload):
        self.target = payload['ssh_target']
    def usage(self, key_ref):
        self.calls += 1
        if self.fail:
            raise Problem('controlled network failure', 502)
        return {'accounts': [{'account_id': 'account-2', 'status': 'ok', 'available': True,
            'fiveHour': {'remainingPercent': 43, 'usedPercent': 57, 'exhausted': False}, 'source': 'live', 'error': None}]}


class ManagerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.clock = Clock()
        self.adapter = FakeProduction()
        self.service = Service(self.temp.name, 'missing.exe', clock=self.clock, production=self.adapter)
        self.addCleanup(self.service.close)
        self.store = self.service.store

    def apply(self, kind, payload):
        plan = self.store.preview(kind, payload)
        return self.store.commit(plan['id'])

    def fields(self, provider='codex', target=None, email='new@example.test'):
        return {'provider': provider, 'email': email, 'mode': 'simulation',
            'target': target or {'mode': 'existing', 'key_id': 'key-ours'},
            'proxy': {'host': '192.0.2.33', 'port': 12323, 'scheme': 'http', 'username': 'test', 'password': 'p:a@ss'}}

    def verified_flow(self, **kwargs):
        fields = self.fields(**kwargs)
        result = self.service.perform('enroll-start', fields)
        self.service.perform('enroll-simulate', {'id': result['id'], 'state': result['state'], 'approved': True})
        return result

    def test_preview_never_changes_state(self):
        before = self.store.path.read_bytes()
        self.store.preview('key_delete', {'id': 'key-original'})
        self.assertEqual(self.store.path.read_bytes(), before)
        self.assertEqual(len(self.store.state['keys']), 2)

    def test_stale_preview_cannot_overwrite(self):
        a = self.store.preview('key_update', {'id': 'key-original', 'name': 'A'})
        self.apply('key_update', {'id': 'key-original', 'name': 'B'})
        with self.assertRaises(Problem) as error:
            self.store.commit(a['id'])
        self.assertEqual(error.exception.status, 409)
        self.assertEqual(self.store.state['keys'][0]['name'], 'B')

    def test_expired_preview(self):
        plan = self.store.preview('key_create', {'name': 'expires'})
        self.clock.value += 601
        with self.assertRaises(Problem):
            self.store.commit(plan['id'])

    def test_key_secret_one_time_and_not_persisted(self):
        plan = self.store.preview('key_create', {'name': 'new key'})
        first = self.store.commit(plan['id'])
        second = self.store.commit(plan['id'])
        secret = first['one_time_key']
        self.assertNotIn('one_time_key', second)
        self.assertNotIn(secret, self.store.path.read_text(encoding='utf-8'))
        self.assertNotIn(secret, json.dumps(self.store.snapshot()))
        self.assertNotIn(secret, json.dumps(self.store.export_plan()))

    def test_delete_key_preserves_accounts_and_unassigns(self):
        before = len(self.store.state['accounts'])
        self.apply('key_delete', {'id': 'key-original'})
        self.assertEqual(len(self.store.state['accounts']), before)
        self.assertTrue(all(a['key_id'] is None for a in self.store.state['accounts'][:2]))

    def test_bindings_cannot_share_accounts(self):
        with self.assertRaises(Problem):
            self.store.preview('bindings_set', {'id': 'key-original', 'account_ids': ['account-2']})

    def test_transferring_account_revokes_old_session_immediately(self):
        selected = self.store.route_for_test('sk-demo-ours', 'same', 'codex')
        self.assertEqual(selected, 'account-2')
        self.apply('account_assign', {'id': selected, 'key_id': 'key-original'})
        self.assertNotEqual(self.store.route_for_test('sk-demo-ours', 'same', 'codex'), selected)
        self.assertEqual(self.store.route_for_test('sk-demo-original', 'same', 'codex'), selected)
        self.assertEqual([a for a in self.store.state['accounts'] if a['id'] == selected][0]['key_id'], 'key-original')

    def test_disabled_key_revokes_access(self):
        self.store.route_for_test('sk-demo-ours', 'same', 'codex')
        self.apply('key_update', {'id': 'key-ours', 'enabled': False})
        with self.assertRaises(Problem) as error:
            self.store.route_for_test('sk-demo-ours', 'same', 'codex')
        self.assertEqual(error.exception.status, 401)

    def test_unknown_key_never_uses_another_pool(self):
        with self.assertRaises(Problem):
            self.store.route_for_test('invalid', 'same', 'claude')

    def test_missing_provider_or_target_cannot_begin(self):
        for patch in [{'provider': 'invented'}, {'target': {}}, {'target': {'mode': 'existing', 'key_id': 'missing'}}]:
            with self.subTest(patch=patch), self.assertRaises(Problem):
                self.service.enroll_start({**self.fields(), **patch})
        self.assertEqual(len(self.store.state['accounts']), 5)

    def test_failed_authorization_keeps_pool_unchanged(self):
        before = self.store.path.read_bytes()
        flow = self.service.enroll_start(self.fields())
        self.service.simulate(flow['id'], flow['state'], False)
        with self.assertRaises(Problem):
            self.service.enroll_preview(flow['id'])
        self.assertEqual(before, self.store.path.read_bytes())

    def test_expired_retry_retains_proxy_and_target(self):
        flow = self.service.enroll_start(self.fields())
        self.clock.value += 301
        self.assertEqual(self.service.enroll_status(flow['id'])['status'], 'expired')
        new = self.service.restart(flow['id'])
        self.assertNotEqual(flow['state'], new['state'])
        self.assertEqual(new['proxy_host'], flow['proxy_host'])
        self.assertEqual(new['target_name'], flow['target_name'])
        with self.assertRaises(Problem):
            self.service.simulate(flow['id'], flow['state'], True)

    def test_verified_authorization_needs_final_commit(self):
        flow = self.verified_flow()
        self.assertEqual(len(self.store.state['accounts']), 5)
        plan = self.service.enroll_preview(flow['id'])
        self.assertEqual(len(self.store.state['accounts']), 5)
        self.service.perform('commit', {'plan_id': plan['id']})
        self.assertEqual(len(self.store.state['accounts']), 6)
        self.assertEqual(self.store.state['accounts'][-1]['key_id'], 'key-ours')

    def test_new_sk_is_created_only_at_final_account_commit(self):
        flow = self.verified_flow(target={'mode': 'new', 'name': 'Project'})
        plan = self.service.enroll_preview(flow['id'])
        self.assertEqual(len(self.store.state['keys']), 2)
        result = self.service.perform('commit', {'plan_id': plan['id']})
        self.assertIn('one_time_key', result)
        self.assertEqual(self.store.state['accounts'][-1]['key_id'], self.store.state['keys'][-1]['id'])

    def test_unassigned_account_cannot_be_called(self):
        flow = self.verified_flow(target={'mode': 'unassigned'})
        plan = self.service.enroll_preview(flow['id'])
        self.store.commit(plan['id'])
        self.assertIsNone(self.store.state['accounts'][-1]['key_id'])
        self.assertNotEqual(self.store.route_for_test('sk-demo-ours', 'session', 'codex'), self.store.state['accounts'][-1]['id'])

    def test_deleted_intended_sk_blocks_final_save(self):
        flow = self.verified_flow()
        self.apply('key_delete', {'id': 'key-ours'})
        with self.assertRaises(Problem):
            self.service.enroll_preview(flow['id'])
        self.assertEqual(len(self.store.state['accounts']), 5)

    def test_import_rejects_provider_email_and_proxy_mismatch(self):
        valid = {'type': 'codex', 'email': 'new@example.test', 'access_token': 'synthetic'}
        for patch in [{'type': 'claude'}, {'email': 'wrong@example.test'}, {'proxy_url': 'http://wrong'}, {'access_token': {}}]:
            with self.subTest(patch=patch), self.assertRaises(Problem):
                self.service.enroll_import({**self.fields(), 'credential': {**valid, **patch}})
        self.assertEqual(len(self.store.state['accounts']), 5)

    def test_import_proxy_saved_before_account_is_added(self):
        flow = self.service.enroll_import({**self.fields(), 'credential': {'type': 'codex', 'email': 'new@example.test', 'access_token': 'synthetic'}})
        internal = self.service.flow(flow['id'])
        data = json.loads(pathlib.Path(internal['credential']).read_text(encoding='utf-8'))
        self.assertIn('192.0.2.33:12323', data['proxy_url'])
        self.assertNotIn('synthetic', json.dumps(flow))
        self.assertEqual(len(self.store.state['accounts']), 5)

    def test_catalog_platforms_have_working_simulated_flow(self):
        for provider in CATALOG:
            with self.subTest(provider=provider['id']):
                flow = self.verified_flow(provider=provider['id'], email=provider['id'] + '@example.test')
                self.assertEqual(self.service.enroll_status(flow['id'])['status'], 'verified')

    def test_readonly_snapshot_is_only_applied_locally_after_preview(self):
        snapshot = {'keys': [], 'accounts': [], 'synced_at': '2026-11-11T00:00:00Z'}
        with mock.patch.object(self.adapter, 'snapshot', return_value=snapshot, create=True):
            plan = self.service.perform('sync-preview', {})
        self.assertEqual(len(self.store.state['keys']), 2)
        self.store.commit(plan['id'])
        self.assertEqual(self.store.state['source'], 'production_snapshot')
        self.assertEqual(self.store.state['accounts'], [])

    def test_restart_clears_failed_authorization_message(self):
        flow = self.service.enroll_start({**self.fields(), 'mode': 'simulation'})
        self.service.simulate(flow['id'], flow['state'], False)
        restarted = self.service.restart(flow['id'])
        self.assertEqual(restarted['status'], 'waiting')
        self.assertIsNone(restarted['message'])

    def test_invalid_import_does_not_leave_resumable_session(self):
        before = set(self.service.flows)
        with self.assertRaises(Problem):
            self.service.enroll_import({**self.fields(), 'credential': {'type': 'claude', 'access_token': 'synthetic'}})
        self.assertEqual(set(self.service.flows), before)

    def prepare_live_quota(self):
        for account in self.store.state['accounts']:
            account.pop('demo_quota', None)
            if account['key_id'] == 'key-ours':
                account['origin_ref'] = account['id']
        self.store.state['keys'][1]['origin_ref'] = 'source-key'

    def test_quota_cache_does_not_repeat_provider_requests(self):
        self.prepare_live_quota()
        one = self.service.quotas.query('key-ours')
        two = self.service.quotas.query('key-ours')
        self.assertFalse(one['cached'])
        self.assertTrue(two['cached'])
        self.assertEqual(self.adapter.calls, 1)
        self.clock.value += 61
        self.service.quotas.query('key-ours')
        self.assertEqual(self.adapter.calls, 2)

    def test_query_failure_is_unknown_not_zero(self):
        self.prepare_live_quota()
        self.adapter.fail = True
        data = self.service.quotas.query('key-ours')
        self.assertTrue(all(a['available'] is None and a['fiveHour'] is None for a in data['accounts']))
        self.assertTrue(all(a['status'] == 'query_failed' for a in data['accounts']))
        self.assertEqual(data['summary']['unknown'], 2)

    def test_binding_change_invalidates_quota_cache(self):
        self.service.quotas.query('key-ours')
        self.apply('account_assign', {'id': 'account-2', 'key_id': 'key-original'})
        data = self.service.quotas.query('key-ours')
        self.assertFalse(data['cached'])
        self.assertEqual({r['account_id'] for r in data['accounts']}, {'account-3'})

    def test_disabled_key_cache_has_zero_callable_accounts(self):
        self.service.quotas.query('key-ours')
        self.apply('key_update', {'id': 'key-ours', 'enabled': False})
        self.assertEqual(self.service.quotas.query('key-ours')['summary']['available'], 0)

    def test_unsupported_quota_is_explicit(self):
        self.store.state['accounts'][2]['provider'] = 'meta'
        row = self.service.quotas.query('key-ours')['accounts'][0]
        self.assertEqual(row['status'], 'unsupported')
        self.assertIsNone(row['fiveHour'])

    def test_atomic_save_failure_keeps_memory_unchanged(self):
        plan = self.store.preview('key_delete', {'id': 'key-original'})
        before = self.store.snapshot()
        with mock.patch.object(self.store, '_save', side_effect=OSError('disk full')):
            with self.assertRaises(OSError):
                self.store.commit(plan['id'])
        self.assertEqual(before, self.store.snapshot())

    def test_store_persists_and_exports_without_secrets(self):
        self.apply('key_update', {'id': 'key-original', 'name': 'renamed'})
        restored = Store(self.temp.name)
        self.assertEqual(restored.state['keys'][0]['name'], 'renamed')
        data = json.dumps(restored.export_plan())
        self.assertNotIn('key_hash', data)
        self.assertNotIn('access_token', data)
        self.assertNotIn('password', data)

    def test_concurrent_confirm_is_idempotent(self):
        plan = self.store.preview('key_create', {'name': 'concurrent'})
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            replies = list(pool.map(lambda _: self.store.commit(plan['id']), range(16)))
        self.assertEqual(len(self.store.state['keys']), 3)
        self.assertEqual(sum('one_time_key' in r for r in replies), 1)


class OAuthTests(unittest.TestCase):
    def test_wrong_and_expired_callback_never_sent(self):
        with tempfile.TemporaryDirectory() as temp:
            api = mock.Mock(return_value={'state': 'correct', 'url': 'https://example.test/auth'})
            worker = adapters.OAuthWorker(temp, '', 'codex', 'http://u:p@192.0.2.1:80', 'new@example.test', api=api)
            worker.start()
            before = api.call_count
            with self.assertRaises(Problem):
                worker.callback('http://localhost/callback?code=x&state=wrong')
            worker.deadline = time.time() - 1
            with self.assertRaises(Problem):
                worker.callback('http://localhost/callback?code=x&state=correct')
            self.assertEqual(api.call_count, before)

    def test_callback_duplicates_do_not_exchange_twice(self):
        with tempfile.TemporaryDirectory() as temp:
            api = mock.Mock(return_value={'state': 'correct', 'url': 'https://example.test/auth'})
            worker = adapters.OAuthWorker(temp, '', 'claude', 'http://u:p@192.0.2.1:80', 'new@example.test', api=api)
            worker.start()
            before = api.call_count
            worker.callback('http://localhost/callback?code=x&state=correct')
            worker.callback('http://localhost/callback?code=x&state=correct')
            self.assertEqual(api.call_count, before + 1)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.service = Service(self.temp.name, 'missing.exe')
        self.server = app.Server(('127.0.0.1', 0), self.service)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = 'http://127.0.0.1:' + str(self.server.server_port)
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(self.cookies))
        with self.opener.open(self.base) as response:
            response.read()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.server.executor.shutdown(wait=True)
        self.service.close()
        self.temp.cleanup()

    def post(self, path, payload, origin=None, opener=None):
        request = urllib.request.Request(self.base + path, data=json.dumps(payload).encode(),
            headers={'Origin': origin or self.base, 'Content-Type': 'application/json'})
        try:
            with (opener or self.opener).open(request) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.load(error)

    def result(self, job):
        for _ in range(50):
            with self.opener.open(self.base + '/api/jobs/' + job) as response:
                result = json.load(response)
            if not result.get('pending'):
                return result
            time.sleep(.01)
        self.fail('Job did not finish')

    def test_cross_origin_and_missing_cookie_are_rejected(self):
        self.assertEqual(self.post('/api/preview', {}, origin='http://untrusted.test')[0], 403)
        plain = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.assertEqual(self.post('/api/preview', {}, opener=plain)[0], 403)

    def test_real_http_preview_commit_and_one_time_key(self):
        status, job = self.post('/api/preview', {'kind': 'key_create', 'payload': {'name': 'http key'}})
        self.assertEqual(status, 202)
        preview = self.result(job['job'])['data']
        self.assertEqual(len(self.service.store.state['keys']), 2)
        _, job = self.post('/api/commit', {'plan_id': preview['id']})
        first = self.result(job['job'])
        second = self.result(job['job'])
        self.assertIn('one_time_key', first['data'])
        self.assertNotIn('one_time_key', second['data'])
        self.assertEqual(len(self.service.store.state['keys']), 3)

    def test_server_refuses_unknown_mutation_action(self):
        _, job = self.post('/api/restart-production', {})
        result = self.result(job['job'])
        self.assertFalse(result['ok'])
        self.assertEqual(result['status'], 404)


if __name__ == '__main__':
    unittest.main(verbosity=2)
