# Maildrop 收发运行说明

应用继续以单个 Go 进程提供 Web、SMTP 收件、持久发件队列和清理任务。

设计参考：[Mailpit](https://github.com/axllent/mailpit) 的邮件内容与附件展示、[mox](https://github.com/mjl-/mox) 和 [maddy](https://github.com/foxcpp/maddy) 的 SMTP、队列和验证处理。协议实现使用 Go 标准库、`go-smtp`、`go-message`、`go-msgauth` 与 `blitiri.com.ar/go/spf`。

## 发件规则

只有主管理员可通过后台邮箱编辑窗口、`PUT /api/admin/mailboxes/{id}/sending-policy` 或持有独立超管 Token 的 MCP `maildrop_set_sending_policy` 开通、撤销发件与修改额度。子管理员和普通邮箱令牌不能修改策略，新邮箱默认未授权。

默认每邮箱每天 100 个收件人、滚动 60 秒内 10 个收件人。每日按北京时间 00:00 重置。To、Cc、Bcc 都计入，同一封中重复的相同地址只投递和计数一次。请求成功入队时原子预扣；失败、退信、删除记录或重试不退额度、不重复扣额。

每封最多 100 个收件人、10 个附件，解码后合计 10 MiB；正文合计最多 1 MiB，MIME 编码后整封最多 20 MiB。独立发件存档 100 MiB/邮箱，包含发送原文及退信原文，保留 30 天。未完成投递的记录不能手动删除。清理按小时执行，即使全局关闭发件也继续清理。幂等请求墓碑保留至邮箱删除，不含正文或附件；存档过期后再次提交旧 key 返回 410，不能重新入队。

发件人固定为当前邮箱，信封退信地址使用同域随机地址。Bcc 只在本邮箱的私有发送记录中显示，不写入 SMTP 消息头或发件原文。业务回复接受 `reply_email_id`，后端验证原邮件归属；`In-Reply-To`/`References` 来自已保存的原文，网页默认回复到原邮件的 Reply-To 或 From。

状态含义：

| 状态 | 含义 |
| --- | --- |
| queued / retry / sending | 排队、等待重试、正在投递 |
| accepted | 对方 SMTP 服务器在 DATA 后确认接受；不代表已进收件箱或最终送达 |
| failed / partial_failed | 永久错误、超过 24 小时或权限撤销；可能只有部分收件人失败 |
| bounced | 已接收并关联一份声明投递失败的 DSN；报告来源不保证可信 |

后台按收件人持久记录状态，已确认接受者不会因为其他收件人的错误重新发送。临时错误按 1、5、15、30、60 分钟退避，最多尝试到首次入队后的 24 小时。DATA 确认丢失或进程在投递结果落库前崩溃属于不确定结果，按常规重试并永久保留“可能重复”标记。收到明确 DATA 成功响应后，本进程只重试记录结果，不重复网络发送。单个应用实例运行一个队列 worker；不要对同一个数据库同时启动多个 Maildrop 实例。

撤销策略立即终止 queued/retry。已开始的网络会话可完成一次；若本次失败，撤销标记阻止后续重试，即使随后重新授权也不恢复旧任务。重新发起一封邮件需要新的请求标识。

## 收件、容量和兼容

新邮箱容量默认为 100 MiB，历史默认 3 MiB（包括旧环境配置值）升级为 100 MiB。数据库通过一次性迁移升级原默认值，保留 50 MiB 等其他自定义值；之后管理员显式设定的值不再被重启覆盖。管理员可调整收件容量，范围 1–10000 MiB；缩小到已用量以下不会删除邮件，只会拒绝新增邮件。

新收件按完整 `.eml` 字节大小计入每个邮箱，包含附件与头部。旧邮件没有原文的仍可阅读，沿用既有计费和保留期限；其原文和附件接口返回 404。正文支持字符集、alternative、related，转发邮件保留为附件；解码合计上限 50 MiB、嵌套 32 层、200 个 MIME 部分、100 个附件，超限明确拒收。

SMTP RCPT 提前拒绝不存在、过期、停用或已满邮箱。DATA 重查全部目标和原有管理员 From 限制，任何目标不合格则整批不落库，返回明确错误。重复目标去重。全部成功才回 250，SQL 故障回 451，可重试。

SPF、DKIM、DMARC 校验默认开启，最多 5 个 DKIM 签名、8 秒 DNS 验证窗口。失败、无记录或 DNS 异常均接收并保存本机验证结果，不信任原文的 Authentication-Results。白名单和 allowed_domains 收件规则保持原语义，仍可独立拒收。邮件 HTML 在无权限 sandbox iframe 中显示，阻断脚本、外部图像、链接跳转和表单；CID 位图通过鉴权 API 获取后嵌入。

## 服务器配置

以 `.env.example` 为变量清单。开启外发前应配置：

```dotenv
SMTP_HOSTNAME=mail.example.com
SMTP_TLS_CERT_FILE=data/tls/fullchain.pem
SMTP_TLS_KEY_FILE=data/tls/privkey.pem
VERIFY_INBOUND=true
OUTBOUND_ENABLED=true
DKIM_KEY_DIR=data/dkim
DKIM_SELECTOR=maildrop
OUTBOUND_IPV4_ONLY=true
```

证书与私钥均未配置时，兼容旧环境继续明文收件；配置任意一项但无法加载时，启动失败。配齐后自动公告 STARTTLS。证书在启动时加载，续签后重启生效。上游 MX 支持 STARTTLS 时进行证书校验，失败重试且不自动降级；上游不公告 STARTTLS 时使用普通 SMTP。

每个发件域使用 `${DKIM_KEY_DIR}/${domain}.pem`。支持 RSA PKCS#1/PKCS#8 与 Ed25519 PKCS#8。生产建议 RSA 2048 位或更强。密钥仅位于服务器，不进入仓库；发送前签名失败返回 503，不入队。例：

```sh
mkdir -p data/dkim data/tls
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out data/dkim/example.com.pem
chmod 600 data/dkim/example.com.pem
openssl pkey -in data/dkim/example.com.pem -pubout -outform DER | openssl base64 -A
```

将最后一条命令输出的公钥放入 `maildrop._domainkey.example.com` TXT：`v=DKIM1; k=rsa; p=<公钥>`。密钥目录和证书目录已加入 .gitignore；Docker 的 data 卷覆盖这两个目录。

运行前应核验实际服务器 TCP 25 出口、IPv4/IPv6 路由、A/MX/PTR、发件域 SPF 和 DKIM/DMARC。SPF 必须覆盖真实直投 IP；DMARC 从可观察策略开始，再按实际业务调整。邮箱域 MX 需指向此服务以接收随机退信地址。真实网络送达、证书、DNS 及服务器信誉只能在目标服务器环境验收，本地测试不证明这些条件。

## HTTP API

完整参数、响应结构、错误码及调用示例见 [API 文档](../API_DOCUMENTATION.md)；MCP 对应能力见 [发信工具参考](mcp-outbound.md)。本节为路由摘要。

邮箱接口一律 `Authorization: Bearer <邮箱 access_token>`，不能把令牌放入 URL。每次请求重新检查邮箱状态及归属；API 响应 `Cache-Control: no-store`。管理员策略接口使用主管理员 Bearer 密码。

| 方法与路径 | 用途 |
| --- | --- |
| GET `/api/emails/{id}/content` | 正文版本、附件元数据与本机身份验证结果 |
| GET `/api/emails/{id}/raw` | 下载收到的 `.eml` |
| GET `/api/emails/{id}/attachments/{attachment_id}` | 下载指定附件；一律 attachment + nosniff |
| GET `/api/sending-policy` | 发件授权、额度、用量、发件存档用量 |
| POST `/api/send` | 提交邮件，必填 `Idempotency-Key` 头（1–128 字节） |
| GET `/api/sent?offset=0` | 发送记录，每页 50 条 |
| GET `/api/sent/{id}` | 邮件内容、收件人状态和退信报告摘要 |
| GET `/api/sent/{id}/raw` | 下载发件原文，不含 Bcc |
| GET `/api/sent/{id}/attachments/{attachment_id}` | 下载发件附件 |
| DELETE `/api/sent/{id}` | 删除已完成发送记录；不恢复额度 |
| GET `/api/delivery-reports/{id}/raw` | 下载关联退信原文 |
| GET/PUT `/api/admin/mailboxes/{id}/sending-policy` | 主管理员读写权限与额度 |

策略 PUT 示例：

```json
{"enabled": true, "daily_limit": 100, "minute_limit": 10}
```

发送 JSON 示例（附件 content 为 base64；text 与 html 可选，允许空正文；整个 JSON 请求最多 16 MiB）：

```json
{
  "to": ["customer@example.net"],
  "cc": [],
  "bcc": [],
  "subject": "业务回复",
  "text": "您好，附件请查收。",
  "reply_email_id": "原邮件的内部 ID，可省略",
  "attachments": [{"filename": "note.txt", "content_type": "text/plain", "content": "aGVsbG8="}]
}
```

成功返回 202 和发送记录。相同 key 与相同规范化请求返回原记录；相同 key 不同请求返回 409；旧记录已清理返回 410。未授权返回 403，收件人数超额 429，附件超限 413，存档不足 409，服务器或 DKIM 未配置 503。额度、容量、幂等记录和队列在一个事务中提交。队列网络 I/O 不占数据库事务。

## 验证命令

```sh
go vet ./...
go test ./...
python -m pytest -q tests
python scripts/verify_ui_browser.py --quick --skip-perf
python scripts/verify_mail_features.py
```

浏览器脚本仅使用临时数据库、随机测试密钥与保留域名，不修改生产数据；收发专项脚本覆盖网络响应丢失后的幂等重试及授权撤销。
