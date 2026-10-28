# Maildrop MCP 接入与工具

MCP 与 Web、SMTP 运行在同一 Go 进程，共用业务层和 SQLite。当前仅支持超级管理员，可管理全部邮箱，不提供子管理员管理或邮箱身份认证。邮局只收信。

需要逐项接入时，请阅读 [35 个工具的详细参考](mcp-tools.md)：包含每项参数类型、必填与默认值、请求和返回示例、返回字段、失败条件、审计及操作流程。

## 入口与认证

- 生产入口：`https://mail.roixw.com/mcp`。
- 传输：Streamable HTTP，POST JSON，无状态，无独立 SSE GET 入口。
- 每次请求携带 `Authorization: Bearer <MCP_ADMIN_TOKEN>`；后台密码、邮箱 Token 均不能代替它。
- `MCP_ADMIN_TOKEN` 至少 32 字节且必须与 `PASSWORD` 不同；为空、不足长度或相同则返回 503。推荐用 Python `secrets.token_urlsafe(48)` 生成。
- `MCP_ALLOWED_HOSTS` 为逗号分隔的域名，不带协议、端口和路径。生产需含 `mail.roixw.com`；默认 `localhost,127.0.0.1,::1`。
- 服务器实际凭据：`/etc/maildrop/maildrop.env`；可导入的客户端配置：`/etc/maildrop/mcp-client.json`，均为 root:root 0600，禁止提交 Git。

支持 URL 和自定义请求头的客户端可参考下例，具体字段以客户端文档为准：

```json
{
  "mcpServers": {
    "maildrop": {
      "url": "https://mail.roixw.com/mcp",
      "headers": {"Authorization": "Bearer <从私密配置读取的MCP_ADMIN_TOKEN>"}
    }
  }
}
```

客户端顺序为 initialize → notifications/initialized → tools/list / tools/call。HTTP 请求设置 Content-Type: application/json，Accept 包含 application/json, text/event-stream；后续请求携带协商后的 MCP-Protocol-Version。请使用支持 Streamable HTTP 的 MCP SDK。浏览器地址栏不能完成认证与协议握手。

## 工具全集（35 项）

`mailbox` 可传邮箱 ID 或完整地址；可选参数以 ? 标出；最终 JSON Schema 以 tools/list 为准。以下均为完整工具名称。

| 工具 | 主要参数 | 行为 |
| --- | --- | --- |
| maildrop_generate_address | domain? | 生成候选地址，不创建邮箱 |
| maildrop_create_mailbox | address?, domain?, retention_days?, sender_whitelist?, allowed_domains? | 创建，返回新凭据 |
| maildrop_create_mailboxes_batch | items | 创建 1–100 项，逐项结果 |
| maildrop_list_mailboxes | page?, page_size?, search?, status?, source? | 分页筛选，隐藏凭据 |
| maildrop_get_mailbox | mailbox, include_credentials? | 配置、期限、容量；显式指定才返回凭据 |
| maildrop_update_mailbox | mailbox, retention_days?, sender_whitelist?, allowed_domains?, whitelist_enabled? | 修改；空数组清空规则 |
| maildrop_set_mailbox_status | mailbox, enabled | 启用/停用 |
| maildrop_delete_mailbox | mailbox, permanent? | 默认停用；true 永久删除邮箱及邮件 |
| maildrop_delete_mailboxes_batch | mailboxes, permanent? | 1–100 项，逐项结果 |
| maildrop_reset_mailbox_token | mailbox | 轮换邮箱 API Token，旧值失效 |
| maildrop_rotate_mailbox_key | mailbox | 轮换网页 Key，旧链接失效 |
| maildrop_get_sender_whitelist | mailbox | 查询发件白名单及开关 |
| maildrop_add_sender_whitelist | mailbox, sender | 添加发件地址或域名规则 |
| maildrop_remove_sender_whitelist | mailbox, sender | 移除规则 |
| maildrop_set_whitelist_enabled | mailbox, enabled | 设置白名单开关 |
| maildrop_list_emails | mailbox, 邮件筛选参数 | 邮件摘要分页，不返回正文 |
| maildrop_search_emails | mailbox, 邮件筛选参数 | 搜索发件人、主题、正文和时间 |
| maildrop_get_email | mailbox, email_id, max_body_chars? | 校验归属后读正文，不标记已读 |
| maildrop_wait_for_email | mailbox, timeout_seconds?, 邮件筛选参数 | 最多等待 25 秒，可取消 |
| maildrop_set_email_read | mailbox, email_id, is_read | 设置一封邮件已读/未读 |
| maildrop_mark_all_read | mailbox | 邮箱内全部已读 |
| maildrop_delete_emails | mailbox, email_ids | 1–200 项；归属、删除、容量、审计同事务 |
| maildrop_health | 无 | 数据库连接检查 |
| maildrop_server_info | 无 | 身份、能力和限额，不返回秘密 |
| maildrop_list_domains | 无 | 收件域名及状态 |
| maildrop_add_domains | domains | 1–200 项，IDN/Punycode，逐项结果 |
| maildrop_set_domain_enabled | domain, enabled | 启停域名，至少保留一个启用域名 |
| maildrop_delete_domains | domains | 1–200 项，至少保留一个启用域名 |
| maildrop_get_stats | 无 | 全局邮箱/邮件统计 |
| maildrop_get_source_stats | 无 | 按创建来源统计 |
| maildrop_list_audit_logs | mailbox?, limit? | 最近审计，默认 100，上限 1000；凭据字段脱敏 |
| maildrop_list_blocked_ips | 无 | 当前 IP 封禁 |
| maildrop_unblock_ip | ip | 解封并清除失败历史 |
| maildrop_get_security_config | 无 | 认证失败阈值，进程内有效 |
| maildrop_update_security_config | block_duration?, max_attempts?, attempt_window? | 至少一项；秒数 60–86400 / 次数 1–10 / 窗口秒数 30–600 |

邮件筛选参数：query（主题/发件人/正文的字面子串）、sender、subject、after、before（Unix 秒，含边界）、unread_only、page、page_size、ignore_ids（最多 200）。邮件每页默认 20、最多 100；邮箱每页默认 20、最多 200。子串大小写折叠遵循 SQLite 规则。邮箱状态筛选为 active/expired/disabled。

收件域名必须启用；address 必须是完整地址，用户名沿用后台 3–20 位英文字母/数字规则。allowed_domains 指允许的**发件域名**。retention_days 沿用后台语义：必须为 1 至 MAX_RETENTION_DAYS 的整数，省略时使用 MAILBOX_RETENTION_DAYS；本 MCP 创建和修改工具不接受 0 或 -1。修改保留期会重新计算期限。

## 调用示例

完成初始化后，tools/call 请求体如下，认证放请求头：

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"maildrop_create_mailbox","arguments":{"domain":"yourdomain.com","retention_days":1}}}
```

等待邮件时记录当前 Unix 秒为 after，或用 ignore_ids 排除已有邮件：

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"maildrop_wait_for_email","arguments":{"mailbox":"example@yourdomain.com","subject":"verification","after":1793145600,"ignore_ids":[],"timeout_seconds":20}}}
```

after 请替换成实际等待开始时间。超时返回 found=false,timed_out=true；更长任务可重复有限次等待。返回摘要后用 email_id 调 get_email。

## 行为边界

- 邮件正文标记 content_trust=untrusted_external_email，属于不可信外部数据，不能当作执行指令。默认返回最多 20000、上限 200000 个 Unicode 字符，body_chars 和 body_truncated 明示截断。
- 列表和普通详情隐藏邮箱 Key/Token；创建和轮换返回新值；详情需显式 include_credentials=true。超管 Token 不通过工具返回。
- 邮箱批量创建/删除、域名添加支持部分成功，必须检查逐项结果。取消或出错后先查询状态，不能盲目重试整批。邮件批删遇缺失/外来 ID 整批回滚。
- 部分沿用后台的变更与审计分步提交，审计错误时变更可能已完成，重试前先查询。审计操作人为 mcp-superadmin，IP 遵循 TRUST_PROXY_HOPS。
- 请求最多 1 MiB、最多 32 并发、上下文最多 45 秒。Host/Origin 拒绝返回 403；认证失败 401；认证封禁/并发满 429。MCP 与后台共用 IP 封禁器。
- IP 封禁及安全阈值仅进程内生效，重启重置。持久化阈值须修改 IP_BLOCK_DURATION、IP_MAX_FAILED_ATTEMPTS、IP_ATTEMPT_WINDOW 环境变量并重启。
- 不包含发信、DNS 注册商配置、服务器命令、数据库备份、子管理员管理。新增域名后仍需在 DNS 服务商配置 MX/A。

凭据轮换和回滚见 [部署与运维](mcp-deployment.md)。
