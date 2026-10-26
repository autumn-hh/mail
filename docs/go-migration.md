# Go 运行、迁移与域名设置

Go 1.26+ 运行整个后端：HTTP、SMTP、MIME 解析、SQLite 与每分钟过期清理。前端资源嵌入二进制，无需 Python 或 Node 运行时。现有 Python 实现留作迁移参考和回归测试，不参与 Go/Docker 启动。

## 启动

```sh
cp .env.example .env
# 编辑 PASSWORD；本地开发可设置 SMTP_PORT=2525
go build -o maildrop ./cmd/maildrop
./maildrop
# 或使用仓库 Compose，Web 端口 8081、SMTP 端口 25
docker compose up -d --build
```

Windows：`go build -o maildrop.exe ./cmd/maildrop`，然后 `.\maildrop.exe`。HTTP 默认端口 5000；继续兼容 `FLASK_HOST`/`FLASK_PORT`。Docker 中使用原有 `./data:/app/data` 挂载。未设置 `PASSWORD` 时管理员接口保持关闭。

## 首页登录和域名配置

首页点击“管理员登录”，在现有管理登录页输入管理员密码；登录后选择“系统设置”。“邮箱域名设置”每行填写一个域名，点击“保存域名”。大小写、重复项与末尾的点会归一化；协议、端口、路径、无效 DNS 标签和空列表会被拒绝。

- 域名保存到 SQLite `settings` 表，重启后保留，变更写入审计日志。
- `DOMAINS`/`DOMAIN` 仅用于首次初始化；之后以页面保存的列表为准。
- 首个域名为默认域名，创建页面和随机地址接口使用最新列表。
- 移除域名立即禁止该域名新建邮箱和 SMTP 收信。已有邮箱、邮件、密钥和 Token 保留，仍可在原有效期内访问。
- 子管理员的域名权限必须同时属于当前启用域名；不会因新增域名自动扩大权限。
- DNS A/MX 记录与公网 SMTP 25 端口仍由部署者配置；网页保存域名不代表 DNS 已生效。

## 已有 SQLite 数据

1. 停止旧 Python 服务，避免两个运行时同时接收或清理邮件。
2. 备份现有数据库。若有 WAL 文件，使用 SQLite 在线备份或停机后一起保存数据库及 WAL；不要仅在写入中复制主文件。
3. 让 `DATABASE_PATH` 指向原 `data/mailbox.db`，启动 Go 服务。
4. 验证旧邮箱登录、旧 Token、邮件正文、管理列表和收信。

现有邮箱 ID、访问 Token、邮箱密钥、邮件 ID 与正文直接复用。初始化只补充缺少字段和配置表，缺失邮箱密钥才生成新值；用户、邀请码及关联表留在原库。新访问 Token 使用 32 字节加密随机数。

回滚：停止 Go，恢复迁移前备份，再启动旧版本。Go 页面域名配置不写入旧 `.env`；回滚到 Python 时须同步所需的域名环境配置。清理服务会按照既有有效期删除过期邮箱及旧邮件，备份应在首次启动 Go 前完成。

## 旧 JSON 收件箱

Go 统一使用 SQLite；不再提供 JSON 运行时。旧 JSON 可显式导入，源文件保持不变，已有地址跳过，单次导入使用事务，错误时整体回滚：

```sh
# .env 设置 USE_DATABASE=true，DATABASE_PATH 指向目标库
./maildrop -import-json /path/to/inbox.json
```

支持旧地址→邮件列表与地址→邮箱信息两种结构，保留已有 Token、密钥、时间与邮件。原 JSON 没有凭据的邮箱会生成新凭据，在管理员详情中获取。

## 验证

```sh
go vet ./...
go test ./... -count=1
go build ./cmd/maildrop
python -m unittest discover -s tests -v
node --check src/frontend/static/scripts/admin_mailbox.js
```

Go 测试使用临时数据库与随机本机 SMTP 端口，覆盖旧表升级、凭据保持、域名持久化/权限、跨邮箱拒绝、原子批量删除、SMTP 信封收件人、MIME 解码、并发容量与清理，以及 JSON 重复导入。Python 测试仅用于旧行为与前端契约回归；不替代 Go 测试。

接口路径保持原样；管理员接口使用 `Authorization: Bearer <PASSWORD>`；邮箱接口只接受 Bearer Token。新增 `GET/PUT /api/admin/domains`，PUT 请求为 `{"domains":["example.com"]}`，成功响应为 `{"success":true,"data":{"domains":["example.com"]}}`。

后台现有“安全配置”的封禁参数与旧版一致，只在当前进程生效；持久化域名设置不改变该语义。
