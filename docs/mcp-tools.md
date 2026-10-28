# Maildrop MCP 工具详细参考

版本依据：生产代码提交 `199c527`，于 2026-10-28 对 `https://mail.roixw.com/mcp` 执行 initialize 与 tools/list 获取 35 项输入 Schema，再对照 internal/mcpapi、internal/admin、internal/inbox、internal/store 实现编写。本文不包含真实认证值。所有地址、ID、邮件内容、统计数字和时间示例均为构造数据，调用前替换。示例写入操作仅供参考，本次文档核对没有在生产执行这些变更。

## 1. 快速定位

完整工具名以 `maildrop_` 开头。每个工具在后文都有参数表、请求示例、响应示例、逐字段返回说明、规则、失败条件和副作用。

- 邮箱：生成/创建、批量创建、分页/详情、更新、启停、停用/硬删除、API Token与网页Key轮换。
- 发件限制：白名单查看/增删/开关；`allowed_domains` 在邮箱创建/更新中配置。
- 邮件：摘要列表、按条件搜索、读取正文、等待、已读/未读、全读、事务批删。
- 系统：健康与能力、收件域名、全局/来源统计、审计、IP封禁及阈值。

## 2. 统一调用与响应

**入口**：`https://mail.roixw.com/mcp`，Streamable HTTP。

每个请求携带 `Authorization: Bearer <MCP_ADMIN_TOKEN>`。后台管理员密码、邮箱access_token、mailbox_key都不是MCP认证凭据。使用私密交付文档或本地mcp-client.json中的Token。每个工具均有完整超级管理员权限，没有按邮箱隔离的MCP账号。

请求头：

```http
Content-Type: application/json
Accept: application/json, text/event-stream
Authorization: Bearer <MCP_ADMIN_TOKEN>
MCP-Protocol-Version: <initialize返回的协议版本>
```

先 `initialize`，取其 `result.protocolVersion`；再发送 `notifications/initialized`，随后 `tools/list` 或 `tools/call`。不要把应用 `mcp_version=1.0.0` 当成协议头中的日期版本。

初始化示例：

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"maildrop-client","version":"1.0"}}}
```

初始化通知，无id：

```json
{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
```

本服务无状态，不需要长期保存会话ID，也不提供旧式独立SSE GET入口。客户端需要支持Streamable HTTP和自定义Authorization头。通常使用SDK，让SDK处理协议握手。远程普通浏览器页面直接调用还会受同源/CORS限制；从其他网页Origin直接请求不是推荐的接入方式。

下面是一项完整请求，后文每项的“调用参数”示例就是该结构的 `params`（含name和arguments），不是独立HTTP请求体：

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"maildrop_health","arguments":{}}}
```

典型成功响应：

```json
{
  "jsonrpc":"2.0",
  "id":2,
  "result":{
    "content":[{"type":"text","text":"{\"status\":\"ok\",\"time\":\"2026-10-28T09:00:00Z\"}"}],
    "structuredContent":{"status":"ok","time":"2026-10-28T09:00:00Z"}
  }
}
```

客户端先处理HTTP失败和JSON-RPC顶层error，再检查工具 `result.isError`。成功时优先取 `result.structuredContent`，兼容旧客户端可解析 `result.content` 中text的JSON。后文“返回数据”仅展示此业务数据，不重复外层。list_domains、list_audit_logs会返回数组，不应假设全部工具的structuredContent都是对象。当前工具未提供约束返回数据的outputSchema，字段依据本版实现；不要依赖没有被承诺的附加字段。

典型业务错误：

```json
{"jsonrpc":"2.0","id":3,"result":{"isError":true,"content":[{"type":"text","text":"Mailbox not found"}]}}
```

业务错误可能在HTTP 200中出现；Schema错误/协议错误可能是JSON-RPC顶层error。批量工具也可能返回成功的MCP结果，但其中有 `success=false` 或 `skipped/failed` 条目，需要再逐项检查。

## 3. 共用数据与行为

### 3.1 输入类型、默认值与邮箱引用

所有参数名区分大小写。boolean必须为true/false，数字用JSON number，数组用JSON array；不把字符串"false"当作false。Schema拒绝未声明的字段，因此不能额外传password、token、role、confirm等参数。

参数表“必填”由线上Schema生成；“默认/省略语义”取自服务端代码，Schema本身没有声明default。部分指针/数组在Schema允许null，但约束可能在业务层拒绝或将其当作不更新。推荐：无需改变的字段直接省略，清空规则传[]，需要关闭传false，避免用null表达业务意图。

`mailbox` 可传现存邮箱ID或完整地址；ID最稳妥。服务不统一规范化查询地址大小写/空白，直接使用创建结果。即使工具为超级管理员，也会核对email_id确实属于传入的邮箱。

创建不传domain时选启用收件域名列表中的第一个，不保证网站访问域名就是邮箱收件域名，先list_domains。域名管理支持Punycode规范化；但本版管理员邮箱地址还受传统地址正则限制，某域名能添加成功不等于任意IDN后缀都能创建邮箱，须以create_mailbox校验为准。

### 3.2 保留、启用、停用与删除

- MCP创建/更新的retention_days为1..MAX_RETENTION_DAYS，缺省使用服务环境MAILBOX_RETENTION_DAYS（代码默认30）；MAX_RETENTION_DAYS代码默认36500，实际部署可覆盖。0/-1不能表示永久。
- 邮箱到期与邮件清理是两条约束。邮件同时受全局EMAIL_RETENTION_SECONDS/DAYS影响，邮箱保留14天不代表每封信必然保存14天。
- 管理启用不延长邮箱期限；到期后需更新retention_days。停用保留数据，但原有过期清理仍可能在之后删除过期邮箱和邮件。
- delete_mailbox默认停用；permanent=true才硬删除。邮件delete_emails始终永久删除，没有回收站。
- 生成地址不创建邮箱。等待邮件不创建邮箱、不延长期限，也不自动标已读。

### 3.3 发件限制的真实匹配

`sender_whitelist` 与 `allowed_domains` 是两层独立规则，按解析后的邮件头From校验，不是SPF/DKIM身份验证。两层都须通过。

| 配置 | 规则 |
| --- | --- |
| whitelist_enabled=false | 跳过sender_whitelist；allowed_domains仍有效 |
| whitelist_enabled=true且白名单为空 | 白名单层放行，不是拒绝全部 |
| sender_whitelist含sender@example.net | 完整From忽略大小写匹配 |
| sender_whitelist含@example.net或裸example.net | 按From的@example.net后缀匹配，不自动放行sub.example.net |
| allowed_domains=[] | 不限制发件域名 |
| allowed_domains含example.net | 允许该域名及其子域；与whitelist_enabled无关 |

create/update整组白名单的校验要求完整地址或@域名，而单条add/remove还接受裸域名；为保持可移植性建议用完整地址或@域名。单条增删去重/删除按字符串精确比较，收信匹配才忽略大小写。规则修改不追溯删除既有信件。

### 3.4 邮件摘要与详情字段

摘要（list/search/wait）字段如下，**均为小写**：

| 字段 | 类型/单位 | 含义 |
| --- | --- | --- |
| id | string | 邮件ID，传给get_email/set_email_read/delete_emails |
| from / to | string | 已解析发件人/收件人 |
| subject | string | 主题；历史空主题转空字符串 |
| timestamp | integer，Unix秒 | 入库接收时间，用于过滤排序 |
| is_read | boolean | 已读标记 |
| content_type | string | Text/HTML等正文类型 |
| size_bytes | integer，字节 | 邮件存储记账大小 |

详情的email对象字段兼容原API，**部分首字母大写**：

| 字段 | 类型/单位 | 含义 |
| --- | --- | --- |
| id / mailbox_id | string | 邮件及所属邮箱ID；mailbox_id为空时可省略 |
| From / To / Subject | string，可历史null | 发件人、收件人、主题 |
| Body | string，可历史null | 解析后的正文；可能按max_body_chars截断 |
| ContentType | string | Text或HTML等；HTML须作为不可信内容处理 |
| Timestamp | integer，Unix秒 | 接收时间 |
| Sent | string，可历史null | 保存的格式化时间文本，不是可靠时间筛选参数 |
| is_read | boolean | 已读标记 |

因此读正文是 `result.structuredContent.email.Body`；不能用 `email.body`。详情email不返回size_bytes，如需容量用摘要。并无附件、原始EML或验证码专用提取工具；验证码从正文自行提取。

### 3.5 邮箱摘要与详情字段

| 字段 | 类型/单位 | 摘要/详情含义 |
| --- | --- | --- |
| id / address | string | 邮箱标识/完整地址 |
| created_at / expires_at | Unix秒 | 创建/到期时间 |
| retention_days | integer，可历史null | 设定保留天数 |
| is_active / admin_disabled / is_expired | boolean | 当前状态/管理停用标记/是否过期 |
| sender_whitelist / whitelist_enabled | string[] / boolean | 发件白名单及开关 |
| created_by_ip | string或null | 创建来源IP |
| last_accessed | Unix秒或null | 历史访问/收信记录；MCP读取不更新它 |
| updated_by_admin / updated_at | string/null；Unix秒/null | 管理员更新记录 |
| created_source | string/null | 只在列表，详情不包含；MCP创建复用admin来源 |
| email_count / unread_count | integer | 邮件及未读数量 |
| last_email_time | Unix秒/null | 仅详情，最近邮件时间 |
| allowed_domains | string[] | 仅详情，独立的允许发件域名 |
| storage_used / storage_limit | 字节 | 仅详情，已用/上限 |
| storage_used_mb / storage_limit_mb | number | 仅详情，字节除1048576 |
| storage_percent | number | 仅详情，百分数0–100为常见范围，非0–1小数 |
| access_token / mailbox_key | string或历史null | 仅显式include_credentials=true的详情返回 |

旧数据可能有null/遗留类型；示例是正常记录。不要把返回列表/详情的字段全部作为update_mailbox参数回传，该工具仅接受文档列出的可更新项。

### 3.6 限额与重试

| 项目 | 限额或规则 |
| --- | --- |
| HTTP请求体 | 最多1MiB；反代可能更早拒绝 |
| 并发 | 最多32个已认证在途MCP请求 |
| 请求上下文 | 最多45秒；底层个别非context调用不承诺强制实时中断 |
| 邮箱批量创建/删除 | 1–100输入项 |
| 域名批量添加/删除 | 1–200输入项 |
| 邮件批删 | 1–200输入项，之后按ID去重 |
| 邮件每页 | 默认20，最多100 |
| 邮箱每页 | 默认20，最多200 |
| 等待 | 默认20秒，最多25秒 |
| 正文 | 默认20000，最多200000个Unicode字符 |
| 审计读取 | 默认100，最多1000，无游标 |

工具annotations中的幂等提示针对业务状态，不保证重复调用的审计、updated_at或计数响应相同。不能自动重试创建、轮换、永久删除、批量变更或重算期限。网络中断/取消不等于服务器回滚；先查询当前状态，再决定重试单项。

## 4. 35个工具的逐项参考

- [01. maildrop_generate_address：生成候选邮箱地址](#maildrop_generate_address)
- [02. maildrop_create_mailbox：创建单个邮箱](#maildrop_create_mailbox)
- [03. maildrop_create_mailboxes_batch：批量创建邮箱](#maildrop_create_mailboxes_batch)
- [04. maildrop_list_mailboxes：分页列出邮箱](#maildrop_list_mailboxes)
- [05. maildrop_get_mailbox：查询邮箱详情](#maildrop_get_mailbox)
- [06. maildrop_update_mailbox：修改邮箱配置](#maildrop_update_mailbox)
- [07. maildrop_set_mailbox_status：启用或停用邮箱](#maildrop_set_mailbox_status)
- [08. maildrop_delete_mailbox：停用或永久删除邮箱](#maildrop_delete_mailbox)
- [09. maildrop_delete_mailboxes_batch：批量停用或永久删除邮箱](#maildrop_delete_mailboxes_batch)
- [10. maildrop_reset_mailbox_token：轮换邮箱 API Token](#maildrop_reset_mailbox_token)
- [11. maildrop_rotate_mailbox_key：轮换邮箱网页访问 Key](#maildrop_rotate_mailbox_key)
- [12. maildrop_get_sender_whitelist：查看发件白名单](#maildrop_get_sender_whitelist)
- [13. maildrop_add_sender_whitelist：添加单条发件白名单](#maildrop_add_sender_whitelist)
- [14. maildrop_remove_sender_whitelist：移除单条发件白名单](#maildrop_remove_sender_whitelist)
- [15. maildrop_set_whitelist_enabled：设置发件白名单开关](#maildrop_set_whitelist_enabled)
- [16. maildrop_list_emails：分页查看邮件摘要](#maildrop_list_emails)
- [17. maildrop_search_emails：按条件检索邮件](#maildrop_search_emails)
- [18. maildrop_get_email：读取单封邮件内容](#maildrop_get_email)
- [19. maildrop_wait_for_email：等待匹配邮件](#maildrop_wait_for_email)
- [20. maildrop_set_email_read：设置单封邮件已读状态](#maildrop_set_email_read)
- [21. maildrop_mark_all_read：将邮箱全部邮件标记已读](#maildrop_mark_all_read)
- [22. maildrop_delete_emails：永久批量删除邮件](#maildrop_delete_emails)
- [23. maildrop_health：检查MCP数据库连接](#maildrop_health)
- [24. maildrop_server_info：查看MCP能力与限额](#maildrop_server_info)
- [25. maildrop_list_domains：列出收件域名](#maildrop_list_domains)
- [26. maildrop_add_domains：批量添加收件域名](#maildrop_add_domains)
- [27. maildrop_set_domain_enabled：启用或禁用收件域名](#maildrop_set_domain_enabled)
- [28. maildrop_delete_domains：批量删除收件域名](#maildrop_delete_domains)
- [29. maildrop_get_stats：查看全局业务统计](#maildrop_get_stats)
- [30. maildrop_get_source_stats：查看邮箱创建来源统计](#maildrop_get_source_stats)
- [31. maildrop_list_audit_logs：查询审计日志](#maildrop_list_audit_logs)
- [32. maildrop_list_blocked_ips：查看当前IP封禁](#maildrop_list_blocked_ips)
- [33. maildrop_unblock_ip：解封单个IP](#maildrop_unblock_ip)
- [34. maildrop_get_security_config：查询认证失败封禁阈值](#maildrop_get_security_config)
- [35. maildrop_update_security_config：修改运行期封禁阈值](#maildrop_update_security_config)

<a id="maildrop_generate_address"></a>

### 01. maildrop_generate_address — 生成候选邮箱地址

用于先向调用方展示地址；不会插入邮箱记录。省略 domain 时选择启用域名按创建时间/插入顺序排列的第一个。随机用户名为 m 加 12 位十六进制字符。

**行为标记**：只读；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| domain | string | 否 | 首个启用收件域名 | 仅传域名，无协议/路径；必须是已启用收件域名。若和address同时传入必须一致。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_generate_address",
  "arguments": {
    "domain": "example.com"
  }
}
```

**成功返回数据示例**：

```json
{
  "address": "m012345abcdef@example.com"
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| address | string | 完整邮箱地址；示例域名需替换为实际启用收件域名。 |

**规则与边界**：仅生成候选值；不检查地址是否已存在，也不保证之后创建成功。拿到地址后必须调用 create_mailbox。重复调用生成不同值。

**失败条件**：域名非法、无启用域名、指定域名未启用。

**副作用与审计**：无写入，无审计。

<a id="maildrop_create_mailbox"></a>

### 02. maildrop_create_mailbox — 创建单个邮箱

可指定完整 address，也可省略让服务随机生成；所有参数都可省略。address 与 domain 同时给出时必须一致。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| address | string | 否 | 省略时随机生成 | 必须为完整邮箱地址；用户名3–20位英文字母/数字。 |
| domain | string | 否 | 首个启用收件域名 | 仅传域名，无协议/路径；必须是已启用收件域名。若和address同时传入必须一致。 |
| retention_days | null/integer | 否 | 创建：MAILBOX_RETENTION_DAYS；更新：保持原值 | 整数1..MAX_RETENTION_DAYS；不接受0/-1。更新时从当前时间重算到期日。 |
| sender_whitelist | null/array，元素string | 否 | 创建：[]；更新：保持原值 | 字符串数组；完整发件地址或@域名。创建非空自动开启；更新整组替换，[]清空但不改开关。 |
| allowed_domains | null/array，元素string | 否 | 创建：[]；更新：保持原值 | 允许的发件域名数组；整组替换。[]不限制发件域名，独立于白名单开关；支持域名及其子域。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_create_mailbox",
  "arguments": {
    "address": "demo123@example.com",
    "retention_days": 7,
    "sender_whitelist": [
      "sender@example.net",
      "@example.org"
    ],
    "allowed_domains": [
      "example.net",
      "example.org"
    ]
  }
}
```

**成功返回数据示例**：

```json
{
  "id": "mb-example-id",
  "address": "demo123@example.com",
  "created_at": 1793145600,
  "expires_at": 1793750400,
  "retention_days": 7,
  "sender_whitelist": [
    "sender@example.net",
    "@example.org"
  ],
  "whitelist_enabled": true,
  "access_token": "<NEW_MAILBOX_TOKEN>",
  "mailbox_key": "<NEW_MAILBOX_KEY>",
  "is_active": true,
  "created_source": "admin",
  "allowed_domains": [
    "example.net",
    "example.org"
  ]
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| address | string | 完整邮箱地址；示例域名需替换为实际启用收件域名。 |
| created_at | integer | 对象创建时间，Unix秒。 |
| expires_at | integer | 邮箱到期时间，Unix秒。 |
| retention_days | integer | 邮箱设定保留天数，不保证每封邮件都保留同样天数。 |
| sender_whitelist | array | 发件白名单字符串数组；具体匹配见3.3。 |
| whitelist_enabled | boolean | 当前发件白名单开关。 |
| access_token | string | 邮箱API凭据；敏感值，不是MCP超管Token。 |
| mailbox_key | string | 邮箱网页访问Key；敏感值。 |
| is_active | boolean | 邮箱是否处于启用状态，不替代过期判断。 |
| created_source | string | 邮箱创建来源；MCP创建仍为admin。 |
| allowed_domains | array | 允许的发件域名数组；不是收件域名。 |

**规则与边界**：用户名仅 3–20 位英文字母/数字，受保护地址禁止。非空 sender_whitelist 在创建时自动开启白名单。created_source 仍记作 admin，审计用户为 mcp-superadmin。保留期是邮箱到期时间；邮件还受全局 EMAIL_RETENTION_* 清理约束。

**失败条件**：已存在地址、保护地址、邮箱格式错误、未启用域名、非法保留天数或发件规则。过期地址尚在数据库时也可能因唯一约束创建失败；不能依赖自动复用。

**副作用与审计**：创建邮箱、规则和 CREATE 审计在同一事务。返回的邮箱凭据需要妥善保存；不是 MCP 超管 Token。

<a id="maildrop_create_mailboxes_batch"></a>

### 03. maildrop_create_mailboxes_batch — 批量创建邮箱

一次处理 1–100 个创建配置；每项字段与 create_mailbox 完全相同，允许空对象表示随机创建。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| items | null/array，元素object | 是 | 无 | 数组1–100项；每项为create_mailbox输入对象，支持{}随机创建。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_create_mailboxes_batch",
  "arguments": {
    "items": [
      {
        "address": "demo123@example.com",
        "retention_days": 7
      },
      {
        "address": "demo456@example.com",
        "retention_days": 7
      }
    ]
  }
}
```

**成功返回数据示例**：

```json
{
  "results": [
    {
      "index": 0,
      "success": true,
      "mailbox": {
        "id": "mb-example-id",
        "address": "demo123@example.com",
        "access_token": "<NEW_MAILBOX_TOKEN>",
        "mailbox_key": "<NEW_MAILBOX_KEY>"
      }
    },
    {
      "index": 1,
      "success": false,
      "error": "邮箱地址已存在"
    }
  ]
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| results | array | 按输入顺序排列的逐项结果数组。 |
| results[].index | integer | 对应输入items的下标，从0开始。 |
| results[].success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |
| results[].mailbox | object | 创建时为邮箱结果对象；批删时为回显输入的邮箱引用。 |
| results[].mailbox.id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| results[].mailbox.address | string | 完整邮箱地址；示例域名需替换为实际启用收件域名。 |
| results[].mailbox.access_token | string | 邮箱API凭据；敏感值，不是MCP超管Token。 |
| results[].mailbox.mailbox_key | string | 邮箱网页访问Key；敏感值。 |

成功项mailbox完整字段与create_mailbox一致，上面为节选；失败项含index/success/error。处理中断时可能另有interrupted=true。

**规则与边界**：按输入顺序处理，index 从 0 开始。每项成功或失败独立返回，成功 mailbox 的字段与单创建一致；示例仅节选 mailbox 字段。取消时可能返回 interrupted=true，也可能连接先取消而没有完整结果。

**失败条件**：items 为空或超过100整次失败；单项错误记录在该项 error，其他项继续。

**副作用与审计**：逐邮箱独立事务，可部分成功；成功邮箱不会因其他项失败而回滚。重试前先查地址，不能盲目重试整个批次。

<a id="maildrop_list_mailboxes"></a>

### 04. maildrop_list_mailboxes — 分页列出邮箱

按地址或 ID 搜索，并按状态、来源筛选，返回邮箱摘要。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| page | integer | 否 | 1；显式0也使用1 | 整数1–1000000，1起始；等待工具建议固定第1页。 |
| page_size | integer | 否 | 20；显式0也使用20 | 整数1–200；默认20；超出结果范围返回空页。 |
| search | string | 否 | 空，不过滤 | 邮箱地址或ID的SQL LIKE片段，%/_可当通配符。 |
| status | string | 否 | 空，全部 | 仅空字符串、active、expired、disabled。 |
| source | string | 否 | 空或all，不过滤 | 按created_source精确匹配；用get_source_stats获取实际键。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_list_mailboxes",
  "arguments": {
    "page": 1,
    "page_size": 20,
    "search": "demo",
    "status": "active",
    "source": "admin"
  }
}
```

**成功返回数据示例**：

```json
{
  "mailboxes": [],
  "total": 0,
  "page": 1,
  "page_size": 20,
  "total_pages": 0
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailboxes | array | 邮箱摘要数组，单项完整字段见3.5。 |
| total | integer | 应用过滤后所有匹配项数量，不是当前页长度。 |
| page | integer | 服务端实际使用的页码，1起始。 |
| page_size | integer | 服务端实际使用的每页大小。 |
| total_pages | integer | 邮箱列表总页数，匹配0条时为0。 |

**规则与边界**：按 created_at 降序。search 使用 SQL LIKE：% 与 _ 有通配意义，不同于邮件搜索的字面子串。source 可用 get_source_stats 获取实际来源值，空或 all 不过滤。active 为 is_active=1 且未到期，disabled 为 is_active=0，expired 按到期时间判断。

**失败条件**：page 不在1–1000000或 page_size不在1–200；status 非空且不属于 active/expired/disabled。

**副作用与审计**：只读；不返回 access_token、mailbox_key、storage_used/limit 或 allowed_domains。

<a id="maildrop_get_mailbox"></a>

### 05. maildrop_get_mailbox — 查询邮箱详情

按邮箱 ID 或完整地址读取配置、到期、统计和容量。可读取已停用邮箱。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| include_credentials | boolean | 否 | false | true时详情额外返回邮箱access_token和mailbox_key；不是MCP认证Token。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_mailbox",
  "arguments": {
    "mailbox": "demo123@example.com",
    "include_credentials": false
  }
}
```

**成功返回数据示例**：

```json
{
  "id": "mb-example-id",
  "address": "demo123@example.com",
  "created_at": 1793145600,
  "expires_at": 1793750400,
  "retention_days": 7,
  "is_active": true,
  "admin_disabled": false,
  "sender_whitelist": [],
  "whitelist_enabled": false,
  "created_by_ip": "192.0.2.10",
  "last_accessed": 1793145600,
  "updated_by_admin": null,
  "updated_at": null,
  "email_count": 1,
  "unread_count": 1,
  "last_email_time": 1793145660,
  "is_expired": false,
  "allowed_domains": [],
  "storage_used": 1024,
  "storage_limit": 3145728,
  "storage_used_mb": 0.0009765625,
  "storage_limit_mb": 3,
  "storage_percent": 0.03255208333333333
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| address | string | 完整邮箱地址；示例域名需替换为实际启用收件域名。 |
| created_at | integer | 对象创建时间，Unix秒。 |
| expires_at | integer | 邮箱到期时间，Unix秒。 |
| retention_days | integer | 邮箱设定保留天数，不保证每封邮件都保留同样天数。 |
| is_active | boolean | 邮箱是否处于启用状态，不替代过期判断。 |
| admin_disabled | boolean | 是否被管理员显式停用。 |
| sender_whitelist | array | 发件白名单字符串数组；具体匹配见3.3。 |
| whitelist_enabled | boolean | 当前发件白名单开关。 |
| created_by_ip | string | 创建来源IP，旧数据可null。 |
| last_accessed | integer | 最近访问/收信时间记录，Unix秒或null；MCP查询不更新。 |
| updated_by_admin | null | 最近更新管理员，旧数据可null。 |
| updated_at | null | 最近更新时间，Unix秒，旧数据可null。 |
| email_count | integer | 当前邮箱邮件条数。 |
| unread_count | integer | 当前邮箱未读条数。 |
| last_email_time | integer | 最近邮件Unix秒；无邮件时null。 |
| is_expired | boolean | 是否按当前时间已到期。 |
| allowed_domains | array | 允许的发件域名数组；不是收件域名。 |
| storage_used | integer | 已用容量字节。 |
| storage_limit | integer | 容量上限字节。 |
| storage_used_mb | number | 已用字节/1048576。 |
| storage_limit_mb | integer | 上限字节/1048576。 |
| storage_percent | number | 已用/上限×100，百分数。 |

include_credentials=true时额外字段access_token/mailbox_key均为敏感字符串或历史null，默认完全不出现。

**规则与边界**：默认删除凭据字段，而非用星号替代。include_credentials=true 会额外返回 access_token、mailbox_key，属于敏感输出。详情不含 created_source。容量 MB 使用1048576字节换算，storage_percent 是百分数。

**失败条件**：mailbox 为空或不存在；数据库读取失败。

**副作用与审计**：只读，不自动延长邮箱、不更新最后访问时间、不改变邮件已读。

<a id="maildrop_update_mailbox"></a>

### 06. maildrop_update_mailbox — 修改邮箱配置

部分更新：只改变传入的配置，至少给出一个可更新字段。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| retention_days | null/integer | 否 | 创建：MAILBOX_RETENTION_DAYS；更新：保持原值 | 整数1..MAX_RETENTION_DAYS；不接受0/-1。更新时从当前时间重算到期日。 |
| sender_whitelist | null/array，元素string | 否 | 创建：[]；更新：保持原值 | 字符串数组；完整发件地址或@域名。创建非空自动开启；更新整组替换，[]清空但不改开关。 |
| allowed_domains | null/array，元素string | 否 | 创建：[]；更新：保持原值 | 允许的发件域名数组；整组替换。[]不限制发件域名，独立于白名单开关；支持域名及其子域。 |
| whitelist_enabled | null/boolean | 否 | 保持原值 | JSON boolean，控制sender_whitelist匹配，不控制allowed_domains。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_update_mailbox",
  "arguments": {
    "mailbox": "demo123@example.com",
    "retention_days": 14,
    "sender_whitelist": [
      "@example.net"
    ],
    "allowed_domains": [
      "example.net"
    ],
    "whitelist_enabled": true
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：sender_whitelist 与 allowed_domains 是整组替换，[] 清空。修改 sender_whitelist 不自动切换开关，需显式设置 whitelist_enabled。retention_days 会将 expires_at 重算为当前时间加N天，并非在旧期限上追加；重新提交会再次改变到期时间。

**失败条件**：无更新字段、邮箱不存在、保留期越界、白名单条目或域名非法。

**副作用与审计**：配置修改、updated_at/updated_by_admin 和 UPDATE 审计同事务。

<a id="maildrop_set_mailbox_status"></a>

### 07. maildrop_set_mailbox_status — 启用或停用邮箱

显式设置管理状态，用于暂停收信或恢复管理停用。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| enabled | boolean | 是 | 无，必须显式给出 | JSON boolean，true启用/false禁用；不是字符串"true"/"false"。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_set_mailbox_status",
  "arguments": {
    "mailbox": "demo123@example.com",
    "enabled": false
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：enabled=false 写 is_active=false、admin_disabled=true；true 写 is_active=true、admin_disabled=false。启用不延长 expires_at，过期邮箱需要同时更新保留期。

**失败条件**：mailbox 为空或不存在，enabled 缺失或不是 JSON boolean。

**副作用与审计**：修改状态和更新元数据并记录 UPDATE 审计；保留邮件和凭据。

<a id="maildrop_delete_mailbox"></a>

### 08. maildrop_delete_mailbox — 停用或永久删除邮箱

默认执行管理停用。明确 permanent=true 时删除邮箱和该邮箱所有邮件。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| permanent | boolean | 否 | false | false仅停用邮箱；true永久删除邮箱及其全部邮件。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_delete_mailbox",
  "arguments": {
    "mailbox": "demo123@example.com",
    "permanent": false
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：默认停用与 set_mailbox_status(enabled=false) 的最终管理状态相同，但审计动作为 SOFT_DELETE。硬删除不能通过启用恢复。全局审计可查 HARD_DELETE，changes 内保留原 mailbox_id/address。

**失败条件**：mailbox 不存在或空；内部错误会回滚该邮箱事务。

**副作用与审计**：停用保留邮件与凭据；永久删除移除邮箱、邮件并写 HARD_DELETE。两种操作均逐邮箱原子提交。

<a id="maildrop_delete_mailboxes_batch"></a>

### 09. maildrop_delete_mailboxes_batch — 批量停用或永久删除邮箱

一次处理1–100个邮箱引用，全部共享同一个 permanent 选择。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailboxes | null/array，元素string | 是 | 无 | 字符串数组1–100项，元素为邮箱ID/完整地址；不自动去重。 |
| permanent | boolean | 否 | false | false仅停用邮箱；true永久删除邮箱及其全部邮件。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_delete_mailboxes_batch",
  "arguments": {
    "mailboxes": [
      "demo123@example.com",
      "demo456@example.com"
    ],
    "permanent": false
  }
}
```

**成功返回数据示例**：

```json
{
  "results": [
    {
      "mailbox": "demo123@example.com",
      "success": true
    },
    {
      "mailbox": "demo456@example.com",
      "success": false,
      "error": "Mailbox not found"
    }
  ]
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| results | array | 按输入顺序排列的逐项结果数组。 |
| results[].mailbox | string | 创建时为邮箱结果对象；批删时为回显输入的邮箱引用。 |
| results[].success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

每个结果项含mailbox/success；失败项额外error。处理中断时可能另有interrupted=true。

**规则与边界**：结果按输入顺序，mailbox 回显输入引用，没有 index。每项失败不阻止后续项；不去重，因此重复引用在永久删除时后续项可能报告不存在。取消可能导致部分完成，需查询现状。

**失败条件**：mailboxes为空或超过100整次失败；不存在邮箱在该项 error 返回。

**副作用与审计**：每邮箱一个事务，可部分成功。permanent=true 会永久删除成功项邮箱及邮件。

<a id="maildrop_reset_mailbox_token"></a>

### 10. maildrop_reset_mailbox_token — 轮换邮箱 API Token

废弃该邮箱原 API Token，生成32随机字节编码为64位十六进制字符串。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_reset_mailbox_token",
  "arguments": {
    "mailbox": "demo123@example.com"
  }
}
```

**成功返回数据示例**：

```json
{
  "mailbox_id": "mb-example-id",
  "access_token": "<NEW_64_HEX_MAILBOX_TOKEN>"
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| access_token | string | 邮箱API凭据；敏感值，不是MCP超管Token。 |

**规则与边界**：仅轮换 access_token；mailbox_key 和 MCP 超管 Token 不变。若调用结果丢失，可通过 get_mailbox(include_credentials=true)读取当前值，避免无必要再次轮换。

**失败条件**：邮箱不存在、生成随机数或数据库事务失败。

**副作用与审计**：新值与 MCP_RESET_TOKEN 审计同事务，审计只存 rotated=true。提交后旧 Token 失效。

<a id="maildrop_rotate_mailbox_key"></a>

### 11. maildrop_rotate_mailbox_key — 轮换邮箱网页访问 Key

重置该邮箱用于网页入口的访问 Key。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_rotate_mailbox_key",
  "arguments": {
    "mailbox": "demo123@example.com"
  }
}
```

**成功返回数据示例**：

```json
{
  "mailbox_id": "mb-example-id",
  "mailbox_key": "<NEW_10_CHAR_MAILBOX_KEY>"
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| mailbox_key | string | 邮箱网页访问Key；敏感值。 |

**规则与边界**：新 Key 为10位字母数字，包含大写、小写和数字。原 Key/链接失效；不轮换邮箱 API Token，已有 API Token 仍可用。不撤销已经被兑换或获取的 API 凭据。

**失败条件**：邮箱不存在、随机生成或数据库事务失败。

**副作用与审计**：更新 mailbox_key，记录 MCP_ROTATE_KEY；审计不保存新密钥。

<a id="maildrop_get_sender_whitelist"></a>

### 12. maildrop_get_sender_whitelist — 查看发件白名单

返回当前白名单规则和是否启用。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_sender_whitelist",
  "arguments": {
    "mailbox": "demo123@example.com"
  }
}
```

**成功返回数据示例**：

```json
{
  "senders": [
    "sender@example.net",
    "@example.org"
  ],
  "enabled": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| senders | array | 白名单规则字符串数组。 |
| enabled | boolean | 当前对象的启用状态，具体对象为白名单或域名。 |

**规则与边界**：此返回不包含 allowed_domains。白名单为空时即使 enabled=true，白名单这层也放行所有发件人；独立 allowed_domains 限制仍有效。

**失败条件**：邮箱不存在或 mailbox 为空。

**副作用与审计**：只读，不更新规则或开关。

<a id="maildrop_add_sender_whitelist"></a>

### 13. maildrop_add_sender_whitelist — 添加单条发件白名单

在保留已有规则的前提下添加一条完整发件地址或域名规则。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| sender | string | 是 | 无 | 白名单增删：1–320字节规则（完整地址、@域名或裸域名），禁止CR/LF；邮件过滤：发件人字面子串，最多320 UTF-8字节。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_add_sender_whitelist",
  "arguments": {
    "mailbox": "demo123@example.com",
    "sender": "@example.net"
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：去除首尾空白，限制1–320字节，禁止CR/LF。支持 sender@example.net、@example.net 和裸域名 example.net。查重按存储字符串精确比较，接收匹配忽略大小写。仅添加，不自动启用白名单。

**失败条件**：mailbox不存在、sender为空/过长/含换行、域名非法。

**副作用与审计**：规则与 MCP_WHITELIST 审计同事务；已有相同字符串也返回成功并记录审计。

<a id="maildrop_remove_sender_whitelist"></a>

### 14. maildrop_remove_sender_whitelist — 移除单条发件白名单

仅移除指定规则，保留其他规则。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| sender | string | 是 | 无 | 白名单增删：1–320字节规则（完整地址、@域名或裸域名），禁止CR/LF；邮件过滤：发件人字面子串，最多320 UTF-8字节。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_remove_sender_whitelist",
  "arguments": {
    "mailbox": "demo123@example.com",
    "sender": "@example.net"
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：移除按去首尾空白后的字符串精确比较，大小写或 @ 前缀不同可能不匹配；先 get_sender_whitelist 取存储值。不存在的规则返回成功。移除最后规则后白名单为空，该层将放行所有发件人。

**失败条件**：输入校验与添加工具相同；邮箱不存在。

**副作用与审计**：规则与 MCP_WHITELIST 审计同事务，不改变 whitelist_enabled 或 allowed_domains。

<a id="maildrop_set_whitelist_enabled"></a>

### 15. maildrop_set_whitelist_enabled — 设置发件白名单开关

显式启用或关闭邮箱白名单。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| enabled | boolean | 是 | 无，必须显式给出 | JSON boolean，true启用/false禁用；不是字符串"true"/"false"。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_set_whitelist_enabled",
  "arguments": {
    "mailbox": "demo123@example.com",
    "enabled": true
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：关闭只跳过 sender_whitelist 校验，不跳过 allowed_domains。启用但列表为空不是拒绝全部，当前实现会放行白名单这一层。

**失败条件**：邮箱不存在、缺失enabled或类型非boolean。

**副作用与审计**：更新开关、管理员更新时间，并写 UPDATE 审计。

<a id="maildrop_list_emails"></a>

### 16. maildrop_list_emails — 分页查看邮件摘要

分页列出指定邮箱摘要，不读取或返回正文内容。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| query | string | 否 | 空，不过滤 | 主题/发件人/正文任意字段命中字面子串，最多500 UTF-8字节。 |
| sender | string | 否 | 空，不过滤 | 发件人字面子串，最多320 UTF-8字节。 |
| subject | string | 否 | 空，不过滤 | 主题字面子串，最多500 UTF-8字节。 |
| after | integer | 否 | 0，不限制起点 | 非负Unix秒，包含边界timestamp>=after；不是毫秒或ISO日期。 |
| before | integer | 否 | 0，不限制终点 | 非负Unix秒，包含边界timestamp<=before；非0时必须>=after。 |
| unread_only | boolean | 否 | false | true仅查未读，false包含已读及未读。 |
| page | integer | 否 | 1；显式0也使用1 | 整数1–1000000，1起始；等待工具建议固定第1页。 |
| page_size | integer | 否 | 20；显式0也使用20 | 邮件工具范围1–100，邮箱列表1–200；页码超过结果范围返回空页。 |
| ignore_ids | null/array，元素string | 否 | [] | 排除的邮件ID数组，最多200项；不要求这些ID存在。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_list_emails",
  "arguments": {
    "mailbox": "demo123@example.com",
    "unread_only": true,
    "page": 1,
    "page_size": 20
  }
}
```

**成功返回数据示例**：

```json
{
  "mailbox_id": "mb-example-id",
  "emails": [
    {
      "id": "email-example-id",
      "from": "sender@example.net",
      "to": "demo123@example.com",
      "subject": "Verification code",
      "timestamp": 1793145660,
      "is_read": false,
      "content_type": "Text",
      "size_bytes": 1024
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| emails | array | 本页邮件摘要数组，不含正文；字段详见3.4。 |
| emails[].id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| emails[].from | string | 摘要发件人。 |
| emails[].to | string | 摘要收件人。 |
| emails[].subject | string | 摘要主题。 |
| emails[].timestamp | integer | 事件或邮件时间，Unix秒。 |
| emails[].is_read | boolean | 邮件已读状态。 |
| emails[].content_type | string | 摘要正文类型，例如Text/HTML。 |
| emails[].size_bytes | integer | 邮件存储记账字节数。 |
| total | integer | 应用过滤后所有匹配项数量，不是当前页长度。 |
| page | integer | 服务端实际使用的页码，1起始。 |
| page_size | integer | 服务端实际使用的每页大小。 |

**规则与边界**：支持全部邮件筛选字段。按 timestamp 降序、同秒按 rowid 降序；total 是所有匹配邮件数量，不是本页数量。翻页期间新邮件可能让偏移分页出现重复，应使用ID去重。

**失败条件**：邮箱不存在、时间范围错误、页码/页大小越界、筛选过长、ignore_ids超过200。

**副作用与审计**：只读，不标记已读；读取正文要调用 get_email。

<a id="maildrop_search_emails"></a>

### 17. maildrop_search_emails — 按条件检索邮件

检索发件人、主题、正文和时间范围，返回摘要。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| query | string | 否 | 空，不过滤 | 主题/发件人/正文任意字段命中字面子串，最多500 UTF-8字节。 |
| sender | string | 否 | 空，不过滤 | 发件人字面子串，最多320 UTF-8字节。 |
| subject | string | 否 | 空，不过滤 | 主题字面子串，最多500 UTF-8字节。 |
| after | integer | 否 | 0，不限制起点 | 非负Unix秒，包含边界timestamp>=after；不是毫秒或ISO日期。 |
| before | integer | 否 | 0，不限制终点 | 非负Unix秒，包含边界timestamp<=before；非0时必须>=after。 |
| unread_only | boolean | 否 | false | true仅查未读，false包含已读及未读。 |
| page | integer | 否 | 1；显式0也使用1 | 整数1–1000000，1起始；等待工具建议固定第1页。 |
| page_size | integer | 否 | 20；显式0也使用20 | 邮件工具范围1–100，邮箱列表1–200；页码超过结果范围返回空页。 |
| ignore_ids | null/array，元素string | 否 | [] | 排除的邮件ID数组，最多200项；不要求这些ID存在。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_search_emails",
  "arguments": {
    "mailbox": "demo123@example.com",
    "query": "123456",
    "sender": "example.net",
    "subject": "Verification",
    "after": 1793145600,
    "before": 1793232000,
    "page": 1,
    "page_size": 20
  }
}
```

**成功返回数据示例**：

```json
{
  "mailbox_id": "mb-example-id",
  "emails": [],
  "total": 0,
  "page": 1,
  "page_size": 20
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| emails | array | 本页邮件摘要数组，不含正文；字段详见3.4。 |
| total | integer | 应用过滤后所有匹配项数量，不是当前页长度。 |
| page | integer | 服务端实际使用的页码，1起始。 |
| page_size | integer | 服务端实际使用的每页大小。 |

**规则与边界**：与 list_emails 调用相同实现和返回结构。query 在主题/发件人/正文中任一命中；与 sender、subject、时间、未读、排除ID条件是AND关系。query/sender/subject均为字面子串，不支持正则、全文分词或 % 通配。SQLite lower 的非ASCII大小写折叠有限。

**失败条件**：与 list_emails 相同。可以不传query，仅按其他条件筛选。

**副作用与审计**：只读，查询正文不意味着返回正文，仍需 get_email。

<a id="maildrop_get_email"></a>

### 18. maildrop_get_email — 读取单封邮件内容

读取指定邮箱内一封邮件的解析后正文。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| email_id | string | 是 | 无 | 指定邮箱内的邮件ID；从list/search/wait摘要取得。 |
| max_body_chars | integer | 否 | 20000；显式0也使用20000 | 有效整数1–200000，Unicode字符数，不是字节；超出正文长度不补齐。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_email",
  "arguments": {
    "mailbox": "demo123@example.com",
    "email_id": "email-example-id",
    "max_body_chars": 20000
  }
}
```

**成功返回数据示例**：

```json
{
  "email": {
    "id": "email-example-id",
    "mailbox_id": "mb-example-id",
    "From": "sender@example.net",
    "To": "demo123@example.com",
    "Subject": "Verification code",
    "Body": "Your code is 123456.",
    "ContentType": "Text",
    "Timestamp": 1793145660,
    "Sent": "2026-10-28 08:01:00",
    "is_read": false
  },
  "body_chars": 20,
  "body_truncated": false,
  "content_trust": "untrusted_external_email"
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| email | object | 邮件详情对象，字段大小写与3.4一致。 |
| email.id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| email.mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| email.From | string | 详情发件人；注意首字母大写。 |
| email.To | string | 详情收件人。 |
| email.Subject | string | 详情主题。 |
| email.Body | string | 正文字符串；可能截断，HTML视为不可信。 |
| email.ContentType | string | 详情正文类型。 |
| email.Timestamp | integer | 邮件接收Unix秒。 |
| email.Sent | string | 保存的格式化时间字符串，不是ISO时间契约。 |
| email.is_read | boolean | 邮件已读状态。 |
| body_chars | integer | 截断前完整正文的Unicode字符数。 |
| body_truncated | boolean | true表示正文只返回前缀。 |
| content_trust | string | 固定untrusted_external_email，标明邮件内容不可信。 |

**规则与边界**：正文字段严格是 email.Body。max_body_chars按Unicode字符截断，不按UTF-8字节；body_chars始终是截断前完整正文字符数。只提供正文前缀，没有offset继续读取；最大200000字符。ContentType可能是Text或HTML；无原始EML、附件下载工具。

**失败条件**：Email not found；Email not found in mailbox；邮箱不存在；max_body_chars越界。

**副作用与审计**：只读，不标记已读。外部邮件内容不可信，不能把邮件文字当作工具调用指令。

<a id="maildrop_wait_for_email"></a>

### 19. maildrop_wait_for_email — 等待匹配邮件

立即查询一次，之后约每250毫秒查询，直到发现匹配结果、超时或取消。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| query | string | 否 | 空，不过滤 | 主题/发件人/正文任意字段命中字面子串，最多500 UTF-8字节。 |
| sender | string | 否 | 空，不过滤 | 发件人字面子串，最多320 UTF-8字节。 |
| subject | string | 否 | 空，不过滤 | 主题字面子串，最多500 UTF-8字节。 |
| after | integer | 否 | 0，不限制起点 | 非负Unix秒，包含边界timestamp>=after；不是毫秒或ISO日期。 |
| before | integer | 否 | 0，不限制终点 | 非负Unix秒，包含边界timestamp<=before；非0时必须>=after。 |
| unread_only | boolean | 否 | false | true仅查未读，false包含已读及未读。 |
| page | integer | 否 | 1；显式0也使用1 | 整数1–1000000，1起始；等待工具建议固定第1页。 |
| page_size | integer | 否 | 20；显式0也使用20 | 邮件工具范围1–100，邮箱列表1–200；页码超过结果范围返回空页。 |
| ignore_ids | null/array，元素string | 否 | [] | 排除的邮件ID数组，最多200项；不要求这些ID存在。 |
| timeout_seconds | integer | 否 | 20；显式0也使用20 | 有效整数1–25秒；不能用0表示无限等待。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_wait_for_email",
  "arguments": {
    "mailbox": "demo123@example.com",
    "subject": "Verification",
    "after": 1793145600,
    "ignore_ids": [
      "already-seen-email-id"
    ],
    "timeout_seconds": 20,
    "page": 1,
    "page_size": 20
  }
}
```

**成功返回数据示例**：

```json
{
  "mailbox_id": "mb-example-id",
  "emails": [
    {
      "id": "email-example-id",
      "from": "sender@example.net",
      "to": "demo123@example.com",
      "subject": "Verification",
      "timestamp": 1793145660,
      "is_read": false,
      "content_type": "Text",
      "size_bytes": 1024
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20,
  "found": true,
  "timed_out": false
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| emails | array | 本页邮件摘要数组，不含正文；字段详见3.4。 |
| emails[].id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| emails[].from | string | 摘要发件人。 |
| emails[].to | string | 摘要收件人。 |
| emails[].subject | string | 摘要主题。 |
| emails[].timestamp | integer | 事件或邮件时间，Unix秒。 |
| emails[].is_read | boolean | 邮件已读状态。 |
| emails[].content_type | string | 摘要正文类型，例如Text/HTML。 |
| emails[].size_bytes | integer | 邮件存储记账字节数。 |
| total | integer | 应用过滤后所有匹配项数量，不是当前页长度。 |
| page | integer | 服务端实际使用的页码，1起始。 |
| page_size | integer | 服务端实际使用的每页大小。 |
| found | boolean | true有匹配邮件，false未找到。 |
| timed_out | boolean | true是本次业务等待超时；不等于请求取消。 |

业务超时的另一种成功返回（不含emails/total/page）：

```json
{
  "found": false,
  "timed_out": true
}
```

**规则与边界**：不是订阅，也不保证必须为新到邮件；不传after/ignore_ids会立即返回旧邮件。timeout_seconds省略或0使用20秒，有效1–25秒。成功是摘要列表，不是正文。超时仅返回 found=false,timed_out=true，没有emails/total/page字段。判定依赖total>0，page过大时可能found=true但emails=[]，等待时应使用page=1。

**失败条件**：筛选/邮箱参数错误立即失败；请求取消是取消错误，不当作普通等待超时。客户端超时应长于选择的等待时长。

**副作用与审计**：只读、可取消；需要长等待时客户端分次轮询，并用ignore_ids去重。

<a id="maildrop_set_email_read"></a>

### 20. maildrop_set_email_read — 设置单封邮件已读状态

将单封邮件设置为已读或未读。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| email_id | string | 是 | 无 | 指定邮箱内的邮件ID；从list/search/wait摘要取得。 |
| is_read | boolean | 是 | 无，必须显式给出 | JSON boolean，true已读、false未读。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_set_email_read",
  "arguments": {
    "mailbox": "demo123@example.com",
    "email_id": "email-example-id",
    "is_read": true
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：必须传JSON boolean，true为已读、false为未读。邮件必须属于mailbox。相同状态重复设置仍成功并记录审计。

**失败条件**：邮件不存在/归属其他邮箱统一 Email not found in mailbox；缺失is_read或类型错误。

**副作用与审计**：更新与 MCP_SET_READ 审计在同一事务，不改变邮件内容。

<a id="maildrop_mark_all_read"></a>

### 21. maildrop_mark_all_read — 将邮箱全部邮件标记已读

将指定邮箱中 is_read=0 的邮件批量改为已读。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_mark_all_read",
  "arguments": {
    "mailbox": "demo123@example.com"
  }
}
```

**成功返回数据示例**：

```json
{
  "updated": 3
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| updated | integer | 本次从未读变为已读的邮件条数。 |

**规则与边界**：updated是本次实际从未读改为已读的条数；无未读时为0。无时间或搜索过滤条件，只作用整个指定邮箱。

**失败条件**：邮箱不存在或数据库事务失败。

**副作用与审计**：更新和 MCP_MARK_ALL_READ 审计同事务；重复执行的业务状态相同但updated可能变为0。

<a id="maildrop_delete_emails"></a>

### 22. maildrop_delete_emails — 永久批量删除邮件

删除指定邮箱的1–200个邮件ID，批量整体校验后执行。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 是 | 无 | 必需引用时不能为空；邮箱ID或完整地址，建议使用创建结果id。读取引用不自动去首尾空白或规范化大小写。 |
| email_ids | null/array，元素string | 是 | 无 | 邮件ID数组1–200项，去重后删除；任一缺失或非该邮箱邮件整批回滚。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_delete_emails",
  "arguments": {
    "mailbox": "demo123@example.com",
    "email_ids": [
      "email-example-id"
    ]
  }
}
```

**成功返回数据示例**：

```json
{
  "deleted": 1,
  "bytes_freed": 1024
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| deleted | integer | 邮件删除工具：唯一邮件条数；域名删除工具：已删域名字符串数组。 |
| bytes_freed | integer | 删除邮件的容量记账字节总和，不表示数据库文件立即变小。 |

**规则与边界**：email_ids重复值去重，deleted为唯一邮件数，bytes_freed为存储计数字节总和。任何ID不存在或归属其他邮箱整批失败，已删除的ID再次提交也报错。

**失败条件**：email_ids为空或>200；Email not found in mailbox: <id>；事务错误。

**副作用与审计**：删除、扣减storage_used（下限0）、MCP_DELETE_EMAILS审计同事务；不可撤销。bytes_freed是容量记账，不表示SQLite物理文件立即缩小。

<a id="maildrop_health"></a>

### 23. maildrop_health — 检查MCP数据库连接

在认证后的MCP连接中检查数据库可连接。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_health",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "status": "ok",
  "time": "2026-10-28T09:00:00Z"
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| status | string | 成功健康结果为ok；仍需先检查工具isError。 |
| time | string | UTC RFC3339格式时间。 |

**规则与边界**：time为UTC RFC3339字符串。此工具不验证外部MX、SMTP端到端、磁盘完整性或邮件服务商送达。

**失败条件**：认证或数据库连接失败；失败不应仅凭输出status判断，要先检查MCP isError。

**副作用与审计**：只读；无需认证的进程探针另有GET /healthz。

<a id="maildrop_server_info"></a>

### 24. maildrop_server_info — 查看MCP能力与限额

报告此MCP服务的固定能力和限制。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_server_info",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "name": "maildrop",
  "mcp_version": "1.0.0",
  "role": "superadmin",
  "transport": "streamable-http",
  "endpoint": "/mcp",
  "receive_only": true,
  "tool_count": 35,
  "limits": {
    "mailbox_batch": 100,
    "domain_batch": 200,
    "email_delete_batch": 200,
    "email_page_size": 100,
    "wait_seconds": 25
  },
  "security_config_persistent": false
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| name | string | MCP实现标识maildrop。 |
| mcp_version | string | 应用暴露版本1.0.0，不是协议日期。 |
| role | string | 固定superadmin。 |
| transport | string | 固定streamable-http。 |
| endpoint | string | MCP路径/mcp。 |
| receive_only | boolean | true表示只收信。 |
| tool_count | integer | 当前工具数35。 |
| limits | object | 工具批量/分页/等待限额。 |
| limits.mailbox_batch | integer | 邮箱批量输入项上限。 |
| limits.domain_batch | integer | 域名批量输入项上限。 |
| limits.email_delete_batch | integer | 邮件批量删除输入项上限。 |
| limits.email_page_size | integer | 邮件页大小上限。 |
| limits.wait_seconds | integer | 单次等待秒数上限。 |
| security_config_persistent | boolean | false：运行期安全设置不持久化。 |

**规则与边界**：mcp_version=1.0.0是应用暴露的版本标识，不是initialize协商的协议日期。协议版本以initialize.result.protocolVersion为准。该工具没有配置秘密。

**失败条件**：通常仅认证/协议层失败。

**副作用与审计**：只读；不做数据库或SMTP健康探测。

<a id="maildrop_list_domains"></a>

### 25. maildrop_list_domains — 列出收件域名

返回收件域名数组，包括启用和禁用域名。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_list_domains",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
[
  {
    "domain": "example.com",
    "enabled": true,
    "created_at": 1793145600,
    "updated_at": 1793145600
  }
]
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| [].domain | string | 规范化收件域名；失败项有时保留原始输入。 |
| [].enabled | boolean | 当前对象的启用状态，具体对象为白名单或域名。 |
| [].created_at | integer | 对象创建时间，Unix秒。 |
| [].updated_at | integer | 最近更新时间，Unix秒，旧数据可null。 |

**规则与边界**：按created_at、同秒插入顺序升序排列。生成随机地址默认选启用项中的第一个。返回配置状态不代表DNS/MX已经正确解析。

**失败条件**：数据库读取失败。

**副作用与审计**：只读，不修改DNS。

<a id="maildrop_add_domains"></a>

### 26. maildrop_add_domains — 批量添加收件域名

一次添加1–200个收件域名，新项默认启用。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| domains | null/array，元素string | 是 | 无 | 字符串数组1–200项，收件域名；只改邮局配置，不配置DNS。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_add_domains",
  "arguments": {
    "domains": [
      "new.example.com",
      "new.example.com",
      "bad domain"
    ]
  }
}
```

**成功返回数据示例**：

```json
{
  "added": [
    {
      "domain": "new.example.com",
      "enabled": true,
      "created_at": 1793145600,
      "updated_at": 1793145600
    }
  ],
  "skipped": [
    {
      "domain": "new.example.com",
      "reason": "重复输入"
    },
    {
      "domain": "bad domain",
      "reason": "域名格式不正确"
    }
  ]
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| added | array | 本次新添加域名对象数组。 |
| added[].domain | string | 规范化收件域名；失败项有时保留原始输入。 |
| added[].enabled | boolean | 当前对象的启用状态，具体对象为白名单或域名。 |
| added[].created_at | integer | 对象创建时间，Unix秒。 |
| added[].updated_at | integer | 最近更新时间，Unix秒，旧数据可null。 |
| skipped | array | 跳过项数组，每项domain/reason。 |
| skipped[].domain | string | 规范化收件域名；失败项有时保留原始输入。 |
| skipped[].reason | string | 该域名未处理的原因。 |

**规则与边界**：去空白、转小写、去末尾点、IDN转Punycode。格式非法、同批重复、数据库已有均在skipped中报告；已有禁用项不会被此工具重新启用。添加仅改邮局配置，DNS仍需另行设置。

**失败条件**：数量不合规整次失败；数据库错误回滚域名事务；审计失败时域名可能已提交。

**副作用与审计**：有效新项在一个域名事务内提交，随后独立审计 BATCH_ADD_DOMAINS；业务上允许有效项成功、无效项跳过。

<a id="maildrop_set_domain_enabled"></a>

### 27. maildrop_set_domain_enabled — 启用或禁用收件域名

控制一个已配置域名是否参与新收信/地址生成。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| domain | string | 是 | 无，必须给出 | 现存收件域名，无协议/路径。 |
| enabled | boolean | 是 | 无，必须显式给出 | JSON boolean，true启用/false禁用；不是字符串"true"/"false"。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_set_domain_enabled",
  "arguments": {
    "domain": "example.com",
    "enabled": true
  }
}
```

**成功返回数据示例**：

```json
{
  "domain": "example.com",
  "enabled": true,
  "created_at": 1793145600,
  "updated_at": 1793145660
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| domain | string | 规范化收件域名；失败项有时保留原始输入。 |
| enabled | boolean | 当前对象的启用状态，具体对象为白名单或域名。 |
| created_at | integer | 对象创建时间，Unix秒。 |
| updated_at | integer | 最近更新时间，Unix秒，旧数据可null。 |

**规则与边界**：至少保留一个启用域名。重复设置状态仍更新时间并可写审计。不删除域名下邮箱或已有邮件；禁用后不能在该域名创建新MCP邮箱，也不能正常接收该域名新邮件。

**失败条件**：域名不存在/非法、最后启用域名不能禁用、enabled缺失或类型错误。

**副作用与审计**：域名事务先提交，再记录UPDATE_DOMAIN审计；审计出错也可能已经生效，先list_domains核对。

<a id="maildrop_delete_domains"></a>

### 28. maildrop_delete_domains — 批量删除收件域名

一次从邮局域名配置中删除1–200项。

**行为标记**：会改变状态；SDK幂等提示=false（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| domains | null/array，元素string | 是 | 无 | 字符串数组1–200项，收件域名；只改邮局配置，不配置DNS。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_delete_domains",
  "arguments": {
    "domains": [
      "old.example.com",
      "missing.example.com"
    ]
  }
}
```

**成功返回数据示例**：

```json
{
  "deleted": [
    "old.example.com"
  ],
  "failed": [
    {
      "domain": "missing.example.com",
      "reason": "域名不存在"
    }
  ]
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| deleted | array | 邮件删除工具：唯一邮件条数；域名删除工具：已删域名字符串数组。 |
| failed | array | 失败项数组，每项domain/reason。 |
| failed[].domain | string | 规范化收件域名；失败项有时保留原始输入。 |
| failed[].reason | string | 该域名未处理的原因。 |

**规则与边界**：去重处理，重复项不额外报告。缺失/非法项报告failed，其余候选统一校验：若会删除全部启用域名则整个域名删除事务拒绝。不会连带删除现存邮箱或邮件，也不修改注册商DNS。

**失败条件**：数量错误；At least one enabled receiving domain must remain；数据库错误。

**副作用与审计**：有效删除在同一域名事务，随后独立BATCH_DELETE_DOMAINS审计。缺失项允许与成功项同时返回；审计失败后先查状态再重试。

<a id="maildrop_get_stats"></a>

### 29. maildrop_get_stats — 查看全局业务统计

查询数据库当前的邮箱与邮件数量。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_stats",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "total_mailboxes": 134,
  "active_mailboxes": 130,
  "expired_mailboxes": 2,
  "disabled_mailboxes": 3,
  "total_emails": 14,
  "unread_emails": 5
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| total_mailboxes | integer | 现存邮箱总数。 |
| active_mailboxes | integer | is_active=1且未到期的邮箱数。 |
| expired_mailboxes | integer | expires_at已到期的邮箱数，可与停用数量重叠。 |
| disabled_mailboxes | integer | is_active=0的邮箱数，可与过期数量重叠。 |
| total_emails | integer | 现存邮件条数，不是历史累计收信量。 |
| unread_emails | integer | 现存未读邮件条数。 |

**规则与边界**：active_mailboxes=is_active=1且未到期；expired_mailboxes按expires_at到期判断；disabled_mailboxes=is_active=0。过期和停用可重叠，所以三者之和不保证等于total_mailboxes。邮件数不是累计收信量，已清理/删除的不计。

**失败条件**：数据库读取失败。

**副作用与审计**：只读；示例数值是构造值，不是实时生产监控。

<a id="maildrop_get_source_stats"></a>

### 30. maildrop_get_source_stats — 查看邮箱创建来源统计

按mailboxes.created_source分组返回数量映射。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_source_stats",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "admin": 20,
  "web": 10,
  "unknown": 2
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| 动态来源键 | integer | 返回来源名称到现存邮箱数量的映射，例如admin/web/unknown；键不是固定枚举。 |

**规则与边界**：键是动态来源值，不是固定枚举；空/NULL归unknown。MCP创建复用admin服务，来源是admin，不会单独出现mcp-superadmin键；审计用户另为mcp-superadmin。

**失败条件**：数据库读取失败。

**副作用与审计**：只读；统计现存邮箱，不是历史累计创建次数。

<a id="maildrop_list_audit_logs"></a>

### 31. maildrop_list_audit_logs — 查询审计日志

返回最近审计记录，可按一个现存邮箱筛选。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| mailbox | string | 否 | 空/省略：全局审计 | 给出时为现存邮箱ID/完整地址；已永久删除的邮箱需查询全局日志。 |
| limit | integer | 否 | 100；显式0也使用100 | 整数1–1000，最近记录条数，不支持分页游标。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_list_audit_logs",
  "arguments": {
    "mailbox": "demo123@example.com",
    "limit": 100
  }
}
```

**成功返回数据示例**：

```json
[
  {
    "id": "audit-example-id",
    "timestamp": 1793145660,
    "action": "MCP_SET_READ",
    "mailbox_id": "mb-example-id",
    "admin_user": "mcp-superadmin",
    "changes": {
      "email_id": "email-example-id",
      "is_read": true
    },
    "ip_address": "192.0.2.10"
  }
]
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| [].id | string | 此对象的ID；邮箱/邮件/审计ID的含义由所在对象决定。 |
| [].timestamp | integer | 事件或邮件时间，Unix秒。 |
| [].action | string | 审计动作代码。 |
| [].mailbox_id | string | 所属邮箱ID；硬删除审计可能为null，原ID在changes。 |
| [].admin_user | string | 操作者标识，MCP变更为mcp-superadmin。 |
| [].changes | object | 该动作的变更明细对象，结构随action变化且已脱敏。 |
| [].ip_address | string | 按可信反代层数解析的客户端IP。 |

**规则与边界**：按timestamp降序，仅支持limit，无offset/cursor/时间筛选。mailbox省略或空为全局；指定时先解析现存邮箱，已永久删除的邮箱无法这样查询。硬删除日志mailbox_id=null，原ID在changes中，需全局读取后识别。递归去除包含token/password/key/credential的字段名。

**失败条件**：limit不在1–1000；指定mailbox不存在；历史changes JSON损坏或数据库读取失败。

**副作用与审计**：只读；MCP输出脱敏不等于数据库中历史原始审计已被清除。

<a id="maildrop_list_blocked_ips"></a>

### 32. maildrop_list_blocked_ips — 查看当前IP封禁

返回IP到剩余封禁秒数的映射。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_list_blocked_ips",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "192.0.2.10": 120,
  "2001:db8::10": 45
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| 动态IP键 | integer（秒） | IP到剩余封禁秒数的映射；无封禁时{}。 |

**规则与边界**：无封禁时{}。仅包含未过期封禁，剩余秒向下取整，临近到期可为0。状态属于内存，服务重启会清除。

**失败条件**：通常仅认证或协议层失败。

**副作用与审计**：只读；当前调用来源若已被封禁，连此工具也无法调用。

<a id="maildrop_unblock_ip"></a>

### 33. maildrop_unblock_ip — 解封单个IP

删除该IP的封禁和失败尝试历史。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| ip | string | 是 | 无 | 单个IPv4/IPv6字面地址，不接受CIDR、域名、端口。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_unblock_ip",
  "arguments": {
    "ip": "192.0.2.10"
  }
}
```

**成功返回数据示例**：

```json
{
  "success": true
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| success | boolean | 单项或本次业务操作是否成功；true不代表其他批量项也成功。 |

**规则与边界**：接受单个IPv4或IPv6字面地址，不接受CIDR、端口、域名。传不存在封禁的有效IP也成功；匹配使用存储字符串，最好直接复制list_blocked_ips的键。若自己IP已封禁，需从未封禁来源执行或等待到期。

**失败条件**：Invalid IP address；审计数据库失败。

**副作用与审计**：先写MCP_UNBLOCK_IP审计，成功后修改内存；不修改防火墙或系统IP规则。

<a id="maildrop_get_security_config"></a>

### 34. maildrop_get_security_config — 查询认证失败封禁阈值

读取MCP与原后台共用的认证失败阈值。

**行为标记**：只读；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

无参数，arguments传{}。

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_get_security_config",
  "arguments": {}
}
```

**成功返回数据示例**：

```json
{
  "config": {
    "block_duration": 300,
    "max_attempts": 3,
    "attempt_window": 60
  },
  "persistent": false
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| config | object | 当前封禁阈值对象。 |
| config.block_duration | integer | 触发后封禁时长，秒。 |
| config.max_attempts | integer | 失败窗口内触发封禁的失败次数。 |
| config.attempt_window | integer | 失败次数统计窗口，秒。 |
| persistent | boolean | 固定false，重启恢复环境文件。 |

**规则与边界**：block_duration和attempt_window单位秒；max_attempts为窗口内触发封禁的失败次数。初始值来自环境变量，生产实际值用本工具读取，不应把示例当现值。

**失败条件**：通常仅认证或协议层失败。

**副作用与审计**：只读，persistent=false表明MCP运行期更改不写环境文件。

<a id="maildrop_update_security_config"></a>

### 35. maildrop_update_security_config — 修改运行期封禁阈值

至少传一个非null字段，只修改给出的设置。

**行为标记**：会改变状态；SDK幂等提示=true（不包含重复审计等附加影响）。

**输入参数**：

| 参数 | JSON类型 | 必填 | 默认/省略语义 | 限制与说明 |
| --- | --- | --- | --- | --- |
| block_duration | null/integer | 否 | 保持原值 | 整数60–86400，单位秒；仅影响后续封禁。 |
| max_attempts | null/integer | 否 | 保持原值 | 整数1–10，窗口内失败次数阈值。 |
| attempt_window | null/integer | 否 | 保持原值 | 整数30–600，单位秒；失败尝试统计窗口。 |

**调用参数（tools/call的params）**：

```json
{
  "name": "maildrop_update_security_config",
  "arguments": {
    "block_duration": 600,
    "max_attempts": 5,
    "attempt_window": 120
  }
}
```

**成功返回数据示例**：

```json
{
  "config": {
    "block_duration": 600,
    "max_attempts": 5,
    "attempt_window": 120
  },
  "persistent": false
}
```

**返回字段**：

| 字段 | 示例类型 | 说明 |
| --- | --- | --- |
| config | object | 当前封禁阈值对象。 |
| config.block_duration | integer | 触发后封禁时长，秒。 |
| config.max_attempts | integer | 失败窗口内触发封禁的失败次数。 |
| config.attempt_window | integer | 失败次数统计窗口，秒。 |
| persistent | boolean | 固定false，重启恢复环境文件。 |

**规则与边界**：block_duration范围60–86400秒，max_attempts范围1–10，attempt_window范围30–600秒。校验全部字段后更新。影响新失败尝试，不调整已有封禁截止时间；不清除失败历史。重启恢复环境文件初始值。

**失败条件**：At least one setting is required；任意字段越界或类型错误；审计写入失败。

**副作用与审计**：先记录MCP_SECURITY_CONFIG审计，再修改共享内存配置；不修改.env、不重启服务。

## 5. 可复用工作流

### 5.1 创建邮箱并等待验证码

1. list_domains取enabled=true的域名；不用猜网站域名。
2. create_mailbox传domain和retention_days。保存返回id和address；真正的完整邮箱凭据仅在受控存储保存。
3. 在触发外部业务发送验证码前记录Unix秒T，并记录/去重已有邮件ID。
4. 在外部业务触发发信，再wait_for_email传mailbox=id、after=T、subject/sender及ignore_ids，page=1。
5. 若found=false，按次数/总时长上限继续等待；若found=true但emails空，先检查page。after包含同秒旧信，用ignore_ids排除已处理ID。
6. 对匹配摘要调用get_email，读取email.Body，从Text/HTML正文识别需要的数据。先检查body_truncated；若为true可提高max_body_chars至上限，超出上限没有继续分页读取入口。
7. 处理成功调用set_email_read；重复执行工作流可用unread_only+ignore_ids，但已读标记本身不保证“新信”。
8. 测试邮箱无需保留时明确调用delete_mailbox(permanent=true)；业务邮箱不自动清理。

### 5.2 批量准备邮箱并保留部分成功

将所有预期地址写入items；create_mailboxes_batch后逐项记录index/success。只对失败且确认尚不存在的地址重试。若发生传输错误没有结果，按预定地址用get_mailbox核对。随机创建返回丢失时无法仅凭请求恢复随机地址，宜在需可靠重试的批量任务中预先指定地址。

### 5.3 调整发件限制

先get_mailbox和get_sender_whitelist读取现状；用update_mailbox整组配置sender_whitelist、allowed_domains和whitelist_enabled，或用add/remove修改单条后set_whitelist_enabled。再读取确认。关闭白名单但仍收不到信时检查allowed_domains、域名启用、邮箱有效期、容量和邮件大小。

### 5.4 轮换凭据

重置邮箱API Token用reset_mailbox_token，旧邮箱Token失效；重置网页Key用rotate_mailbox_key，旧Key失效但不撤销邮箱API Token。MCP超管Token轮换需要改服务器环境文件、更新客户端并重启，不通过这35个工具执行。若希望全部旧邮箱访问方式失效，需要分别处理网页Key和邮箱Token。

### 5.5 从统计和审计排查问题

先health和get_stats；再get_mailbox核对状态、期限、容量、发件限制。用list_audit_logs取最近操作；删除邮箱后的审计需全局查询，硬删除事件的原mailbox_id位于changes。list_blocked_ips和get_security_config只涵盖应用认证封禁，不是网络防火墙。新增收件域名须另外配置DNS MX/A和确认SMTP25可达。

## 6. 失败处理对照

| 层级/表现 | 常见原因 | 处理 |
| --- | --- | --- |
| HTTP401 | 未带正确独立MCP Bearer Token | 用私密客户端配置，不用后台密码/邮箱Token |
| HTTP403 | Host不在白名单或Origin校验失败 | 使用正式域名，检查反代Host；浏览器跨站调用不被允许 |
| HTTP413 | 超过请求体限额或反代限制 | 拆小批次；不把正文/大文件塞入工具输入 |
| HTTP429 | 认证失败封禁或32并发已满 | 查看响应文字区分，等待或降低并发，别持续错误认证 |
| HTTP503 MCP is disabled | Token空、短于32字节或等于网页密码 | 修改环境独立Token并重启 |
| JSON-RPC error | JSON/方法/协议或Schema不合法 | 检查工具名、必填项、JSON类型、未知字段 |
| result.isError=true | 业务校验失败或服务器错误 | 读取content中的错误文本，不按成功数据解析 |
| results[].success=false | 邮箱批量单项失败 | 保留成功项，仅核对失败项再重试 |
| skipped/failed非空 | 域名批量中重复/已存在/缺失/非法项 | 按每项reason处理，不能只看HTTP状态 |
| found=false,timed_out=true | 本次等待没有匹配邮件 | 属于正常业务结果；检查过滤条件并有限次继续等待 |
| Operation failed; check server logs... | 未公开的内部错误，可能含审计失败 | 管理员查journalctl；变更类先查询当前状态再重试 |

## 7. 本版已知边界与文档验证

此版本支持35个工具和超管身份；不提供发信、附件/EML、邮件转发配置、DNS服务商操作、子管理员MCP、邮箱身份MCP、专门验证码解析或持久化安全阈值工具。搜索按单个邮箱；无跨邮箱正文全文搜索、无审计时间分页、无超过200000字符的正文分段接口。

本次验证：线上initialize与tools/list共35项；全部工具请求示例与线上inputSchema逐项校验；业务范围、返回字段与当前实现回读核对；JSON示例语法和字段覆盖检查。未为写文档再次在生产执行删除、轮换或配置修改。部署阶段的端到端收信测试记录见交付文档。

实现索引：

- internal/mcpapi/server.go：认证、Host/Origin、传输、错误与脱敏。
- internal/mcpapi/mailboxes.go：邮箱与发件白名单工具。
- internal/mcpapi/emails.go：邮件列表/搜索/正文/等待/已读/批删。
- internal/mcpapi/system.go：域名、统计、审计、封禁与配置。
- internal/admin/mailboxes.go、domains.go、operations.go：复用业务规则、事务及审计。
- internal/inbox/service.go：收件与发件规则匹配。

接入总览见 [mcp.md](mcp.md)，运维见 [mcp-deployment.md](mcp-deployment.md)。
