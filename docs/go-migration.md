# Go 合并版本迁移

运行架构与域名管理统一采用模块化 Go 后端，见 [Go 重写说明](go-rewrite.md)。首页管理员登录、更新界面、域名启停及批量添加/删除均保留。Python 运行时代码已移除。

## 已有 SQLite

停止旧服务并备份数据库（含正在使用的 WAL），再以原 DATABASE_PATH 启动新二进制。不要同时运行两个收信或清理进程。

从本地旧 Go 版本升级时，自动将 settings 表中的 domains 设置迁入空的 domains 表；已有 domains 表配置优先。邮箱、邮件、ID、Token 和邮箱密钥保留，旧邮件容量记账补齐。迁移错误会整体回滚，不会静默使用环境变量覆盖保存的域名。之后在「域名设置」管理，环境变量只初始化空域名表。

域名删除/禁用与子管理员授权、SMTP 的具体语义遵循模块化版本，详见 go-rewrite.md。升级前后应检查管理员域名列表、旧邮箱登录、旧 Token 和实际收信。回滚前先停止新服务，再恢复升级前数据库备份和对应旧程序。

## 旧 JSON 显式导入

设置 DATABASE_PATH 指向目标 SQLite 后运行：

```sh
./maildrop -import-json /path/to/inbox.json
```

兼容「地址 → 邮件数组」和「地址 → 邮箱信息（含 emails）」两种结构。保持已有凭据和邮件内容，缺少凭据时生成安全随机值；已有地址跳过。整批使用事务，失败全部回滚，源文件不修改。运行时统一使用 SQLite。

## 健康检查与验证

```sh
./maildrop -healthcheck
go vet ./...
go test ./... -count=1
go build ./...
python -m unittest discover -s tests -q
python scripts/verify_ui_browser.py --quick --skip-perf
```

健康检查读取 FLASK_PORT 并请求本机 /healthz，HTTP 与数据库可用返回 200，数据库不可用返回 503。Docker 沿用模块化版本的部署方式。
