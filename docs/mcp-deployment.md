# MCP 部署与运维

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
| 说明 | `/opt/maildrop/docs/mcp.md`、`mcp-deployment.md` |
| Web / SMTP | `127.0.0.1:5000` / `0.0.0.0:25` |
| HTTPS | 1Panel OpenResty 容器 `1Panel-openresty-PG6X` |
| 站点配置 | `/opt/1panel/www/conf.d/mail.roixw.com.conf` |

真实认证信息不保存在本文或 Git。root 可通过 SSH 读取私密客户端配置后导入可信客户端。原后台 PASSWORD 保持原配置，MCP 使用独立随机 Token。

## 构建与发布

```bash
go vet ./...
go test ./... -count=1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o maildrop-linux-amd64 ./cmd/maildrop
sha256sum maildrop-linux-amd64
```

更新前记录版本与校验和，在 root 专用目录备份二进制、环境、systemd unit、反代配置和数据库。在线 SQLite 必须通过 backup API 取得一致备份，不能只复制主 db 忽略 WAL。备份目录 0700、敏感文件 0600。

候选程序先在数据库备份的另一份副本上启动，使用 loopback 高位 HTTP/SMTP 端口，验证迁移、healthz、MCP 握手与工具发现。启动会清理副本过期数据，不可指向生产库。

保留原业务设置，只增加 MCP_ADMIN_TOKEN 和 MCP_ALLOWED_HOSTS=mail.roixw.com,localhost,127.0.0.1,::1。HTTP 保持 loopback，不开放新公网端口。反代须保留 Host、Authorization 和正确的客户端 IP，一层反代使用 TRUST_PROXY_HOPS=1。现有 root.conf 已符合转发要求，默认超时容纳 25 秒等待；JSON 响应无需 SSE 配置。若修改反代，先备份并执行 docker exec 1Panel-openresty-PG6X nginx -t，成功再 reload。

原子替换二进制后 systemctl restart maildrop，检查服务、HTTP、SMTP 和数据库。失败恢复旧程序与环境。生产启动仍会执行原有过期清理，数量变化应与清理日志核对。

## 日常检查

```bash
systemctl is-active maildrop
curl --fail http://127.0.0.1:5000/healthz
ss -lntp '( sport = :25 or sport = :5000 )'
journalctl -u maildrop --since '15 minutes ago' --no-pager
```

公网验证：无认证 /mcp 返回 401；正确凭据完成 initialize、tools/list（35 项）和 stats。收信验证仅创建专用测试邮箱，SMTP 投递测试信，通过 MCP 读取，再删除该测试邮箱。测试覆盖本机 SMTP 到 MCP，外部运营商送达仍依赖 MX、网络与发件方策略。

## 凭据轮换与撤销

用安全编辑器修改环境文件 MCP_ADMIN_TOKEN，同步更新 /etc/maildrop/mcp-client.json 及客户端，然后 systemctl restart maildrop。推荐 Python secrets.token_urlsafe(48)，不复用网页密码或邮箱凭据。旧值在重启后无效，不支持多把 MCP Token 并存。撤销时置空并重启，/mcp 返回 503，网页与 SMTP 按原配置运行。

不要把真实 Token 放 URL、Git、问题单、共享日志或公共文档。旧备份中的秘密同样需要保护。

## 回滚

从 /etc/maildrop/mcp-release.json 读取本次 backup。MCP 本身没有独立数据库表，优先只恢复旧程序和环境，保留部署后新信。

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

自动测试覆盖真实 MCP HTTP SDK、SQLite、SMTP、35 工具发现、认证/Host/Origin、邮件归属、等待/取消、Unicode 截断、批删回滚、域名保护、凭据隐藏/轮换。部署还需完成副本预演、公网 HTTPS 握手和测试信清理，结果记入发布记录。本次未改前端，不包含全部页面视觉回归。
