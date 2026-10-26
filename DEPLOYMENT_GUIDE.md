# Maildrop 部署与使用报告

本文档介绍 Maildrop 临时邮箱系统的部署流程、配置说明及使用方法。当前版本由单个 Go 二进制同时提供 HTTP、SMTP 与定时清理，不再依赖 Python。

## 1. 项目概述

Maildrop 是一个自托管的临时邮箱服务，允许用户在其自定义域名上接收电子邮件。Web、SMTP 与过期清理运行在同一进程中，邮件保存在 SQLite。

### 核心特性
- **多域名支持**：首次启动可用 `.env` 播种域名，之后在管理后台维护。
- **SQLite 存储**：数据库文件默认位于工作目录下的 `data/mailbox.db`。
- **管理面板**：可视化管理邮箱、域名、审计日志和安全配置。
- **生命周期管理**：自定义邮件和邮箱的保留期限。
- **API V2**：完善的 RESTful 接口，方便集成。

---

## 2. 部署指南

### 2.1 环境准备
- **操作系统**：Linux（推荐 Ubuntu 20.04+）。在 Windows 上交叉编译时使用 `GOOS=linux GOARCH=amd64`。
- **容器部署**：Docker，以及 Compose v2 插件（`docker compose`）或旧版 `docker-compose`。`deploy.sh` 会自动探测二者。
- **直接运行**：Go 1.26，与 `go.mod` 中的版本一致。
- **网络要求**：
  - SMTP 使用 **25** 端口。许多云服务商（如 AWS、GCP、阿里云）默认屏蔽 25 端口，需申请解封。
  - 进程内 Web 默认监听 **5000**。Docker Compose 把它映射到宿主机 **8081**，绑定地址由 `WEB_BIND_HOST` 决定。

### 2.2 域名配置 (关键步骤)
为了接收邮件，必须正确配置域名的 DNS 记录：
1. **A 记录**：将 `mail.yourdomain.com` 指向服务器的公网 IP。
2. **MX 记录**：将根域名 `yourdomain.com` 的 MX 记录指向 `mail.yourdomain.com`，优先级设为 10。

### 2.3 方式一：Docker Compose 部署（推荐）

1. **准备配置**：复制 `.env.example` 为 `.env`，按第 3 节修改域名和密码。`.env` 不会进入镜像。
2. **构建镜像**：
   ```bash
   docker compose build
   ```
   也可使用 `docker-compose build`，或 `./deploy.sh update`（会执行 `build --pull` 以拉取基础镜像）。

   镜像为多阶段构建。构建阶段使用 `golang:1.26-alpine`，先下载模块再编译静态二进制（`CGO_ENABLED=0`）。SQLite 驱动是纯 Go，不需要 C 工具链。前端模板和静态资源由 `go:embed` 打进二进制。运行阶段使用 `alpine:3.22`，只复制该二进制，并安装 `tzdata` 与 `ca-certificates`。
3. **启动**：
   ```bash
   docker compose up -d
   ```
   或 `./deploy.sh start`。脚本会检查 `.env`，并打印 `http://localhost:8081`。

容器工作目录是 `/app`。`DATABASE_PATH` 默认 `data/mailbox.db`，相对该目录，对应卷 `./data:/app/data`。Compose 通过 `env_file` 注入 `.env`；若工作目录里另有 `.env`，已存在的环境变量优先，不会被文件覆盖。

容器继续以 root 运行。已有部署的 bind mount 中 `data/mailbox.db` 常为 root 所有，且容器内要绑定 25 端口。收到 SIGTERM 后进程在 15 秒内退出；Compose 的 `stop_grace_period` 设为 20 秒。

端口映射：
- `${WEB_BIND_HOST:-0.0.0.0}:8081` → 容器 `5000`
- `25:25`（SMTP 需保持公网收信能力）

生产环境可将 `WEB_BIND_HOST=127.0.0.1`，避免绕过 HTTPS 反向代理直接访问管理页面。

### 2.4 方式二：直接运行二进制

不使用 Docker 时，在仓库根目录构建，并把 `.env` 放在启动时的工作目录旁（程序从当前目录加载 `.env`）：

```bash
go build -o maildrop ./cmd/maildrop
sudo ./maildrop
```

监听 25 端口需要 root，或给二进制赋予 `CAP_NET_BIND_SERVICE` 后再以普通用户运行：

```bash
sudo setcap cap_net_bind_service=+ep ./maildrop
./maildrop
```

此方式下 Web 默认监听 `FLASK_HOST:FLASK_PORT`（`0.0.0.0:5000`），没有 Compose 的 8081 映射。`DATABASE_PATH` 仍默认相对工作目录的 `data/mailbox.db`。

### 2.5 从 Python 版本升级

可以沿用原来的 `.env` 和 `data/mailbox.db`。升级前先停止旧进程或容器，并备份数据库。SQLite 以 WAL 模式运行，请同时备份旁边的文件：

```bash
cp -a data/mailbox.db data/mailbox.db.bak
cp -a data/mailbox.db-wal data/mailbox.db-wal.bak 2>/dev/null || true
cp -a data/mailbox.db-shm data/mailbox.db-shm.bak 2>/dev/null || true
```

然后用本节的 Go 镜像或二进制启动。首次启动会自动迁移表结构，无需单独执行迁移命令。

请确认升级前已经在使用 SQLite（`data/mailbox.db`）。`USE_DATABASE=false` 的 JSON 存储不再支持，`inbox.json` 不会被导入。

`DOMAINS` 只在域名表为空时播种。Python 版把域名放在环境变量中，数据库里没有 `domains` 表，因此升级后的第一次启动会用当前 `DOMAINS` 写入域名表。之后域名在管理后台的 **域名设置** 中增删和启停；再修改 `.env` 的 `DOMAINS` 不会覆盖已有域名。

`FLASK_SECRET_KEY` 已不再使用，留在 `.env` 中无影响。

---

## 3. 配置说明 (.env)

变量名与 Python 版相同。容器部署时由 Compose 的 `env_file` 注入；直接运行时从工作目录的 `.env` 加载。

| 变量名 | 说明 | 示例值 |
| :--- | :--- | :--- |
| `FLASK_HOST` | 进程内 Web 监听地址 | `0.0.0.0` |
| `FLASK_PORT` | 进程内 Web 端口；Compose 映射到宿主机 8081 | `5000` |
| `WEB_BIND_HOST` | 宿主机 Web 绑定地址，只影响 Compose 端口映射 | `127.0.0.1` |
| `SMTP_HOST` | SMTP 监听地址 | `0.0.0.0` |
| `SMTP_PORT` | SMTP 接收端口 | `25` |
| `PASSWORD` | 管理员登录密码 | `your_secure_password` |
| `DOMAINS` | 仅在域名表为空时播种，逗号分隔；之后以管理后台为准 | `domain1.com,domain2.com` |
| `DATABASE_PATH` | SQLite 路径，相对工作目录；容器内即 `/app/data/mailbox.db` | `data/mailbox.db` |
| `EMAIL_RETENTION_DAYS` | 邮件保留天数 | `7` |
| `MAILBOX_RETENTION_DAYS` | 邮箱保留天数 | `30` |
| `USE_DATABASE` | 已忽略。设为 `false` 不再切换到 JSON，始终使用 SQLite | `true` |
| `FLASK_SECRET_KEY` | 已不再使用 | （可删除） |

---

## 4. 使用说明

### 4.1 管理员功能
- **访问地址**：Compose 部署为 `http://<WEB_BIND_HOST>:8081/admin`；直接运行二进制时为 `http://<主机>:5000/admin`。这是管理后台入口。
- **登录**：使用 `.env` 中设置的 `PASSWORD`。未配置密码时管理接口拒绝认证，收信与公开邮箱仍可用。
- **功能**：
  - **仪表板**：查看全站统计数据。
  - **邮箱管理**：创建、禁用或硬删除邮箱地址。
  - **域名设置**：添加、启用、禁用或删除域名。首次播种之后以此处为准。
  - **审计日志**：追踪系统关键操作。
  - **安全监控**：查看被封禁的 IP 列表及解封。

### 4.2 用户功能
- **注册邮箱**：管理员可在管理面板或 `/register` 页面为用户手动创建邮箱。
- **登录查看**：用户通过 `/web/<编码邮箱>----<编码密钥>` 访问只收件的邮箱页面；不兼容旧 token 链接。
- **API 测试**：`/api-test` 只提供密钥换令牌、邮箱信息和收件箱查询。

---

## 5. 维护与排调

- **查看日志**：
  - Docker 模式：`docker compose logs -f`，或 `./deploy.sh logs`
  - 直接运行：查看进程的标准错误输出。
- **停止与更新**：`./deploy.sh stop`、`./deploy.sh restart`、`./deploy.sh update`。`update` 会先停止，再 `build --pull` 后启动。
- **数据库位置**：默认 `./data/mailbox.db`。升级或迁移前先备份该文件及 WAL 附属文件。
- **清理任务**：进程内按保留期限清理过期数据，不再使用独立的 Python 线程。
- **健康检查**：镜像用 BusyBox `wget` 请求 `http://127.0.0.1:5000/`，并丢弃响应体。

---
*报告更新日期：2026-10-26*
