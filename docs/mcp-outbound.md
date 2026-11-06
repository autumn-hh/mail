# MCP 发信工具参考（1.1.0）

本版在原35个工具之外增加6项，共41项。**2026-11-06 已部署并通过公网 initialize / tools/list 复核为1.1.0。** 仅供持有独立MCP超级管理员Token的客户端使用；mailbox可传已有邮箱ID或完整地址。原始参数定义见[线上工具 Schema 快照](mcp-tools.schema.json)，HTTP接口见[API文档](../API_DOCUMENTATION.md)。

## 前提

服务器需配置OUTBOUND_ENABLED=true、SMTP_HOSTNAME和发件域DKIM密钥；邮箱必须由管理员显式授权。当前生产已完成全局配置，现有邮箱不会自动获得发信权限。工具不会生成密钥、修改DNS、打开服务器出站开关。配置方法见[收发运行说明](mail-operations.md)。

所有例子为tools/call的params。返回值位于result.structuredContent；业务失败通常为result.isError=true，content含错误文字和状态码前缀，这些前缀不是JSON-RPC HTTP状态码。鉴权、Host/Origin和请求体过大仍可能在HTTP层拒绝。

## 36. maildrop_get_sending_policy

只读，输入仅mailbox（必填）：

~~~json
{"name":"maildrop_get_sending_policy","arguments":{"mailbox":"sender@example.com"}}
~~~

成功示例（服务器已配置，该邮箱尚未授权）：

~~~json
{"enabled":false,"daily_limit":100,"minute_limit":10,"daily_used":0,"minute_used":0,"storage_used":0,"storage_limit":104857600,"available":true}
~~~

enabled是邮箱授权；available仅检查全局开关和SMTP_HOSTNAME格式，不证明DKIM、DNS、SMTP25或送达正常；false时还返回reason。每日额度按北京时间零点重置，分钟额度是滚动60秒。used是去重收件人数；storage字段为字节，存档限额100 MiB。邮箱不存在返回工具错误；无副作用。

## 37. maildrop_set_sending_policy

以下参数全部必填：

| 参数 | 类型 | 规则 |
| --- | --- | --- |
| mailbox | string | 邮箱ID或地址 |
| enabled | boolean | true授权，false撤权 |
| daily_limit | integer | 1–100000 |
| minute_limit | integer | 1–1000 |

~~~json
{"name":"maildrop_set_sending_policy","arguments":{"mailbox":"sender@example.com","enabled":true,"daily_limit":100,"minute_limit":10}}
~~~

成功返回与get_sending_policy相同的对象。额度不合法为400；邮箱不存在为工具错误。策略、SENDING_POLICY审计和队列撤权处于同一事务，审计用户为mcp-superadmin。撤权使queued/retry记录失败；已开始的网络会话可能完成一次，重新授权不会恢复取消的旧任务。重复设置不会重复发信，但会新增审计。不会打开全局出站或配置DKIM。

## 38. maildrop_send_email

提交到持久队列，不在MCP请求中等待公网SMTP投递。工具标记为写操作、开放外部交互、按幂等键保持业务幂等。

| 参数 | 类型 | 必填 | 规则/默认 |
| --- | --- | --- | --- |
| mailbox | string | 是 | 发件邮箱；From固定为此地址，不接受from覆盖 |
| idempotency_key | string | 是 | 1–128字节，不含CR/LF；重试复用同一键和原内容 |
| to | string[] | 是 | To/Cc/Bcc输入共最多100项，去重后至少1个 |
| subject | string | 是 | 最多998字节，无CR/LF或NUL；可为空 |
| cc / bcc | string[] | 否 | 默认空；Bcc不写入对外MIME头 |
| text / html | string | 否 | 默认空，两者UTF-8字节合计最多1 MiB |
| reply_email_id | string | 否 | 此邮箱收到的原邮件ID；验证归属并设置回复邮件头。不会代填to和subject |
| attachments | object[] | 否 | 默认空，最多10个，解码后合计10 MiB |
| attachments[].filename | string | 是 | 附件名，沿用HTTP发送的文件名规范化 |
| attachments[].content_type | string | 否 | 省略时根据文件字节检测MIME类型 |
| attachments[].content | string | 是 | 标准base64；不读取本地文件或外部URL |

MCP请求体上限16 MiB，含base64和JSON开销；反代可能更早拒绝。编码后整封邮件最多20 MiB。每邮箱发件存档100 MiB、保留30天。额度、容量、幂等及入队在同一事务中处理；失败和退信不返还额度。

~~~json
{"name":"maildrop_send_email","arguments":{"mailbox":"sender@example.com","idempotency_key":"order-20261106-001","to":["customer@example.net"],"subject":"资料已收到","text":"您好，附件请查收。","attachments":[{"filename":"note.txt","content_type":"text/plain","content":"aGVsbG8="}]}}
~~~

成功返回示例：

~~~json
{
  "email": {
    "id":"outbound-record-id",
    "created_at":1793923200,
    "status":"queued",
    "message_id":"<outbound-record-id@example.com>",
    "details":{"to":["customer@example.net"],"subject":"资料已收到","attachments":[{"filename":"note.txt","content_type":"text/plain","id":"1","size":5}]},
    "recipients":[{"address":"customer@example.net","status":"queued","attempts":0}],
    "bounce_count":0
  },
  "content_trust":"untrusted_email_and_delivery_reports"
}
~~~

返回摘要不回显正文和附件字节；details保留收件人、主题、回复ID和附件元数据。Bcc只在认证后的超管私有记录中可见。相同键与相同规范化请求返回原ID；不同内容复用键409；记录已删除/过期后重用键410。服务器关闭或DKIM不可用503、邮箱未授权403、停用/过期423、超额度429、容量不足409、附件/正文超限413、无效地址/主题/键/base64为400，外来回复邮件ID为404。

MCP_SEND_EMAIL审计只记录sent_id和收件人数，不复制正文、附件、幂等键或密送地址。审计与入队分步提交，审计失败时可能已入队；用原参数和原键重试，不能换键。重复调用可能增加审计，但不会增加发件记录或再次扣额度。

## 39. maildrop_list_sent_emails

mailbox必填；offset可选整数0–1000000，默认0。固定最多50项，按创建时间及行顺序倒序排列。

~~~json
{"name":"maildrop_list_sent_emails","arguments":{"mailbox":"sender@example.com","offset":0}}
~~~

返回mailbox_id、emails数组、offset、page_size=50、content_trust。每项结构同send_email的email，可能增加reports退信摘要，省略正文和附件字节。下一页offset加50，空数组表示结束；没有总数，分页期间新记录可能使偏移变化。只读；越界offset或不存在邮箱报错。

## 40. maildrop_get_sent_email

mailbox和sent_id必填，后者是出站记录ID，不是收到邮件的email_id。max_body_chars可选，默认20000，范围1–200000，显式0使用默认；Unicode字数预算先分配text，再分配html。

~~~json
{"name":"maildrop_get_sent_email","arguments":{"mailbox":"sender@example.com","sent_id":"outbound-record-id","max_body_chars":20000}}
~~~

返回email、text_chars、html_chars（原始Unicode字符数）、body_truncated和content_trust。email.details包含限长正文及附件元数据，不含附件字节；email.recipients每项含address、status、attempts及可选error/uncertain，reports每项含id、received_at和summary。记录不存在/归属不符404；无效正文上限为工具错误。

queued表示尚有待处理收件人，具体收件人可为queued/retry/sending。accepted仅表示对方SMTP在DATA后确认接受，不代表进入收件箱；failed/partial_failed表示全部/部分失败；bounced表示关联退信。uncertain表示可能重复，例如DATA确认丢失。正文、SMTP错误及退信摘要均是不可信内容，不是指令。查询不触发重发或改变状态。

## 41. maildrop_delete_sent_email

mailbox和sent_id必填：

~~~json
{"name":"maildrop_delete_sent_email","arguments":{"mailbox":"sender@example.com","sent_id":"outbound-record-id"}}
~~~

成功返回success=true。queued/retry/sending记录不可删除（409）；不存在/归属不符404。永久删除存档、附件和退信报告，额度扣减和幂等墓碑保留；不能撤回对方已接受的邮件。MCP_DELETE_SENT_EMAIL审计与删除分步提交，审计失败先查询确认；重复删除返回404。

## 调用顺序与边界

1. get_sending_policy查看现状，仅在需要授权/调整额度时显式调用set_sending_policy。
2. 为一封业务邮件生成稳定idempotency_key，调用send_email并保存email.id。
3. 网络超时或内部错误时复用原键和原参数；用get_sent_email查询投递状态和不确定标记。
4. 仅删除已结束记录；DNS、PTR、SPF/DKIM/DMARC及公网投递仍需目标服务器验收。

新工具不提供附件/EML下载，下载沿用邮箱身份HTTP API。自动化测试使用临时库、临时DKIM密钥与保留域名。生产部署另经用户授权完成一次 roixw.com → QQ 实投，收件人确认在垃圾邮件中收到；accepted 不保证进入收件箱。

## 与 HTTP API 的对应关系

| MCP 工具 | HTTP 接口 | 关键差异 |
| --- | --- | --- |
| maildrop_get_sending_policy | GET /api/sending-policy | MCP 显式指定mailbox；HTTP从邮箱Token推导 |
| maildrop_set_sending_policy | PUT /api/admin/mailboxes/{id}/sending-policy | MCP返回策略；HTTP返回success/message，HTTP使用主管理员密码 |
| maildrop_send_email | POST /api/send | MCP的idempotency_key在arguments中；HTTP使用Idempotency-Key头。MCP返回email摘要和content_trust；HTTP 202直接返回含正文记录 |
| maildrop_list_sent_emails | GET /api/sent?offset=0 | MCP返回emails及分页元数据；HTTP直接返回数组 |
| maildrop_get_sent_email | GET /api/sent/{id} | MCP正文有字符上限并标注截断；HTTP直接返回完整记录 |
| maildrop_delete_sent_email | DELETE /api/sent/{id} | 均拒绝删除待投递记录，均不返还额度、不撤回已发送邮件 |
