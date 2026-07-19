# Maildrop UI 重构总结

## 1. 目标与选型

本次重构在不引入前端构建链的前提下，统一公开登录、收件箱、邮箱注册、只读 API 调试台和管理后台的视觉与交互。实现继续使用 Flask 模板、原生 CSS 和原生 JavaScript，避免为现有项目增加不必要的框架与运行时依赖。

视觉方案采用低饱和浅色渐变与 Soft UI（柔和拟物）语言：

- 大圆角表面配合亮、暗双阴影，形成克制的浮雕层次；
- Hover 上浮、Active 内凹，统一使用 180ms 柔和动效；
- 以绿色主色强调主要操作，状态色仅用于成功、警告和错误反馈；
- 使用系统字体栈和本地 SVG sprite，不加载 Font Awesome、外部字体或图标 CDN；
- 保留亮色、暗色和跟随系统三种主题，并尊重减少动态效果的系统偏好。

## 2. 共享基础

| 资源 | 职责 |
| --- | --- |
| `src/frontend/static/styles/ui_foundation.css` | 设计令牌、Soft UI 表面、按钮、表单、表格、Toast、Dialog、焦点态、响应式规则和减少动效规则 |
| `src/frontend/static/icons/maildrop-icons.svg` | 全站线性图标 sprite，替代第三方图标字体 |
| `src/frontend/static/scripts/theme.js` | `auto / light / dark` 主题初始化与切换；偏好仅写入当前标签页的 `sessionStorage` |
| `src/frontend/static/scripts/ui_components.js` | 暴露 `MaildropUI`，统一图标、Toast、Dialog 和 Busy 状态 |

`MaildropUI` 的稳定接口如下：

- `MaildropUI.icon(name, className = '')`
- `MaildropUI.toast(message, type = 'info', options = {})`
- `MaildropUI.openDialog(dialog, opener = document.activeElement)`
- `MaildropUI.closeDialog(dialog)`
- `MaildropUI.setBusy(element, busy, label = '')`

所有页面先加载共享基础样式，再加载页面样式；脚本按 `theme.js`、`ui_components.js`、页面脚本的顺序加载，确保页面逻辑只依赖已经初始化的共享能力。

## 3. 页面与组件映射

| 页面 | 路由 | 主要组件 | 保留的业务边界 |
| --- | --- | --- | --- |
| 邮箱登录 | `/`、`/web` | 凭据表单、密码可见性、主题切换、状态提示 | 只生成 `/web/<编码邮箱>----<编码密钥>` |
| 收件箱 | `/web/<邮箱>----<密钥>` | 导航、邮件列表、详情、批量操作、空状态、切换邮箱 | 仅收件、查看、标记和删除，不提供写信能力 |
| 邮箱注册 | `/register` | 管理员验证、注册表单、域名选择、结果提示 | 使用现有管理员授权注册接口 |
| API 调试台 | `/api-test` | 凭据交换、邮箱信息、收件箱只读输出 | 凭据仅保存在运行时内存，不提供写接口 |
| 管理后台 | `/admin` | 登录、统计卡片、邮箱表格、审计、配置、子管理员和 Dialog | 保留既有 DOM 与 `/api/admin/*` 契约 |

## 4. 安全与可访问性约束

- 邮箱访问链接不兼容旧 `/mailbox?address=...&token=...` 形式，只使用固定的 `/web/<邮箱>----<密钥>` 路由。
- 邮箱访问令牌通过 `Authorization: Bearer` 发送，不写入查询参数；邮箱密钥不写入 Web Storage。
- API 调试台只调用密钥交换、邮箱信息和收件箱查询接口，所有输出使用纯文本方式渲染。
- 公开收件箱与调试台不包含写信、回复、转发、发件箱或测试发信入口。
- 管理端继续使用现有 Bearer 管理鉴权；动态服务端数据必须转义或通过 `textContent / value` 写入 DOM。
- 交互控件提供 `:focus-visible` 焦点提示，关键按钮保持可触达尺寸；Dialog 由共享组件处理焦点恢复。
- `prefers-reduced-motion: reduce` 下压缩动画与过渡；共享样式在 768px 和 480px 提供响应式降级。
- `/web/...` 中的邮箱密钥属于敏感凭据，部署时仍需对反向代理和可观测性访问日志进行脱敏。

## 5. 验证

在仓库根目录运行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify_ui.ps1
```

脚本依次执行：

1. Python `compileall` 语法检查；
2. `src/frontend/static/scripts/` 下全部 JavaScript 的 `node --check`；
3. `pytest` 回归测试；
4. `docker compose config --quiet` 配置校验。

需要同时验证镜像构建时运行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify_ui.ps1 -Build
```

### 最终结果

| 检查项 | 结果 |
| --- | --- |
| Python compileall | 通过 |
| 全部 JavaScript `node --check` | 通过 |
| pytest | `26 passed, 133 subtests passed` |
| Docker Compose 配置 | 通过 |
| Docker 镜像构建 | 在目标服务器通过 |
| 桌面与移动端浏览器验收 | 通过，无横向溢出或控制台错误 |
| 本地移动端 Lighthouse | Accessibility / Best Practices / SEO / Agentic Browsing 均为 `100` |

### 生产部署验证

- 生产入口：`https://tempmail.dearmer.xyz/`；部署目录：`/root/maildrop-master`；部署提交：`8271de6365fe0e93254aed260e54048ba79ac121`；
- 生产 `.env` 仅存在于服务器且权限为 `600`；Compose 服务 `tempmail` 为 `healthy`，Web 仅绑定 `127.0.0.1:8081`，SMTP 保持公网 `25`；
- OpenResty 上游使用 `127.0.0.1:8081`，HTTPS 响应包含 HSTS，证书私钥权限已收紧为 `600`，公网无法直连 `8081`；
- `/`、`/web`、`/admin`、`/register`、`/api-test` 与本地图标均返回 `200`；
- 旧 `/mailbox?address=...&token=...` 与不含 `----` 的 `/web/...` 均返回 `404`；
- 管理员认证、真实客户端 IP、凭据页禁止缓存响应头与 SMTP 投递到收件箱的端到端链路均已通过在线检查；测试邮箱、邮件和对应审计记录已精确清理；
- 数据库 `quick_check=ok`，部署前后保持 `137` 个邮箱、`49` 封邮件、`863` 条审计日志和 `2` 个子管理员；既有 `726` 条孤立审计日志外键问题未在本次 UI 部署中改写；
- 容器健康检查不保存 HTML 响应体；邮件数据目录权限为 `700`；回滚备份位于 `/root/maildrop-master-backups/20260719-231731-before-8271de6`。

## 6. 部署步骤

1. 准备 `.env`，仅在部署环境配置域名、管理密码、数据库路径和网络策略；不要把 `.env` 或数据文件提交到版本库。使用同机单层 HTTPS 反向代理时设置 `WEB_BIND_HOST=127.0.0.1` 和 `TRUST_PROXY_HOPS=1`，防止公网通过 `8081` 绕过 TLS，并只信任这一跳写入的客户端 IP；无受信代理时保持 `TRUST_PROXY_HOPS=0`。
2. 执行不带 `-Build` 的验证脚本，先确认源码、测试和 Compose 配置有效。
3. 执行带 `-Build` 的验证脚本，或单独运行 `docker compose build`。
4. 使用 `docker compose up -d` 启动服务。
5. 验证 `/`、`/web`、有效凭据链接、`/admin`、`/register` 和 `/api-test`，并确认旧页面与测试发信接口返回 `404`。
6. 将宿主机或 host-network 反向代理上游指向 `127.0.0.1:8081`，检查其覆盖 `X-Forwarded-For`、日志脱敏、HSTS 与缓存策略，并确认应用记录真实客户端 IP、公网无法直连 `8081` 后再开放访问。bridge 网络中的代理应改用受控共享网络，不可照搬 loopback 上游。
