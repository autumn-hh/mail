# Maildrop MCP 方法与参数速查

版本：**1.1.0，41 个工具**（2026-11-06）。入口：`https://mail.roixw.com/mcp`，Streamable HTTP。

认证：`Authorization: Bearer <MCP_ADMIN_TOKEN>`，使用独立 MCP 超级管理员 Token。先完成 `initialize` → `notifications/initialized`，再调用 `tools/list` / `tools/call`。

`mailbox` 为邮箱 ID 或完整地址；必填/可选按当前已部署版本 Schema 标注。

| 方法 | 用途 | 必填参数 | 可选参数 |
| --- | --- | --- | --- |
| `maildrop_generate_address` | 生成候选邮箱地址 | 无 | `domain: string` |
| `maildrop_create_mailbox` | 创建单个邮箱 | 无 | `address: string`、`domain: string`、`retention_days: int`、`sender_whitelist: string[]`、`allowed_domains: string[]` |
| `maildrop_create_mailboxes_batch` | 批量创建邮箱 | `items: object[]` | 无 |
| `maildrop_list_mailboxes` | 分页列出邮箱 | 无 | `page: int`、`page_size: int`、`search: string`、`status: string`、`source: string` |
| `maildrop_get_mailbox` | 查询邮箱详情 | `mailbox: string` | `include_credentials: bool` |
| `maildrop_update_mailbox` | 修改邮箱配置 | `mailbox: string` | `retention_days: int`、`sender_whitelist: string[]`、`allowed_domains: string[]`、`whitelist_enabled: bool` |
| `maildrop_set_mailbox_status` | 启用或停用邮箱 | `mailbox: string`、`enabled: bool` | 无 |
| `maildrop_delete_mailbox` | 停用或永久删除邮箱 | `mailbox: string` | `permanent: bool` |
| `maildrop_delete_mailboxes_batch` | 批量停用或永久删除邮箱 | `mailboxes: string[]` | `permanent: bool` |
| `maildrop_reset_mailbox_token` | 轮换邮箱 API Token | `mailbox: string` | 无 |
| `maildrop_rotate_mailbox_key` | 轮换邮箱网页访问 Key | `mailbox: string` | 无 |
| `maildrop_get_sender_whitelist` | 查看发件白名单 | `mailbox: string` | 无 |
| `maildrop_add_sender_whitelist` | 添加单条发件白名单 | `mailbox: string`、`sender: string` | 无 |
| `maildrop_remove_sender_whitelist` | 移除单条发件白名单 | `mailbox: string`、`sender: string` | 无 |
| `maildrop_set_whitelist_enabled` | 设置发件白名单开关 | `mailbox: string`、`enabled: bool` | 无 |
| `maildrop_list_emails` | 分页查看邮件摘要 | `mailbox: string` | `query: string`、`sender: string`、`subject: string`、`after: int`、`before: int`、`unread_only: bool`、`page: int`、`page_size: int`、`ignore_ids: string[]` |
| `maildrop_search_emails` | 按条件检索邮件 | `mailbox: string` | `query: string`、`sender: string`、`subject: string`、`after: int`、`before: int`、`unread_only: bool`、`page: int`、`page_size: int`、`ignore_ids: string[]` |
| `maildrop_get_email` | 读取单封邮件内容 | `mailbox: string`、`email_id: string` | `max_body_chars: int` |
| `maildrop_wait_for_email` | 等待匹配邮件 | `mailbox: string` | `query: string`、`sender: string`、`subject: string`、`after: int`、`before: int`、`unread_only: bool`、`page: int`、`page_size: int`、`ignore_ids: string[]`、`timeout_seconds: int` |
| `maildrop_set_email_read` | 设置单封邮件已读状态 | `mailbox: string`、`email_id: string`、`is_read: bool` | 无 |
| `maildrop_mark_all_read` | 将邮箱全部邮件标记已读 | `mailbox: string` | 无 |
| `maildrop_delete_emails` | 永久批量删除邮件 | `mailbox: string`、`email_ids: string[]` | 无 |
| `maildrop_health` | 检查MCP数据库连接 | 无 | 无 |
| `maildrop_server_info` | 查看MCP能力与限额 | 无 | 无 |
| `maildrop_list_domains` | 列出收件域名 | 无 | 无 |
| `maildrop_add_domains` | 批量添加收件域名 | `domains: string[]` | 无 |
| `maildrop_set_domain_enabled` | 启用或禁用收件域名 | `domain: string`、`enabled: bool` | 无 |
| `maildrop_delete_domains` | 批量删除收件域名 | `domains: string[]` | 无 |
| `maildrop_get_stats` | 查看全局业务统计 | 无 | 无 |
| `maildrop_get_source_stats` | 查看邮箱创建来源统计 | 无 | 无 |
| `maildrop_list_audit_logs` | 查询审计日志 | 无 | `mailbox: string`、`limit: int` |
| `maildrop_list_blocked_ips` | 查看当前IP封禁 | 无 | 无 |
| `maildrop_unblock_ip` | 解封单个IP | `ip: string` | 无 |
| `maildrop_get_security_config` | 查询认证失败封禁阈值 | 无 | 无 |
| `maildrop_update_security_config` | 修改运行期封禁阈值 | 无 | `block_duration: int`、`max_attempts: int`、`attempt_window: int` |
| `maildrop_get_sending_policy` | 查询发信授权、额度和用量 | `mailbox: string` | 无 |
| `maildrop_set_sending_policy` | 开通或撤销发信、设置额度 | `mailbox: string`、`enabled: bool`、`daily_limit: int`、`minute_limit: int` | 无 |
| `maildrop_send_email` | 提交邮件到发送队列 | `mailbox: string`、`idempotency_key: string`、`to: string[]`、`subject: string` | `cc: string[]`、`bcc: string[]`、`text: string`、`html: string`、`reply_email_id: string`、`attachments: object[]` |
| `maildrop_list_sent_emails` | 分页查看发送记录 | `mailbox: string` | `offset: int` |
| `maildrop_get_sent_email` | 查看发送内容、投递状态和退信 | `mailbox: string`、`sent_id: string` | `max_body_chars: int` |
| `maildrop_delete_sent_email` | 删除已结束的发送记录 | `mailbox: string`、`sent_id: string` | 无 |

`create_mailboxes_batch.items[]` 的可选字段：`address: string`、`domain: string`、`retention_days: int`、`sender_whitelist: string[]`、`allowed_domains: string[]`。

`update_mailbox` 除 `mailbox` 外至少传一个更新字段；`update_security_config` 至少传一个配置字段。`after`、`before` 为 Unix 秒。

`send_email.attachments[]`：必填 `filename: string`、`content: string`（标准 base64）；可选 `content_type: string`，省略时检测文件类型。最多 10 个附件、解码后合计 10 MiB；正文共 1 MiB；整个 MCP 请求最多 16 MiB。

发信须先获得邮箱授权。`daily_limit` 为 1–100000，`minute_limit` 为 1–1000，按去重收件人数计额。`idempotency_key` 为 1–128 字节，同一封邮件重试须复用原键和原内容。`accepted` 仅表示对方 SMTP 接受，不保证进入收件箱。发送列表每页最多 50 条，`offset` 默认 0。

调用格式（完成握手后）：

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "maildrop_get_sending_policy",
    "arguments": {"mailbox": "sender@example.com"}
  }
}
```

成功数据读 `result.structuredContent`；业务错误检查 `result.isError` 和 `result.content`。完整规则见 [工具参考](mcp-tools.md) 与 [发信工具](mcp-outbound.md)，参数定义见 [Schema](mcp-tools.schema.json)。
