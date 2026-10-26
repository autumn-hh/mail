import shutil
import subprocess
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def read(relative_path):
    return (ROOT / relative_path).read_text(encoding='utf-8')


class AdminCreationUiTest(unittest.TestCase):
    def test_mailbox_creation_labels_keep_register_internal_contract(self):
        html = read('src/frontend/templates/admin_mailbox.html')
        script = read('src/frontend/static/scripts/admin_mailbox.js')

        self.assertNotIn('用户注册', html + script)
        self.assertIn('data-view="register"', html)
        self.assertIn('id="register-view"', html)
        self.assertIn('<span>邮箱创建</span>', html)
        self.assertIn('<option value="register">自助创建</option>', html)
        self.assertIn("'register': { text: '自助创建'", script)
        self.assertIn("label: '自助创建'", script)

    def test_domain_settings_nav_view_and_states_are_present(self):
        html = read('src/frontend/templates/admin_mailbox.html')
        script = read('src/frontend/static/scripts/admin_mailbox.js')
        css = read('src/frontend/static/styles/admin_mailbox.css')

        # 导航与视图沿用既有 data-view/id 契约。
        self.assertIn('data-view="domains"', html)
        self.assertIn('<span>域名设置</span>', html)
        self.assertIn('id="domains-view"', html)
        self.assertIn("viewName === 'domains'", script)

        # 列表具备加载、空与错误三种状态。
        self.assertIn('id="domains-list"', html)
        self.assertIn('loading-row', html)
        for state in ('loading-row', 'empty-row', 'error-row'):
            with self.subTest(state=state):
                self.assertRegex(script, rf"colspan=\"4\" class=\"{state}")
        self.assertIn('暂无域名', script)

        # 启停开关、删除确认与行内错误兜底沿用页面既有模式。
        self.assertIn('role="switch"', script)
        self.assertIn('aria-checked', script)
        self.assertIn('confirmAdminAction({', script)
        self.assertIn("title: '删除域名'", script)
        self.assertIn('.domain-switch.is-on .domain-switch-track', css)

    def test_domain_settings_use_the_admin_domains_api_contract(self):
        script = read('src/frontend/static/scripts/admin_mailbox.js')

        # 增删改查到 /api/admin/domains，路径参数统一 URL 编码。
        self.assertIn('async domainRequest(', script)
        self.assertIn("this.domainRequest('/api/admin/domains'", script)
        self.assertEqual(script.count('/api/admin/domains/${encodeURIComponent(domain)}'), 2)
        self.assertIn("method: 'PUT'", script)
        self.assertIn("method: 'DELETE'", script)
        self.assertIn('isValidDomainName(domain)', script)

        # 域名变更后必须刷新邮箱创建表单的域名缓存。
        self.assertIn('async refreshDomainData()', script)
        self.assertIn('await this.loadDomainSettings();', script)
        self.assertIn('await this.loadAvailableDomains();', script)

    def test_batch_creation_controls_and_feedback_are_present(self):
        html = read('src/frontend/templates/admin_mailbox.html')
        script = read('src/frontend/static/scripts/admin_mailbox.js')
        css = read('src/frontend/static/styles/admin_mailbox.css')

        self.assertRegex(
            html,
            r'name="reg-batch-strategy"\s+value="random"\s+checked',
        )
        self.assertIn('name="reg-batch-strategy" value="sequence"', html)
        self.assertRegex(
            html,
            r'id="reg-batch-count"\s+min="2"\s+max="100"',
        )
        for element_id in (
            'reg-batch-preview-list',
            'reg-batch-progress-text',
            'reg-batch-progress-bar',
        ):
            self.assertIn(f'id="{element_id}"', html)

        self.assertIn('addresses.slice(0, 3)', script)
        self.assertIn('`已完成 ${safeCompleted}/${safeTotal}`', script)
        self.assertIn("successCount === total", script)
        self.assertIn("successCount === 0 ? 'error' : 'warning'", script)
        self.assertRegex(css, r'\.batch-result-list\s*\{[^}]*max-height:\s*340px')
        self.assertRegex(css, r'\.batch-result-list\s*\{[^}]*overflow-y:\s*auto')
        self.assertIn('.batch-completion-message.warning', css)
        self.assertIn('.batch-completion-message.error', css)

    @unittest.skipUnless(shutil.which('node'), '需要 Node.js 验证前端生成逻辑')
    def test_random_names_are_unique_and_domains_are_normalized_and_rotated(self):
        # 直接执行类方法而不启动页面，验证随机数碰撞时仍满足同批唯一性。
        node_test = textwrap.dedent(
            r'''
            const fs = require('fs');
            const source = fs.readFileSync(
                'src/frontend/static/scripts/admin_mailbox.js',
                'utf8'
            );
            const start = source.indexOf('class AdminMailboxManager');
            const end = source.indexOf('let adminManager', start);
            if (start < 0 || end < 0) throw new Error('无法定位管理类');
            const classSource = source
                .slice(start, end)
                .replace(/\/\/ 全局实例\s*$/, '')
                .trim();
            const AdminMailboxManager = eval(`(${classSource})`);
            const manager = Object.create(AdminMailboxManager.prototype);

            const originalRandom = Math.random;
            Math.random = () => 0;
            const single = manager.generateRandomUsername();
            const usernames = manager.generateRandomUsernames(100);
            Math.random = originalRandom;

            if (single !== 'alexsmith0000') throw new Error('单个用户名未包含英文名和英文姓');
            if (new Set(usernames).size !== 100) throw new Error('同批用户名不唯一');
            if (!usernames.every(name => /^[a-z]+[0-9]{4}$/.test(name) && name.length <= 20)) {
                throw new Error('用户名格式或长度不正确');
            }

            const domains = manager.normalizeDomains([
                ' alpha.test ', '', 'beta.test', 'alpha.test', null
            ]);
            if (JSON.stringify(domains) !== JSON.stringify(['alpha.test', 'beta.test'])) {
                throw new Error('域名未正确清理或去重');
            }

            const addresses = manager.generateRandomBatchAddresses(100, domains);
            const domainCounts = addresses.reduce((counts, address) => {
                const domain = address.split('@')[1];
                counts[domain] = (counts[domain] || 0) + 1;
                return counts;
            }, {});
            if (addresses.length !== 100) throw new Error('批量地址数量不正确');
            if (new Set(addresses.map(address => address.split('@')[0])).size !== 100) {
                throw new Error('批量地址用户名不唯一');
            }
            if (Object.keys(domainCounts).length !== 2) throw new Error('未轮转全部域名');
            if (Math.abs(domainCounts['alpha.test'] - domainCounts['beta.test']) > 1) {
                throw new Error('域名分配不均衡');
            }
            '''
        )

        result = subprocess.run(
            ['node', '-e', node_test],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
