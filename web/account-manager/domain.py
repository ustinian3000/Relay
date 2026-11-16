"""Transactional local control-plane model. No production writes or cleartext SKs."""
import copy
import datetime
import hashlib
import json
import os
import pathlib
import re
import secrets
import threading
import time


CATALOG = [
    {'id': 'claude', 'name': 'Claude', 'category': 'primary', 'methods': ['oauth', 'import'], 'quota': True, 'host': 'api.anthropic.com'},
    {'id': 'codex', 'name': 'GPT · Codex', 'category': 'primary', 'methods': ['oauth', 'import'], 'quota': True, 'host': 'chatgpt.com'},
    {'id': 'antigravity', 'name': 'Antigravity', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'cloudcode-pa.googleapis.com'},
    {'id': 'kimi', 'name': 'Kimi', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'api.kimi.com'},
    {'id': 'kimi-ai', 'name': 'Kimi AI', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'api.kimi.com'},
    {'id': 'xai', 'name': 'xAI · Grok', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'api.x.ai'},
    {'id': 'meta', 'name': 'Meta', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'api.meta.ai'},
    {'id': 'devin', 'name': 'Devin', 'category': 'other', 'methods': ['oauth', 'import'], 'quota': False, 'host': 'api.devin.ai'},
]


class Problem(Exception):
    def __init__(self, message, status=400):
        super().__init__(message)
        self.status = status


def now_iso():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def provider_id(value):
    value = {'anthropic': 'claude', 'openai': 'codex'}.get(str(value).lower(), str(value).lower())
    if value not in {p['id'] for p in CATALOG}:
        raise Problem('请选择服务实际支持的平台。')
    return value


def text(value, label, maximum=100):
    value = str(value or '').strip()
    if not value or len(value) > maximum or any(ord(c) < 32 for c in value):
        raise Problem(label + '不能为空、过长或包含控制字符。')
    return value


def item(items, identifier, label):
    found = next((x for x in items if x['id'] == identifier), None)
    if found is None:
        raise Problem(label + '不存在，可能已被删除，请刷新。', 404)
    return found


def demo_state():
    def window(remaining, seconds):
        return {'remainingPercent': remaining, 'usedPercent': 100 - remaining, 'exhausted': remaining == 0,
            'resetsAt': datetime.datetime.fromtimestamp(time.time() + seconds, datetime.timezone.utc).isoformat(), 'resetsInSeconds': seconds}
    keys = [{'id': 'key-original', 'name': '示例 SK A', 'mask': 'sk-••••-A001', 'enabled': True, 'key_hash': hashlib.sha256(b'sk-demo-original').hexdigest(), 'origin_ref': None},
            {'id': 'key-ours', 'name': '示例 SK B', 'mask': 'sk-••••-B002', 'enabled': True, 'key_hash': hashlib.sha256(b'sk-demo-ours').hexdigest(), 'origin_ref': None}]
    accounts = []
    for index, (provider, name, key, host) in enumerate([
        ('claude', 'claude-old@example.test', 'key-original', '192.0.2.10'),
        ('claude', 'claude-new@example.test', 'key-original', '192.0.2.11'),
        ('codex', 'codex-a@example.test', 'key-ours', '192.0.2.12'),
        ('codex', 'codex-b@example.test', 'key-ours', '192.0.2.12'),
        ('codex', 'pending@example.test', None, '192.0.2.50')]):
        quota = {'status': 'ok', 'available': True, 'fiveHour': window(94 if index < 2 else 34, 3600), 'weekly': window(99 if index < 2 else 30, 86400), 'monthly': None, 'error': None, 'source': 'local_demo', 'queriedAt': now_iso()}
        if index == 3:
            quota.update({'status': 'quota_exhausted', 'available': False, 'weekly': window(0, 86400)})
        accounts.append({'id': 'account-' + str(index), 'provider': provider, 'email': name, 'enabled': True,
            'key_id': key, 'proxy_host': host, 'proxy_port': 12323, 'proxy_mode': 'inherited' if index == 0 else 'dedicated',
            'origin_ref': None, 'verified': True, 'credential_ref': None, 'demo_quota': quota})
    return {'revision': 1, 'keys': keys, 'accounts': accounts, 'events': [], 'source': 'demo', 'synced_at': None}


class Store:
    def __init__(self, directory, clock=time.time):
        self.directory = pathlib.Path(directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.path = self.directory / 'state.json'
        self.clock = clock
        self.lock = threading.RLock()
        self.plans = {}
        self.bindings = {}
        if not self.path.exists():
            self._save(demo_state())
        self.state = json.loads(self.path.read_text(encoding='utf-8'))
        self._validate(self.state)

    def _save(self, state):
        raw = json.dumps(state, ensure_ascii=False, indent=2)
        ready = self.path.with_suffix('.pending')
        with ready.open('w', encoding='utf-8') as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        ready.chmod(0o600)
        os.replace(ready, self.path)

    def _validate(self, state):
        keys = {k['id'] for k in state['keys']}
        if len(keys) != len(state['keys']) or len({a['id'] for a in state['accounts']}) != len(state['accounts']):
            raise Problem('本地数据存在重复标识。')
        if len({k['name'].casefold() for k in state['keys']}) != len(keys):
            raise Problem('SK 名称不能重复。')
        identities = [(a['provider'], a['email'].casefold()) for a in state['accounts']]
        if len(set(identities)) != len(identities):
            raise Problem('这个平台下已有相同账号，请使用现有账号调整绑定。')
        for account in state['accounts']:
            if account.get('key_id') is not None and account['key_id'] not in keys:
                raise Problem('账号引用了不存在的 SK。')

    def snapshot(self):
        with self.lock:
            state = copy.deepcopy(self.state)
        for key in state['keys']:
            key.pop('key_hash', None)
            key.pop('origin_ref', None)
            key['account_ids'] = [a['id'] for a in state['accounts'] if a.get('key_id') == key['id']]
        for account in state['accounts']:
            for private in ['origin_ref', 'credential_ref', 'demo_quota']:
                account.pop(private, None)
        return state

    def validate_target(self, target, state=None):
        state = state or self.state
        if not isinstance(target, dict) or target.get('mode') not in ('existing', 'new', 'unassigned'):
            raise Problem('请先明确选择目标 SK、新建 SK 或暂不分配。')
        if target['mode'] == 'existing':
            key = item(state['keys'], target.get('key_id'), '目标 SK')
            if not key['enabled']:
                raise Problem('目标 SK 已停用，请先启用或选择其他 SK。')
            return key['name']
        if target['mode'] == 'new':
            name = text(target.get('name'), '新 SK 名称', 60)
            if any(k['name'].casefold() == name.casefold() for k in state['keys']):
                raise Problem('SK 名称已存在。')
            return '新建：' + name
        return '暂不分配'

    def _apply(self, state, kind, payload, commit=False):
        secret = None
        def new_key(name, identifier=None):
            nonlocal secret
            name = text(name, 'SK 名称', 60)
            raw = 'sk-local-' + secrets.token_urlsafe(28)
            key = {'id': identifier or 'key-' + secrets.token_hex(8), 'name': name, 'enabled': True,
                'key_hash': hashlib.sha256(raw.encode()).hexdigest(), 'mask': 'sk-••••-' + raw[-4:], 'origin_ref': None}
            state['keys'].append(key)
            if commit:
                secret = raw
            return key
        if kind == 'key_create':
            key = new_key(payload.get('name'), payload['id'])
            summary = '新建 SK「' + key['name'] + '」，暂不绑定账号'
        elif kind == 'key_update':
            key = item(state['keys'], payload.get('id'), 'SK')
            old_name = key['name']
            if 'name' in payload:
                key['name'] = text(payload['name'], 'SK 名称', 60)
            if 'enabled' in payload:
                if not isinstance(payload['enabled'], bool):
                    raise Problem('启用状态必须为布尔值。')
                key['enabled'] = payload['enabled']
            summary = '更新「' + old_name + '」：' + ('启用' if key['enabled'] else '停用')
        elif kind == 'key_delete':
            key = item(state['keys'], payload.get('id'), 'SK')
            count = sum(a.get('key_id') == key['id'] for a in state['accounts'])
            state['keys'].remove(key)
            for account in state['accounts']:
                if account.get('key_id') == key['id']:
                    account['key_id'] = None
            summary = '删除 SK「' + key['name'] + '」；' + str(count) + ' 个账号改为待分配，凭据不删除'
        elif kind == 'bindings_set':
            key = item(state['keys'], payload.get('id'), 'SK')
            identifiers = payload.get('account_ids')
            if not isinstance(identifiers, list) or len(set(identifiers)) != len(identifiers):
                raise Problem('绑定账号列表无效或包含重复项。')
            selected = [item(state['accounts'], identifier, '账号') for identifier in identifiers]
            for account in selected:
                if account.get('key_id') not in (None, key['id']):
                    raise Problem(account['email'] + ' 已属于其他 SK，请在账号页明确转移。')
            for account in state['accounts']:
                if account.get('key_id') == key['id'] and account['id'] not in identifiers:
                    account['key_id'] = None
            for account in selected:
                account['key_id'] = key['id']
            summary = '「' + key['name'] + '」绑定账号更新为 ' + str(len(selected)) + ' 个'
        elif kind in ('account_assign', 'account_toggle'):
            account = item(state['accounts'], payload.get('id'), '账号')
            if kind == 'account_toggle':
                if not isinstance(payload.get('enabled'), bool):
                    raise Problem('启用状态无效。')
                account['enabled'] = payload['enabled']
                summary = ('启用' if account['enabled'] else '停用') + '账号 ' + account['email']
            else:
                old = next((k['name'] for k in state['keys'] if k['id'] == account.get('key_id')), '待分配')
                target = payload.get('key_id')
                new = item(state['keys'], target, '目标 SK')['name'] if target else '待分配'
                account['key_id'] = target or None
                summary = account['email'] + '：' + old + ' → ' + new
        elif kind == 'account_add':
            target = payload['target']
            self.validate_target(target, state)
            account = copy.deepcopy(payload['account'])
            if not account.get('verified'):
                raise Problem('账号身份与专属代理尚未核对，不能加入账号池。')
            if target['mode'] == 'new':
                account['key_id'] = new_key(target['name'], payload['new_key_id'])['id']
            elif target['mode'] == 'existing':
                account['key_id'] = target['key_id']
            else:
                account['key_id'] = None
            state['accounts'].append(account)
            name = next((k['name'] for k in state['keys'] if k['id'] == account.get('key_id')), '待分配')
            summary = '添加 ' + account['email'] + ' → ' + name + '；固定代理 ' + account['proxy_host']
        elif kind == 'source_sync':
            source = payload['snapshot']
            state['keys'] = copy.deepcopy(source['keys'])
            state['accounts'] = copy.deepcopy(source['accounts'])
            state['source'] = 'production_snapshot'
            state['synced_at'] = source['synced_at']
            summary = '只读同步线上快照到本地：' + str(len(state['keys'])) + ' 个 SK、' + str(len(state['accounts'])) + ' 个账号；覆盖当前测试副本'
        else:
            raise Problem('不支持的管理操作。')
        self._validate(state)
        return summary, secret

    def preview(self, kind, payload):
        with self.lock:
            payload = copy.deepcopy(payload)
            if kind == 'key_create':
                payload['id'] = 'key-' + secrets.token_hex(8)
            if kind == 'account_add':
                payload['new_key_id'] = 'key-' + secrets.token_hex(8)
            proposed = copy.deepcopy(self.state)
            summary, _ = self._apply(proposed, kind, payload)
            key_names = {k['id']: k['name'] for k in self.state['keys']}
            new_names = {k['id']: k['name'] for k in proposed['keys']}
            previous = {a['id']: a for a in self.state['accounts']}
            changes = []
            for account in proposed['accounts']:
                old = previous.get(account['id'])
                before = key_names.get(old.get('key_id'), '待分配') if old else '尚未添加'
                after = new_names.get(account.get('key_id'), '待分配')
                if not old or old.get('key_id') != account.get('key_id') or old.get('enabled') != account.get('enabled'):
                    changes.append({'account': account['email'], 'before': before, 'after': after,
                        'enabled_before': old.get('enabled') if old else None, 'enabled_after': account['enabled']})
            identifier = secrets.token_hex(16)
            self.plans[identifier] = {'kind': kind, 'payload': payload, 'revision': self.state['revision'],
                'expires': self.clock() + 600, 'result': None}
            return {'id': identifier, 'summary': summary, 'revision': self.state['revision'],
                'expires_at': self.clock() + 600, 'environment': 'local',
                'changes': changes,
                'before': {'keys': len(self.state['keys']), 'accounts': len(self.state['accounts'])},
                'after': {'keys': len(proposed['keys']), 'accounts': len(proposed['accounts'])}}

    def commit(self, identifier):
        with self.lock:
            plan = self.plans.get(identifier)
            if not plan:
                raise Problem('变更预览不存在，请重新预览。', 404)
            if plan['result']:
                return {**plan['result'], 'already_applied': True}
            if plan['expires'] <= self.clock():
                raise Problem('变更预览已过期，请重新预览。', 409)
            if plan['revision'] != self.state['revision']:
                raise Problem('本地数据已经变化，请重新预览，避免覆盖其他操作。', 409)
            proposed = copy.deepcopy(self.state)
            summary, secret = self._apply(proposed, plan['kind'], plan['payload'], commit=True)
            proposed['revision'] += 1
            proposed['events'] = ([{'time': now_iso(), 'summary': summary, 'kind': plan['kind']}] + proposed['events'])[:50]
            backup = self.directory / ('state-backup-' + str(self.state['revision']) + '.json')
            if not backup.exists():
                backup.write_bytes(self.path.read_bytes())
                backup.chmod(0o600)
            self._save(proposed)
            self.state = proposed
            result = {'applied': True, 'summary': summary, 'revision': proposed['revision'], 'environment': 'local'}
            plan['result'] = dict(result)
            if secret:
                result['one_time_key'] = secret
            return result

    def export_plan(self):
        with self.lock:
            return {'environment': 'local', 'production_applied': False, 'revision': self.state['revision'],
                'sk_bindings': [{'id': k['id'], 'name': k['name'], 'enabled': k['enabled'],
                    'account_ids': [a['id'] for a in self.state['accounts'] if a.get('key_id') == k['id']]} for k in self.state['keys']],
                'accounts': [{k: a.get(k) for k in ('id', 'provider', 'email', 'key_id', 'enabled', 'proxy_host', 'proxy_port', 'proxy_mode')} for a in self.state['accounts']]}

    def route_for_test(self, key, session, provider):
        """Validate local access semantics without sending inference requests."""
        digest = hashlib.sha256(key.encode()).hexdigest()
        with self.lock:
            owner = next((k for k in self.state['keys'] if k.get('key_hash') == digest and k['enabled']), None)
            if not owner:
                raise Problem('SK 不存在或已停用。', 401)
            candidates = [a for a in self.state['accounts'] if a.get('key_id') == owner['id'] and a['enabled'] and a['provider'] == provider]
            if not candidates:
                raise Problem('当前 SK 没有可用的对应平台账号。', 503)
            cache_key = owner['id'], session, provider
            previous = self.bindings.get(cache_key)
            selected = next((a for a in candidates if a['id'] == previous), candidates[0])
            self.bindings[cache_key] = selected['id']
            return selected['id']
