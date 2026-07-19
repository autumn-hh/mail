import atexit
import os
import shutil
import sqlite3
import sys
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from urllib.parse import quote


ROOT = Path(__file__).resolve().parents[1]
BACKEND = ROOT / 'src' / 'backend'
TEST_DATA = Path(tempfile.mkdtemp(prefix='maildrop-backend-tests-'))
atexit.register(lambda: shutil.rmtree(TEST_DATA, ignore_errors=True))

# 在导入应用前指定临时数据库，避免测试读取或修改工作区 data/。
os.environ['DATABASE_PATH'] = str(TEST_DATA / 'mailbox.db')
os.environ['USE_DATABASE'] = 'true'
os.environ['ENABLE_IP_WHITELIST'] = 'false'
os.environ['PASSWORD'] = 'test-admin-password'
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(BACKEND))

import config  # noqa: E402
from database import DatabaseManager, db_manager  # noqa: E402
from src.backend import db_inbox_handler  # noqa: E402
from src.backend.flask_app import app  # noqa: E402
from ip_blocker import ip_blocker  # noqa: E402
from src.backend.routes.admin_api import get_client_ip  # noqa: E402


class PublicMailboxBackendTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        app.config.update(TESTING=True)
        cls.client = app.test_client()

    def create_mailbox(self, prefix='mailbox', retention_days=7):
        address = f'{prefix}-{uuid.uuid4().hex[:10]}@example.com'
        return db_manager.create_mailbox(address, retention_days=retention_days)

    @staticmethod
    def bearer(mailbox):
        return {'Authorization': f"Bearer {mailbox['access_token']}"}

    @staticmethod
    def add_email(mailbox, subject='测试邮件'):
        now = int(time.time())
        return db_manager.add_email(mailbox['id'], {
            'id': str(uuid.uuid4()),
            'From': 'sender@example.net',
            'To': mailbox['address'],
            'Subject': subject,
            'Body': '<p>安全测试正文</p>',
            'ContentType': 'text/html',
            'Timestamp': now,
            'Sent': '刚刚',
        })

    def test_strict_page_routes_and_private_headers(self):
        self.assertEqual(self.client.get('/').status_code, 200)
        self.assertEqual(self.client.get('/web').status_code, 200)
        login_redirect = self.client.get('/login', follow_redirects=False)
        self.assertEqual(login_redirect.status_code, 302)
        self.assertEqual(login_redirect.headers['Location'], '/')
        self.assertEqual(self.client.get('/admin').status_code, 200)
        self.assertEqual(self.client.get('/register').status_code, 200)
        self.assertEqual(self.client.get('/api-test').status_code, 200)

        credential = quote('user----label@example.com----secret-key-123', safe='----')
        response = self.client.get(f'/web/{credential}')
        self.assertEqual(response.status_code, 200)
        self.assertIn('no-store', response.headers['Cache-Control'])
        self.assertEqual(response.headers['Referrer-Policy'], 'no-referrer')
        self.assertIn('noindex', response.headers['X-Robots-Tag'])
        self.assertEqual(response.headers['X-Frame-Options'], 'DENY')
        self.assertEqual(response.headers['X-Content-Type-Options'], 'nosniff')
        self.assertIn('camera=()', response.headers['Permissions-Policy'])
        self.assertIn("frame-src 'self'", response.headers['Content-Security-Policy'])
        self.assertIn("frame-ancestors 'none'", response.headers['Content-Security-Policy'])

        invalid_credential = self.client.get('/web/not-a-valid-credential')
        self.assertEqual(invalid_credential.status_code, 404)
        self.assertIn('no-store', invalid_credential.headers['Cache-Control'])
        self.assertEqual(invalid_credential.headers['Referrer-Policy'], 'no-referrer')
        self.assertIn('noindex', invalid_credential.headers['X-Robots-Tag'])

        for path in (
            '/mailbox',
            '/admin/mailboxes',
            '/web/not-a-credential',
            '/api/login',
            '/api/mailbox',
            '/api/mailbox/demo',
            '/api/api-test',
            '/api/demo/get_token',
            '/api/demo/mailbox_info',
            '/api/demo/emails',
            '/api/create_mailbox_v2',
            '/api/user_login',
            '/api/send_test_email',
            '/api/migrate_to_database',
            '/api/export_from_database',
            '/api/admin/whitelist',
            '/api/admin/test_ip',
        ):
            with self.subTest(path=path):
                self.assertEqual(self.client.get(path).status_code, 404)

        self.assertEqual(self.client.post('/api/demo/get_token', json={}).status_code, 404)
        self.assertEqual(self.client.post('/api/create_mailbox_v2', json={}).status_code, 404)
        # 旧用户密码登录会回传整批 access_token，必须彻底移除而不是保留第二认证链。
        self.assertEqual(self.client.post('/api/user_login', json={}).status_code, 404)
        self.assertEqual(self.client.post('/api/send_test_email', json={}).status_code, 404)
        # 迁移与导出曾允许匿名指定文件路径，删除路由可从根源避免任意读写。
        self.assertEqual(self.client.post('/api/migrate_to_database', json={}).status_code, 404)
        self.assertEqual(self.client.post('/api/export_from_database', json={}).status_code, 404)
        # 旧管理配置接口曾直接改写 .env，统一移除后只保留当前管理 Blueprint。
        self.assertEqual(self.client.post('/api/admin/whitelist', json={}).status_code, 404)
        self.assertEqual(self.client.post('/api/admin/test_ip', json={}).status_code, 404)

    def test_mailbox_key_is_available_through_database_handler(self):
        mailbox = self.create_mailbox('key-field')
        by_address = db_manager.get_mailbox_by_address(mailbox['address'])
        public_handler_data = db_inbox_handler.get_mailbox_info(mailbox['address'])
        self.assertEqual(by_address['mailbox_key'], mailbox['mailbox_key'])
        self.assertEqual(public_handler_data['mailbox_key'], mailbox['mailbox_key'])

    def test_old_database_rows_receive_a_mailbox_key(self):
        old_path = TEST_DATA / f'old-{uuid.uuid4().hex}.db'
        now = int(time.time())
        with sqlite3.connect(old_path) as conn:
            conn.execute('''
                CREATE TABLE mailboxes (
                    id TEXT PRIMARY KEY,
                    address TEXT UNIQUE NOT NULL,
                    created_at INTEGER NOT NULL,
                    expires_at INTEGER NOT NULL,
                    retention_days INTEGER DEFAULT 7,
                    is_active BOOLEAN DEFAULT 1,
                    sender_whitelist TEXT DEFAULT '[]',
                    created_by_ip TEXT,
                    access_token TEXT,
                    last_accessed INTEGER
                )
            ''')
            conn.execute('''
                INSERT INTO mailboxes
                (id, address, created_at, expires_at, retention_days, is_active,
                 sender_whitelist, access_token, last_accessed)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            ''', (
                'legacy-id', 'legacy@example.com', now, now + 86400, 7, 1,
                '[]', 'legacy-token', now,
            ))

        migrated = DatabaseManager(str(old_path)).get_mailbox_by_address('legacy@example.com')
        self.assertIsNotNone(migrated['mailbox_key'])
        self.assertGreaterEqual(len(migrated['mailbox_key']), 6)

    def test_key_exchange_handles_valid_wrong_expired_and_disabled_mailboxes(self):
        active = self.create_mailbox('active-key')
        malformed = self.client.post('/api/get_mailbox_token', json={
            'address': 123,
            'mailbox_key': ['not', 'a', 'string'],
        })
        self.assertEqual(malformed.status_code, 400)

        valid = self.client.post('/api/get_mailbox_token', json={
            'address': active['address'],
            'mailbox_key': active['mailbox_key'],
        })
        self.assertEqual(valid.status_code, 200)
        self.assertEqual(valid.get_json()['access_token'], active['access_token'])

        wrong = self.client.post('/api/get_mailbox_token', json={
            'address': active['address'],
            'mailbox_key': 'wrong-key-123',
        })
        self.assertEqual(wrong.status_code, 401)
        self.assertIn('message', wrong.get_json())

        expired = self.create_mailbox('expired-key')
        with db_manager.get_connection() as conn:
            conn.execute('UPDATE mailboxes SET expires_at = ? WHERE id = ?',
                         (int(time.time()) - 1, expired['id']))
        expired_response = self.client.post('/api/get_mailbox_token', json={
            'address': expired['address'],
            'mailbox_key': expired['mailbox_key'],
        })
        self.assertEqual(expired_response.status_code, 410)

        disabled = self.create_mailbox('disabled-key')
        with db_manager.get_connection() as conn:
            conn.execute('UPDATE mailboxes SET is_active = 0 WHERE id = ?', (disabled['id'],))
        disabled_response = self.client.post('/api/get_mailbox_token', json={
            'address': disabled['address'],
            'mailbox_key': disabled['mailbox_key'],
        })
        self.assertEqual(disabled_response.status_code, 423)

    def test_all_creation_apis_return_mailbox_key(self):
        malicious_sub_token = f'sub-{uuid.uuid4().hex}'
        db_manager.create_sub_admin(malicious_sub_token, ['example.com'])
        malicious_sub = self.client.post(
            '/api/register_with_token',
            headers={'X-Sub-Admin-Token': malicious_sub_token},
            json={'email': '<img_src_x>@example.com', 'retention_days': 7},
        )
        self.assertEqual(malicious_sub.status_code, 400)

        malicious_admin = self.client.post(
            '/api/register',
            headers={'Authorization': config.PASSWORD},
            json={'email': 'safe@<img>.com', 'retention_days': 7},
        )
        self.assertEqual(malicious_admin.status_code, 400)

        register_email = f'reg{uuid.uuid4().hex[:8]}@example.com'
        registered = self.client.post(
            '/api/register',
            headers={'Authorization': config.PASSWORD},
            json={'email': register_email, 'retention_days': 7},
        )
        self.assertEqual(registered.status_code, 201, registered.get_data(as_text=True))
        self.assertTrue(registered.get_json().get('mailbox_key'))

        sub_token = f'sub-{uuid.uuid4().hex}'
        db_manager.create_sub_admin(sub_token, ['example.com'])
        sub_created = self.client.post(
            '/api/register_with_token',
            headers={'X-Sub-Admin-Token': sub_token},
            json={'email': f'sub{uuid.uuid4().hex[:8]}', 'retention_days': 7},
        )
        self.assertEqual(sub_created.status_code, 201, sub_created.get_data(as_text=True))
        self.assertTrue(sub_created.get_json().get('mailbox_key'))

        admin_created = self.client.post(
            '/api/admin/mailboxes',
            headers={'Authorization': f'Bearer {config.PASSWORD}'},
            json={
                'address': f'owner{uuid.uuid4().hex[:8]}@example.com',
                'retention_days': 7,
            },
        )
        self.assertEqual(admin_created.status_code, 200, admin_created.get_data(as_text=True))
        self.assertTrue(admin_created.get_json()['data'].get('mailbox_key'))

    def test_admin_authentication_fails_closed_and_ignores_spoofed_forwarded_ip(self):
        original_password = config.PASSWORD
        config.PASSWORD = ''
        try:
            login = self.client.post('/api/admin_login', json={'password': ''})
            self.assertEqual(login.status_code, 503)
            admin_api = self.client.get(
                '/api/admin/mailboxes',
                headers={'Authorization': 'Bearer anything'},
            )
            self.assertEqual(admin_api.status_code, 401)
        finally:
            config.PASSWORD = original_password

        # 仅使用 WSGI 已确认的远端地址，客户端自带 X-Forwarded-For 不能绕过限速。
        with app.test_request_context(
            '/',
            headers={'X-Forwarded-For': '198.51.100.99'},
            environ_overrides={'REMOTE_ADDR': '203.0.113.7'},
        ):
            self.assertEqual(get_client_ip(), '203.0.113.7')

        attacker_ip = '203.0.113.8'
        ip_blocker.unblock_ip(attacker_ip)
        try:
            statuses = [
                self.client.post(
                    '/api/admin_login',
                    json={'password': 'wrong-password'},
                    environ_base={'REMOTE_ADDR': attacker_ip},
                ).status_code
                for _ in range(ip_blocker.max_attempts)
            ]
            self.assertEqual(statuses[-1], 403)
            blocked = self.client.post(
                '/api/admin_login',
                json={'password': config.PASSWORD},
                environ_base={'REMOTE_ADDR': attacker_ip},
            )
            self.assertEqual(blocked.status_code, 403)
        finally:
            ip_blocker.unblock_ip(attacker_ip)

    def test_bearer_reads_require_auth_and_never_return_secrets(self):
        owner = self.create_mailbox('read-owner')
        other = self.create_mailbox('read-other')
        owner_email = self.add_email(owner)
        other_email = self.add_email(other)

        self.assertEqual(self.client.get('/api/mailbox_info_v2').status_code, 401)
        query_only = self.client.get(
            f"/api/mailbox_info_v2?token={owner['access_token']}"
        )
        self.assertEqual(query_only.status_code, 401)

        info = self.client.get('/api/mailbox_info_v2', headers=self.bearer(owner))
        self.assertEqual(info.status_code, 200)
        mailbox_data = info.get_json()['mailbox']
        self.assertNotIn('access_token', mailbox_data)
        self.assertNotIn('mailbox_key', mailbox_data)

        inbox = self.client.get(
            f"/api/get_inbox?address={quote(owner['address'])}",
            headers=self.bearer(owner),
        )
        self.assertEqual(inbox.status_code, 200)
        self.assertEqual([item['id'] for item in inbox.get_json()], [owner_email])

        cross_inbox = self.client.get(
            f"/api/get_inbox?address={quote(other['address'])}",
            headers=self.bearer(owner),
        )
        self.assertEqual(cross_inbox.status_code, 403)

        detail = self.client.get(
            f"/api/get_email?address={quote(owner['address'])}&id={owner_email}",
            headers=self.bearer(owner),
        )
        self.assertEqual(detail.status_code, 200)
        self.assertTrue(detail.get_json()['is_read'])

        cross_detail = self.client.get(
            f"/api/get_email?address={quote(owner['address'])}&id={other_email}",
            headers=self.bearer(owner),
        )
        self.assertEqual(cross_detail.status_code, 403)

    def test_every_mailbox_write_requires_bearer_and_ownership(self):
        owner = self.create_mailbox('write-owner')
        other = self.create_mailbox('write-other')
        other_email = self.add_email(other)

        cases = (
            ('/api/mark_email_read', {'email_id': other_email, 'is_read': True}),
            ('/api/delete_email', {'email_id': other_email}),
            ('/api/delete_emails_batch', {'email_ids': [other_email]}),
            ('/api/mark_all_read', {'address': other['address']}),
            ('/api/add_sender_whitelist', {'address': other['address'], 'sender': '@example.net'}),
            ('/api/remove_sender_whitelist', {'address': other['address'], 'sender': '@example.net'}),
            ('/api/update_retention', {'address': other['address'], 'retention_days': 8}),
            ('/api/regenerate_mailbox_key', {
                'address': other['address'],
                'current_key': other['mailbox_key'],
            }),
            ('/api/toggle_mailbox_status', {'address': other['address']}),
            ('/api/toggle_whitelist', {'address': other['address'], 'enabled': True}),
        )

        for path, payload in cases:
            with self.subTest(path=path, check='missing-token'):
                response = self.client.post(path, json=payload)
                self.assertEqual(response.status_code, 401, response.get_data(as_text=True))
            with self.subTest(path=path, check='wrong-owner'):
                response = self.client.post(path, json=payload, headers=self.bearer(owner))
                self.assertEqual(response.status_code, 403, response.get_data(as_text=True))

        own_email = self.add_email(owner, '可修改邮件')
        updated = self.client.post(
            '/api/mark_email_read',
            json={'email_id': own_email, 'is_read': True},
            headers=self.bearer(owner),
        )
        self.assertEqual(updated.status_code, 200)
        self.assertTrue(db_manager.get_email_by_id(own_email)['is_read'])


if __name__ == '__main__':
    unittest.main()
