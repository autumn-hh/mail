# 模板知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
HTML 模板由 Flask 渲染，页面脚本在静态目录中加载。

## 去哪里找
| 页面 | 位置 | 说明 |
| --- | --- | --- |
| 管理首页 | src/frontend/templates/admin.html | 管理登录与设置 |
| 管理邮箱 | src/frontend/templates/admin_mailbox.html | 邮箱列表/审计 |
| 邮箱管理 | src/frontend/templates/mailbox_manager.html | 用户邮箱 UI |
| 邮箱登录 | src/frontend/templates/mailbox_login.html | token/密钥登录 |
| 注册页面 | src/frontend/templates/register.html | 管理员验证 + 注册 |
| API 测试 | src/frontend/templates/api_test.html | 接口测试页 |
| 主页 | src/frontend/templates/index.html | 目前重定向 |

## 约定
- 页面通过 `render_template` 渲染，不走前端路由。
- 脚本与样式从 `static/` 目录加载。
- 管理页默认走 `/admin` 与 `/admin/mailboxes`。

## 反模式
- 不要在模板中直接写业务逻辑，保持为展示层。
