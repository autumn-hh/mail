# MCP 部署与运维

更新日期：2026-11-06。生产已部署源码 `48c4482`，MCP **1.1.0 / 41 个工具**；本页记录本次实际配置与后续运维方法。

## 当前生产布局

| 项目 | 位置 |
| --- | --- |
| 主机 | `172.96.161.181`（reliablesite） |
| Web / 后台 / MCP | `https://mail.roixw.com` / `/admin` / `/mcp` |
| 服务 | `maildrop.service`，用户 maildrop |
| 程序 | `/opt/maildrop/maildrop` |
| 工作目录 / SQLite | `/var/lib/maildrop` / `/var/lib/maildrop/data/mailbox.db` |
| 环境文件（含秘密） | `/etc/maildrop/maildrop.env`，root:root 0600 |
| 客户端配置（含秘密） | `/etc/maildrop/mcp-client.json`，root:root 0600 |
| 发布记录及备份路径 | `/etc/maildrop/mcp-release.json`（无凭据） |
| 发布源码 | `/opt/maildrop/releases/<发布编号>/source` |
| 本次发布 | `/opt/maildrop/releases/outbound-20261106T0943Z` |
| 升级前备份 | `/var/lib/maildrop/backups/pre-outbound-20261106T0943Z` |
| SMTP TLS | `/var/lib/maildrop/data/tls/fullchain.pem`、`privkey.pem`，root:maildrop 0640 |
| DKIM | `/var/lib/maildrop/data/dkim/<发件域>.pem`，私钥 0600，selector 为 `maildrop` |
| 证书同步 | `/usr/local/sbin/maildrop-sync-smtp-cert`，`maildrop-cert-sync.timer` 每 6 小时执行 |
| 说明 | 服务器 `/opt/maildrop/API_DOCUMENTATION.md` 与 `/opt/maildrop/docs/` 中的 MCP/运行文档 |
| Web / SMTP | `127.0.0.1:5000` / `0.0.0.0:25` |
| HTTPS | 1Panel OpenResty 容器 `1Panel-openresty-PG6X` |
| 站点配置 | `/opt/1panel/www/conf.d/mail.roixw.com.conf` |

真实认证信息不保存在本文或 Git。root 可通过 SSH 读取私密客户端配置后导入可信客户端。原后台 PASSWORD 保持原配置，MCP 使用独立随机 Token。

当前非秘密配置：

```dotenv
SMTP_HOSTNAME=mail.roixw.com
SMTP_TLS_CERT_FILE=/var/lib/maildrop/data/tls/fullchain.pem
SMTP_TLS_KEY_FILE=/var/lib/maildrop/data/tls/privkey.pem
VERIFY_INBOUND=true
OUTBOUND_ENABLED=true
DKIM_KEY_DIR=/var/lib/maildrop/data/dkim
DKIM_SELECTOR=maildrop
OUTBOUND_IPV4_ONLY=true
```

`roixw.com`、`dearmer.xyz`、`dearmer.edu.kg` 已分别配置 RSA 2048 位 DKIM、公网 SPF 与 DMARC。SPF 包含真实出口 `172.96.161.181`；DMARC 当前为 `p=none; adkim=r; aspf=r`。A/PTR 对应 `mail.roixw.com`，三个域的 MX 指向该主机，HELO 主机名也有 SPF。

SMTP 证书来源为 1Panel 站点证书目录。同步任务只在新证书有效且内容变化时复制并重启 maildrop；部署时证书有效期截至 2027-01-24。每个邮箱仍须独立授权；本次仅授权一只验证邮箱，其余保持原策略。

## 构建与发布

```bash
go vet ./...
go test ./... -count=1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o maildrop-linux-amd64 ./cmd/maildrop
sha256sum maildrop-linux-amd64
```

更新前记录版本与校验和，在 root 专用目录备份二进制、环境、systemd unit、反代配置和数据库。在线 SQLite 必须通过 backup API 取得一致备份，不能只复制主 db 忽略 WAL。备份目录 0700、敏感文件 0600。

候选程序先在数据库备份的另一份副本上启动，使用 loopback 高位 HTTP/SMTP 端口，验证迁移、healthz、MCP 握手与工具发现。启动会清理副本过期数据，不可指向生产库。

保留原业务设置和凭据；MCP 使用独立 MCP_ADMIN_TOKEN 与 MCP_ALLOWED_HOSTS=mail.roixw.com,localhost,127.0.0.1,::1。出站所需变量见上表及 [收发运行说明](mail-operations.md)。HTTP 保持 loopback，不开放新公网端口。反代须保留 Host、Authorization 和正确的客户端 IP，一层反代使用 TRUST_PROXY_HOPS=1。默认超时容纳 25 秒等待；JSON 响应无需 SSE 配置。当前站点配置已设 `client_max_body_size 16m`，与 MCP / 发信 JSON 上限一致。修改反代前备份，执行 `docker exec 1Panel-openresty-PG6X nginx -t` 成功后 reload。

原子替换二进制后 systemctl restart maildrop，检查服务、HTTP、SMTP 和数据库。失败恢复旧程序与环境。生产启动仍会执行原有过期清理，数量变化应与清理日志核对。

## 日常检查

```bash
systemctl is-active maildrop
systemctl is-active maildrop-cert-sync.timer
curl --fail http://127.0.0.1:5000/healthz
ss -lntp '( sport = :25 or sport = :5000 )'
journalctl -u maildrop --since '15 minutes ago' --no-pager
```

公网只读验证：正确凭据完成 initialize、tools/list（本版 41 项）和 maildrop_server_info，版本应为1.1.0，outbound_enabled应为true。无认证请求会返回401并可能累计认证失败次数，日常检查使用有效凭据。收信链路可用专用测试邮箱验证；对外发信需先确定授权发件邮箱和收件目标，检查发送记录及收件方反馈，不能只看入队响应。

## 凭据轮换与撤销

用安全编辑器修改环境文件 MCP_ADMIN_TOKEN，同步更新 /etc/maildrop/mcp-client.json 及客户端，然后 systemctl restart maildrop。推荐 Python secrets.token_urlsafe(48)，不复用网页密码或邮箱凭据。旧值在重启后无效，不支持多把 MCP Token 并存。撤销时置空并重启，/mcp 返回 503，网页与 SMTP 按原配置运行。

不要把真实 Token 放 URL、Git、问题单、共享日志或公共文档。旧备份中的秘密同样需要保护。

## 回滚

从 /etc/maildrop/mcp-release.json 读取本次 backup。本版包含出站队列、原文/附件表及一次性容量迁移。优先只恢复旧程序和环境，保留部署后新信；旧程序不会处理新出站队列，应先核对排队记录。不要为回滚直接恢复旧库丢弃新信。

```bash
# BACKUP_DIR 设为发布记录中的实际绝对路径
systemctl stop maildrop
install -m 0755 "$BACKUP_DIR/maildrop" /opt/maildrop/maildrop.rollback
mv /opt/maildrop/maildrop.rollback /opt/maildrop/maildrop
install -m 0600 "$BACKUP_DIR/maildrop.env" /etc/maildrop/maildrop.env
systemctl start maildrop
systemctl is-active maildrop
```

仅当确有损坏且接受丢弃备份后新信时才恢复数据库。先停服务，将现有 db/WAL/SHM 整套保留到另一目录，恢复一致备份，设置 maildrop:maildrop、0600，再启动。不要把旧 WAL/SHM 留在恢复库旁。

## 验证范围

自动测试已覆盖真实 MCP HTTP SDK、SQLite、SMTP、41 工具发现、认证/Host/Origin、邮件归属、等待/取消、Unicode 截断、批删回滚、域名保护、凭据隐藏/轮换，以及发件附件、幂等、额度与撤权。

本次生产部署已完成数据库副本预演、公网 HTTPS 握手和 41 工具发现、原有邮箱/收信/凭据保留校验、SMTP STARTTLS、真实 IPv4 出口及 10 条 DNS TXT 公网核验。写信和发送记录页面在新加载的生产浏览器中验收，覆盖 1440px 与 390px。

经用户授权从 roixw.com 向 QQ 发送一封验证邮件，对方 SMTP 接受；用户确认在垃圾邮件中收到。通过存档原文与公共 DNS 独立验证 SPF/DKIM/DMARC 通过，但未检查 QQ 侧 Authentication-Results，不能宣称 QQ 侧验证通过或进入收件箱。另两个域名完成密钥和 DNS 配置，未逐一实测公网投递。

程序 SHA256：`b27fd9e1e6bf19ed8d44ce806953f375be34eab30022010261b705a0efb8db25`。详细结果与回滚路径在服务器发布记录中；工具 Schema 的无凭据副本见 [mcp-tools.schema.json](mcp-tools.schema.json)。
