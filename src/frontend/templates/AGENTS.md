# 模板知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
HTML 模板是纯静态文件，由 Go 服务内嵌后原样输出（不做模板渲染），页面脚本在静态目录中加载。

## 去哪里找
| 页面 | 位置 | 说明 |
| --- | --- | --- |
| 管理邮箱 | src/frontend/templates/admin_mailbox.html | `/admin` 邮箱列表/审计/域名设置 |
| 邮箱管理 | src/frontend/templates/mailbox_manager.html | `/web/<邮箱>----<密钥>` 收件 UI |
| 邮箱登录 | src/frontend/templates/mailbox_login.html | `/` 与 `/web` 邮箱/密钥登录 |
| 注册页面 | src/frontend/templates/register.html | 管理员验证 + 注册 |
| API 测试 | src/frontend/templates/api_test.html | 只读接口调试页 |
| 主页 | src/frontend/templates/mailbox_login.html | 公开登录入口 |

## 约定
- 页面由 internal/web/pages.go 直接输出，不走前端路由；模板内不得出现 `{{`/`{%` 模板语法。
- 静态资源引用写成 `/static/...` 绝对路径。
- 脚本与样式从 `static/` 目录加载。
- 管理页只走 `/admin`；`/admin/mailboxes` 不保留兼容路由。
- `/mailbox` 携带旧 address/token 查询参数的页面链接不保留兼容。

## 反模式
- 不要在模板中直接写业务逻辑，保持为展示层。
