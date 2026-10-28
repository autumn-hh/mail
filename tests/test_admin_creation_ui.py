import re
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

        # 列表具备加载、空与错误三种状态，复选列加入后跨五列。
        self.assertIn('id="domains-list"', html)
        self.assertIn('loading-row', html)
        for state in ('loading-row', 'empty-row', 'error-row'):
            with self.subTest(state=state):
                self.assertRegex(script, rf"colspan=\"5\" class=\"{state}")
        self.assertIn('暂无域名', script)

        # 启停开关、删除确认与行内错误兜底沿用页面既有模式。
        self.assertIn('role="switch"', script)
        self.assertIn('aria-checked', script)
        self.assertIn('confirmAdminAction({', script)
        self.assertIn("title: '删除域名'", script)
        self.assertIn('.domain-switch.is-on .domain-switch-track', css)

    def test_domain_batch_add_form_contract(self):
        html = read('src/frontend/templates/admin_mailbox.html')
        script = read('src/frontend/static/scripts/admin_mailbox.js')

        # 新增标记一律不带内联事件或行内样式，兼容严格 CSP。
        section = html[html.index('<div class="domains-content">'):html.index('id="settings-view"')]
        self.assertIsNone(re.search(r'\son[a-z]+\s*=', section))
        self.assertNotIn('style=', section)

        # 文本域接受换行、逗号、空格或分号分隔的多个域名。
        self.assertRegex(html, r'<textarea id="domain-add-input"[^>]*rows="3"')
        self.assertIn('每行一个，也支持逗号、空格或分号分隔', html)
        self.assertIn('parseDomainEntries(raw)', script)
        self.assertIn(r"/[\s,;，；]+/u", script)
        self.assertIn(r".replace(/\.$/, '')", script)

        # 实时计数、空态禁用提交、超过 200 内联提示并锁定提交。
        self.assertIn('id="domain-add-count" aria-live="polite">将添加 0 个域名<', html)
        self.assertIn('将添加 ${count} 个域名', script)
        self.assertIn('id="domain-add-limit" role="alert" hidden>', html)
        self.assertIn('单次最多添加 200 个域名', html + script)
        self.assertIn('id="domain-add-submit" disabled>', html)
        self.assertIn('submitButton.disabled = count === 0 || count > 200', script)

        # 一律提交批量接口，服务端逐项校验为准；忙碌态沿用共享 helper。
        self.assertIn("this.domainRequest('/api/admin/domains/batch-add'", script)
        self.assertIn('body: JSON.stringify({ domains })', script)
        self.assertIn("MaildropUI.setBusy(submitButton, true, '添加中...')", script)

        # 持久结果面板：role=status 通报，列出每个跳过域名及原因并转义。
        self.assertIn(
            '<div class="domain-result" id="domains-result" role="status"'
            ' aria-live="polite" hidden></div>',
            html,
        )
        self.assertIn("renderDomainResult({ type: 'add'", script)
        self.assertIn('已添加 ${added.length} 个域名', script)
        self.assertIn('跳过 ${skipped.length} 个：', script)
        self.assertIn('escapeHtml(item.reason)', script)
        self.assertIn('aria-label="关闭结果提示"', script)

        # 只有实际添加成功的域名从文本域移除，被跳过的留给管理员修正；
        # 国际化域名先转成与服务端一致的 Punycode 再比较。
        self.assertIn("input.value = domains.filter(domain => !addedSet.has(toAsciiDomain(domain))).join('\\n')", script)
        self.assertIn("return new URL(`http://${domain}`).hostname;", script)

    def test_domain_batch_delete_contract(self):
        html = read('src/frontend/templates/admin_mailbox.html')
        script = read('src/frontend/static/scripts/admin_mailbox.js')
        css = read('src/frontend/static/styles/admin_mailbox.css')

        # 复选列：行内复选框带读屏名称，表头全选支持半选态。
        self.assertIn('id="domains-select-all"', html)
        self.assertIn('aria-label="选择全部域名"', html)
        self.assertIn('aria-label="选择域名 ${escapeHtml(item.domain)}"', script)
        self.assertIn('selectAll.indeterminate = count > 0 && count < rowCount', script)
        self.assertIn('class="table-select"><input type="checkbox" id="domains-select-all"', html)

        # 选中计数与危险按钮；N=0 时禁用，文案嵌真实数量。
        self.assertIn('id="domains-selected-summary"', html)
        self.assertIn('已选择 ${count} 个域名', script)
        self.assertIn('删除所选（<span id="domains-batch-delete-count">0</span>）', html)
        self.assertIn('deleteButton.disabled = count === 0', script)
        self.assertIn('.domains-batch-bar', css)

        # 确认弹窗沿用页面模式：计数、至多 10 个域名与「等 N 个」折叠。
        self.assertIn("title: '批量删除域名'", script)
        self.assertIn('selected.slice(0, 10)', script)
        self.assertIn('等 ${selected.length} 个', script)
        self.assertIn('请先选择要删除的域名', script)

        # 提交批量删除接口；成功后刷新域名缓存并裁剪勾选集。
        self.assertIn("this.domainRequest('/api/admin/domains/batch-delete'", script)
        self.assertIn('body: JSON.stringify({ domains: selected })', script)
        self.assertIn("renderDomainResult({ type: 'delete'", script)
        self.assertIn('已删除 ${deleted.length} 个域名', script)
        self.assertIn('失败 ${failed.length} 个：', script)
        self.assertIn("deleted.forEach(domain => this.selectedDomains.delete(String(domain)))", script)
        self.assertIn('this.selectedDomains].filter(domain => existing.has(domain))', script)

        # 409 等整体失败原样展示服务端文案，列表保持不变（仅成功路径刷新）。
        self.assertIn("renderDomainResult({ type: 'error', message: error.message || '批量删除域名失败' })", script)
        self.assertIn("console.error('批量删除域名失败:', error)", script)

    def test_domain_settings_use_the_admin_domains_api_contract(self):
        script = read('src/frontend/static/scripts/admin_mailbox.js')

        # 增删改查到 /api/admin/domains，路径参数统一 URL 编码；添加一律走批量接口。
        self.assertIn('async domainRequest(', script)
        self.assertIn("this.domainRequest('/api/admin/domains'", script)
        self.assertIn("this.domainRequest('/api/admin/domains/batch-add'", script)
        self.assertIn("this.domainRequest('/api/admin/domains/batch-delete'", script)
        self.assertEqual(script.count('/api/admin/domains/${encodeURIComponent(domain)}'), 2)
        self.assertIn("method: 'PUT'", script)
        self.assertIn("method: 'DELETE'", script)

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
