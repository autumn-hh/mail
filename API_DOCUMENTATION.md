# Maildrop 页面与 API 文档

更新日期：**2026-11-06**。依据当前 Go 服务源码 `48c4482`；线上 MCP 已复核为 **1.1.0 / 41 个工具**。
生产 HTTP 基址：`https://mail.roixw.com`；本地默认：`http://127.0.0.1:5000`。下文以生产地址举例，凭据与业务地址均为占位值。

本文覆盖 HTTP 页面、鉴权、收件、发信、发送记录和管理接口。MCP 是独立协议入口，详见 [MCP 接入](docs/mcp.md)、[41 个工具参考](docs/mcp-tools.md)、[6 个发信工具](docs/mcp-outbound.md) 和 [线上工具 Schema 快照](docs/mcp-tools.schema.json)。服务器环境配置见 [收发运行说明](docs/mail-operations.md)。

## 认证与返回约定

| 用途 | 凭据与传递方式 | 权限 |
| --- | --- | --- |
| 邮箱登录换 Token | `POST /api/get_mailbox_token`，JSON 中的 `address` + `mailbox_key` | 换取该邮箱 `access_token` |
| 收件、发信和发送记录 | `Authorization: Bearer <MAILBOX_ACCESS_TOKEN>` | 仅所属邮箱；发信还需单独授权 |
| 主管理员 API | `Authorization: Bearer <ADMIN_PASSWORD>` | `/api/admin/*`，含发信授权 |
| 主管理员注册邮箱 | `Authorization: <ADMIN_PASSWORD>` | `/api/register`，沿用原始密码头格式 |
| 子管理员注册邮箱 | `X-Sub-Admin-Token: <SUB_ADMIN_TOKEN>` | `/api/register_with_token`，受域名与额度限制，不能授予发信权限 |
| MCP 超级管理员 | `Authorization: Bearer <MCP_ADMIN_TOKEN>` | `/mcp` 的全部工具；与后台密码、邮箱 Token 相互独立 |

JSON 请求建议发送 `Content-Type: application/json`。常规 JSON 请求体上限 1 MiB；`POST /api/send` 和 MCP 为 16 MiB，包含 base64 及 JSON 开销。当前生产反代上限也为 16 MiB。时间戳除明确说明外均为 Unix 秒；大小为字节。

HTTP 响应没有统一外包装：邮箱发信/详情直接返回对象，发送列表直接返回数组；管理员读取通常返回 `{ "success": true, "data": ... }`，管理员写入通常返回 `success` 和 `message`。公开接口错误一般为 `{ "error": "原因", "message": "可选说明" }`，管理员错误一般带 `success: false`。MCP 业务结果位于 JSON-RPC 的 `result.structuredContent`，不能套用 HTTP 返回格式。

## 页面路由

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/` | 公开邮箱登录页 |
| `GET` | `/login` | 重定向到 `/` |
| `GET` | `/web` | 公开邮箱登录页 |
| `GET` | `/web/<编码邮箱>----<编码密钥>` | 收件箱页面，刷新时重新校验密钥 |
| `GET` | `/admin` | 唯一管理后台入口 |
| `GET` | `/register` | 管理员授权的邮箱注册页 |
| `GET` | `/api-test` | 只读 API 调试台 |

`/mailbox` 携带旧 address/token 查询参数的访问方式与 `/admin/mailboxes` 不保留兼容
路由，访问应返回 `404`。生成页面链接时必须分别对邮箱和密钥执行 URL 编码：

```text
/web/user%40example.com----mailbox-key
```

> `/web/...` 按产品要求将邮箱密钥保留在地址栏。该密钥可能进入浏览器历史、书签、
> 截图与反向代理访问日志；页面虽使用禁止缓存、禁止引用和禁止索引响应头，仍应将完整
> URL 视为敏感凭据。

`/api-test` 只提供以下调试流程：用邮箱地址与 `mailbox_key` 调用
`POST /api/get_mailbox_token`，再以 Bearer 令牌读取 `/api/mailbox_info_v2` 和
`/api/get_inbox`。调试页不创建或修改数据，不提供发信功能；密钥与令牌只保存在页面
内存，响应统一经 `<pre>.textContent` 输出。

## 邮箱认证流程

公开 API 仍会先应用项目配置的 IP 白名单；未放行的来源返回 `403`。

### 1. 用邮箱密钥换取访问令牌

```http
POST /api/get_mailbox_token
Content-Type: application/json
```

```json
{
  "address": "user@example.com",
  "mailbox_key": "your-mailbox-key"
}
```

成功响应：

```json
{
  "success": true,
  "address": "user@example.com",
  "access_token": "generated-access-token",
  "mailbox_id": "mailbox-uuid",
  "expires_at": 1678886400
}
```

### 2. 调用邮箱 API

后续邮箱 API 仅接受 Bearer 令牌，不接受 query string 中的旧 `token` 参数：

```http
Authorization: Bearer generated-access-token
```

公开页面只把访问令牌保存到当前会话的 `sessionStorage['maildrop_access_token']` 与内存；
邮箱密钥不写入 Web Storage。

典型错误状态：

- `401`：凭据或访问令牌无效；
- `403`：令牌不属于目标邮箱；
- `404`：邮箱或邮件不存在；
- `410`：邮箱已过期；
- `423`：邮箱已禁用。

## 公开与注册 API

### 基础信息

| 方法 | 端点 | 说明 |
| --- | --- | --- |
| `GET` | `/api/get_random_address` | 获取随机邮箱地址与可用域名 |
| `GET` | `/api/get_domain` | 获取域名配置 |
| `POST` | `/api/get_mailbox_token` | 邮箱地址 + 邮箱密钥换取访问令牌 |
| `POST` | `/api/admin_login` | JSON `{ "password": "<ADMIN_PASSWORD>" }` 校验后台密码；成功返回 `success: true` 和 `message`，不签发新 Token |
| `GET` | `/healthz` | 无需认证的数据库连通性检查；成功 `200 {"status":"ok"}`，不可用 `503 {"status":"unavailable"}` |

### 创建邮箱

#### 管理员密码注册

```http
POST /api/register
Authorization: <ADMIN_PASSWORD>
Content-Type: application/json
```

#### 子管理员令牌注册

```http
POST /api/register_with_token
X-Sub-Admin-Token: <SUB_ADMIN_TOKEN>
Content-Type: application/json
```

请求可包含 `email`、`retention_days` 和发件人白名单等字段。创建成功响应会同时返回
`access_token` 与 `mailbox_key`；公开页面链接只能使用 `mailbox_key`：

```json
{
  "success": true,
  "mailbox_created": true,
  "mailbox_address": "user@example.com",
  "access_token": "generated-access-token",
  "mailbox_key": "generated-mailbox-key",
  "retention_days": 30
}
```

### 收件箱与邮箱设置

以下接口均要求 `Authorization: Bearer <access_token>`，并校验目标邮箱归属：

| 方法 | 端点 | 说明 |
| --- | --- | --- |
| `GET` | `/api/mailbox_info_v2` | 从 Bearer 令牌推导并返回邮箱信息；响应不返回访问令牌或邮箱密钥 |
| `GET` | `/api/get_inbox?address=<邮箱>` | 获取邮件列表 |
| `GET` | `/api/get_email?address=<邮箱>&id=<邮件ID>` | 获取单封邮件 |
| `POST` | `/api/mark_email_read` | 标记单封邮件已读 |
| `POST` | `/api/mark_all_read` | 全部标记已读 |
| `POST` | `/api/delete_email` | 删除单封邮件 |
| `POST` | `/api/delete_emails_batch` | 批量删除邮件 |
| `POST` | `/api/add_sender_whitelist` | 添加发件人白名单项 |
| `POST` | `/api/remove_sender_whitelist` | 删除发件人白名单项 |
| `POST` | `/api/update_retention` | 更新保留天数 |
| `POST` | `/api/regenerate_mailbox_key` | 重新生成邮箱密钥 |
| `POST` | `/api/toggle_mailbox_status` | 启用或禁用邮箱 |
| `POST` | `/api/toggle_whitelist` | 启用或禁用白名单 |

`get_inbox` 使用 `address` 查询参数，`get_email` 使用 `address` 与 `id` 查询参数；地址型
写接口在 JSON 中提交 `address`，邮件型写接口提交 `email_id` 或 `email_ids`。这些参数
只用于定位资源，不能替代 Bearer 鉴权。

示例：

```bash
curl "https://mail.roixw.com/api/get_inbox?address=user%40example.com" \
  -H "Authorization: Bearer generated-access-token"
```

## 邮件正文、原文与附件

以下接口使用邮箱 Bearer Token；邮箱由 Token 确定，不需要 `address`。路径中的 ID 必须属于该邮箱，否则返回 `404`。

| 方法 | 路径 | 成功响应 |
| --- | --- | --- |
| `GET` | `/api/emails/{id}/content` | `200`，直接返回正文详情对象 |
| `GET` | `/api/emails/{id}/raw` | `.eml` 原文，`message/rfc822` |
| `GET` | `/api/emails/{id}/attachments/{attachment}` | 附件二进制，ID 来自详情的 `attachments[].id` |

详情字段：`message_id`、`reply_to`、`to`、`cc`、`references`、`in_reply_to`、`date`、`text`、`html`、`attachments`、`authentication`、`warnings`。空的可选字段可能省略。附件元数据包含 `id`、`filename`、`content_type`、`content_id`（可选）、`inline`、`size`，不包含附件字节。

`authentication.spf`、`authentication.dmarc` 是对象，`authentication.dkim` 是数组，各项包含 `result` 和可选 `domain` / `reason`；`authentication.policy` 可选。这是本服务收件时的验证结果，不是对外发送邮件的收件方反馈。旧邮件未存原文时，上述内容、原文和附件接口可能返回 `404`，仍可通过原有 `/api/get_email` 阅读。

下载响应带 `Content-Disposition: attachment`、`X-Content-Type-Options: nosniff` 和限制性 CSP。浏览器下载须用带 Authorization 的请求，不能把 Token 放入链接。正文和附件均为不可信邮件内容，HTML 应隔离显示。

## 发信授权与额度

| 方法 | 路径 | 凭据 / 成功响应 |
| --- | --- | --- |
| `GET` | `/api/sending-policy` | 邮箱 Token；`200`，直接返回策略 |
| `GET` | `/api/admin/mailboxes/{id}/sending-policy` | 主管理员密码；`200`，策略在 `data` 中 |
| `PUT` | `/api/admin/mailboxes/{id}/sending-policy` | 主管理员密码；`200`，返回 `success` 和 `message` |

PUT 必须同时提供全部三个字段：

```json
{"enabled": true, "daily_limit": 100, "minute_limit": 10}
```

`enabled` 为布尔值，`daily_limit` 为 1–100000 的整数，`minute_limit` 为 1–1000 的整数。成功返回：

```json
{"success": true, "message": "发件权限与额度已保存"}
```

邮箱读取策略示例（已配置服务器、该邮箱尚未授权）：

```json
{
  "enabled": false,
  "daily_limit": 100,
  "minute_limit": 10,
  "daily_used": 0,
  "minute_used": 0,
  "storage_used": 0,
  "storage_limit": 104857600,
  "available": true
}
```

`enabled` 是邮箱授权；`available` 只检查全局出站开关和 SMTP 主机名格式，不能证明 DKIM、DNS、25 端口或实际送达。不可用时还返回 `reason`。新邮箱默认未授权；默认额度每天 100、滚动 60 秒内 10 个去重收件人，每日按北京时间 00:00 重置。To/Cc/Bcc 一并计数，成功入队即扣额，重试不重复扣额，失败、退信或删除不返还额度。

撤权会终止 queued/retry 任务；已开始的网络投递可能完成一次。重新授权不会恢复已取消的旧任务。当前生产服务器已开启出站，但不会因此自动授权所有邮箱。

## 提交邮件

`POST /api/send` 使用邮箱 Bearer Token，必须提供 `Idempotency-Key` 请求头。发件人固定为当前邮箱；不接受 `from`、`mailbox`、`idempotency_key` 等额外 JSON 字段。

| 字段 | 类型 | 规则 |
| --- | --- | --- |
| `to` / `cc` / `bcc` | string[] | 合计输入最多 100 项，规范化去重后至少 1 个；允许只填 Cc/Bcc |
| `subject` | string | 默认空，最多 998 个 UTF-8 字节，不能含 CR/LF/NUL |
| `text` / `html` | string | 可选，允许空正文，两者 UTF-8 字节合计最多 1 MiB |
| `reply_email_id` | string | 可选，当前邮箱收到的原邮件 ID；设置回复邮件头，不代填收件人/主题 |
| `attachments` | object[] | 可选，最多 10 个，解码后合计最多 10 MiB |
| `attachments[].filename` | string | 文件名；去除目录部分后最长 240 字节，不得含 CR/LF/NUL |
| `attachments[].content_type` | string | 可选；省略时根据文件字节检测 MIME 类型 |
| `attachments[].content` | string | 标准 base64 字节，不接受文件路径或 URL |

HTTP 中省略 `to` / `subject` 分别视为空列表 / 空主题；MCP 的 Schema 要求显式传入这两个字段。附件的 `id` / `size` 为服务器生成的输出元数据，不应在发送时设置。收件地址用户名只支持 ASCII，域名支持 IDN 转 Punycode。Bcc 不进入对外 MIME 头。

以下是 Bash/curl 示例；先将邮件保存为 `send.json`：

```json
{
  "to": ["customer@example.net"],
  "subject": "资料已收到",
  "text": "您好，附件请查收。",
  "attachments": [
    {"filename": "note.txt", "content_type": "text/plain", "content": "aGVsbG8="}
  ]
}
```

```bash
curl --fail-with-body "https://mail.roixw.com/api/send" \
  -H "Authorization: Bearer $MAILBOX_ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-20261106-001" \
  --data-binary @send.json
```

成功返回 `202` 和直接的发送记录对象（HTTP 返回含正文，MCP 发送响应仅含摘要）：

```json
{
  "id": "outbound-record-id",
  "created_at": 1793923200,
  "status": "queued",
  "message_id": "<outbound-record-id@example.com>",
  "details": {
    "to": ["customer@example.net"],
    "subject": "资料已收到",
    "text": "您好，附件请查收。",
    "attachments": [{"filename": "note.txt", "content_type": "text/plain", "id": "1", "size": 5}]
  },
  "recipients": [{"address": "customer@example.net", "status": "queued", "attempts": 0}],
  "bounce_count": 0
}
```

`Idempotency-Key` 为 1–128 字节，不得含 CR/LF；为每封业务邮件生成稳定且唯一的键。超时或结果不明时必须用**原键与原内容**重试。相同键和相同规范化内容返回原记录，可能已是投递后的状态；不同内容复用键返回 `409`。记录删除或过期后复用旧键返回 `410`，不会重新发送。幂等墓碑保留到邮箱被删除。

整个请求 JSON 最多 16 MiB，编码后邮件最多 20 MiB。每邮箱发件存档独立限额 100 MiB，包含原文和退信，保留 30 天。额度、容量校验、幂等和入队同事务；公网投递在后台异步执行。

## 发送记录、状态和退信

以下接口都使用邮箱 Token。

| 方法 | 路径 | 成功响应 |
| --- | --- | --- |
| `GET` | `/api/sent?offset=0` | `200`，发送记录数组，最多 50 项，无正文，无总数 |
| `GET` | `/api/sent/{id}` | `200`，完整发送记录，不含附件字节 |
| `GET` | `/api/sent/{id}/raw` | 发件 `.eml` 原文，不含 Bcc 头 |
| `GET` | `/api/sent/{id}/attachments/{attachment}` | 附件二进制 |
| `DELETE` | `/api/sent/{id}` | `200`，`success: true`、`message: "发送记录已删除，额度不会恢复"` |
| `GET` | `/api/delivery-reports/{id}/raw` | 退信原文；ID 取自发送记录的 `reports[].id` |

列表按创建时间及记录顺序倒序。`offset` 默认 0，负数或普通非数字输入按 0 处理；下一页加 50，返回 `[]` 表示结束。新邮件可能改变分页偏移。

发送记录字段与上面的 `202` 示例相同；详情额外保留正文，列表省略 `details.text/html`。`details` 保留收件人（含私有 Bcc）、主题、回复 ID 和附件元数据。`recipients[]` 包含 `address`、`status`、`attempts`，可附 `error`、`uncertain`。`reports[]` 可选，每项为 `id`、`received_at`、`summary`；`bounce_count` 为关联报告数。

| 状态 | 含义 |
| --- | --- |
| `queued` | 汇总状态表示还有待处理收件人；具体收件人可能为 queued / retry / sending |
| `retry` / `sending` | 收件人级状态：等待重试 / 正在投递 |
| `accepted` | 对方 SMTP 在 DATA 后确认接受；不代表进入收件箱 |
| `failed` / `partial_failed` | 全部失败 / 部分失败且部分已被接受 |
| `bounced` | 已关联退信；应检查各收件人状态与报告 |

`uncertain: true` 表示可能重复，例如 DATA 确认丢失。临时错误会自动退避重试，最多到入队后 24 小时；查询接口不会触发重发。待投递记录不可删除（`409`），删除已结束记录不会撤回对方邮件。正文、SMTP 错误和退信摘要均为不可信数据。

### 发信与内容接口错误

| HTTP 状态 | 常见原因 |
| --- | --- |
| `400` | 非法 JSON/未知字段、幂等键缺失、无效地址或主题、超过 100 个收件人、base64 错误、策略值非法 |
| `401` | 邮箱 Token 或管理员凭据无效/缺失 |
| `403` | IP 白名单拒绝、邮箱未授权发信、目标邮箱归属错误 |
| `404` | 邮件/邮箱/附件/退信不存在或不属于当前邮箱，旧邮件没有原文，回复 ID 不属于当前邮箱 |
| `409` | 同一幂等键内容冲突、发件存档已满、删除尚未结束的发送记录 |
| `410` | 邮箱过期，或幂等键对应的存档已删除/过期 |
| `413` | 请求体、正文、附件数/大小或编码后邮件超过上限 |
| `423` | 邮箱停用；提交过程中邮箱过期/停用也可能返回此码 |
| `429` | 每日或滚动分钟收件人数额度不足 |
| `503` | 服务器出站未配置或 DKIM 签名不可用 |
| `500` | 内部处理失败；发信重试仍须复用原键和原内容 |

## 管理员 API

管理员路由统一使用 `/api/admin` 前缀，并严格要求
`Authorization: Bearer <ADMIN_PASSWORD>`。部署未配置 `PASSWORD` 时拒绝管理请求。

### 邮箱与统计

| 方法 | 端点 | 说明 |
| --- | --- | --- |
| `GET` | `/api/admin/mailboxes` | 分页查询邮箱 |
| `GET` | `/api/admin/mailboxes/<mailbox_id>` | 获取邮箱详情 |
| `POST` | `/api/admin/mailboxes` | 创建邮箱 |
| `PUT` | `/api/admin/mailboxes/<mailbox_id>` | 更新邮箱 |
| `DELETE` | `/api/admin/mailboxes/<mailbox_id>` | 删除或软删除邮箱 |
| `POST` | `/api/admin/mailboxes/batch-delete` | 批量删除邮箱 |
| `POST` | `/api/admin/mailboxes/<mailbox_id>/reset-token` | 重置访问令牌 |
| `POST` | `/api/admin/mailboxes/<mailbox_id>/enable` | 恢复邮箱 |
| `GET` | `/api/admin/mailboxes/<mailbox_id>/audit-logs` | 查询邮箱审计日志 |
| `GET` | `/api/admin/audit-logs` | 查询全局审计日志 |
| `GET` | `/api/admin/stats` | 系统统计 |
| `GET` | `/api/admin/source-stats` | 创建来源统计 |

管理员创建邮箱成功时，`data` 包含 `address`、`access_token` 与 `mailbox_key`。管理端
生成的公开链接格式同样只能是 `/web/<编码邮箱>----<编码密钥>`。

### 安全配置与子管理员

| 方法 | 端点 | 说明 |
| --- | --- | --- |
| `GET` | `/api/admin/blocked-ips` | 查询被封禁 IP |
| `DELETE` | `/api/admin/blocked-ips/<ip>` | 解除 IP 封禁 |
| `GET` | `/api/admin/security-config` | 查询安全配置 |
| `PUT` | `/api/admin/security-config` | 更新安全配置 |
| `GET` | `/api/admin/sub-admins` | 查询子管理员 |
| `POST` | `/api/admin/sub-admins` | 创建子管理员 |
| `PUT` | `/api/admin/sub-admins/<sub_admin_id>` | 更新子管理员 |
| `DELETE` | `/api/admin/sub-admins/<sub_admin_id>` | 删除子管理员 |

### 域名设置

收信域名保存在数据库中，由管理后台「域名设置」维护，修改后立即生效。`.env` 的 `DOMAINS`
只在域名表为空（首次启动）时导入。`/api/get_random_address`、`/api/get_domain` 与注册接口
都只使用已启用的域名；没有任何启用域名时返回 `503`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/admin/domains` | 查询全部域名 |
| `POST` | `/api/admin/domains` | 添加域名，请求体 `{"domain": "example.com"}`，成功返回 `201` |
| `PUT` | `/api/admin/domains/<domain>` | 启用或禁用，请求体 `{"enabled": true}` |
| `DELETE` | `/api/admin/domains/<domain>` | 删除域名 |
| `POST` | `/api/admin/domains/batch-add` | 批量添加，请求体 `{"domains": ["a.com", "b.com"]}` |
| `POST` | `/api/admin/domains/batch-delete` | 批量删除，请求体同上 |

批量接口每次最多 200 个域名，空白条目会被忽略。

- **批量添加**：部分成功。有效的新域名在同一事务中按输入顺序加入，其余写入 `skipped` 并说明原因（`域名格式不正确`、`重复输入`、`域名已存在`）。
  返回 `{"success": true, "data": {"added": [域名对象...], "skipped": [{"domain": "...", "reason": "..."}]}}`。
- **批量删除**：存在的域名在同一事务中删除，不存在的写入 `failed`。如果整批删除后不剩任何启用的域名，整批拒绝并返回 `409`，一个都不删。
  返回 `{"success": true, "data": {"deleted": ["a.com"], "failed": [{"domain": "...", "reason": "域名不存在"}]}}`。
- 请求体不是字符串列表或条目全为空返回 `400 domains必须是非空字符串列表`，超过 200 个返回 `400 单次最多处理 200 个域名`。

域名对象格式：`{"domain": "example.com", "enabled": true, "created_at": 1800000000, "updated_at": 1800000000}`。
域名会先规范化：去掉首尾空白，转成小写，去掉末尾的点，国际化域名转为 Punycode。

| 状态码 | `error` | 场景 |
| --- | --- | --- |
| `400` | `域名格式不正确` | 域名缺失或格式非法 |
| `400` | `enabled必须是布尔值` | `PUT` 的 `enabled` 缺失或不是布尔值 |
| `404` | `域名不存在` | 路径中的域名不存在 |
| `409` | `域名已存在` | 重复添加 |
| `409` | `至少需要保留一个启用的域名` | 删除或禁用最后一个启用的域名 |

子管理员注册只能使用自己授权的域名，其中在「域名设置」里被禁用的域名会被排除；不在域名表中的授权域名仍可使用。

## 已移除或不兼容

- 旧 `/api/send_test_email` 已删除；授权发信使用 `/api/send` 或 MCP `maildrop_send_email`，回复使用 `reply_email_id`；
- 未使用且允许请求指定文件路径的旧迁移、导出 API 已删除；
- `/api/create_mailbox_v2` 已移除且未注册，请求返回 `404`；
- 无调用方且会批量返回访问令牌的 `/api/user_login` 已移除，请求返回 `404`；
- 会直接改写 `.env` 的旧 `/api/admin/whitelist` 与 `/api/admin/test_ip` 已移除；
- `/mailbox` 旧 token 页面链接不兼容；
- `/admin/mailboxes` 旧管理页面入口不兼容。

SMTP 监听负责收件和关联退信，独立的持久队列 worker 负责向目标 MX 直接投递出站邮件。202 入队和 accepted 状态均不保证收件箱位置；2026-11-06 的生产实测邮件已由收件人确认在 QQ 垃圾邮件中收到。
