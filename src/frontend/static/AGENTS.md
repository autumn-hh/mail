# 静态资源知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
静态资源以原生 JS/CSS 为主，直接由模板加载。

## 去哪里找
| 任务 | 位置 | 说明 |
| --- | --- | --- |
| 首页脚本 | src/frontend/static/scripts/main.js | 随机邮箱与收件箱展示 |
| 邮箱管理 | src/frontend/static/scripts/mailbox_manager.js | 用户邮箱全功能界面 |
| 管理面板 | src/frontend/static/scripts/admin.js | 白名单与设置 |
| 管理邮箱 | src/frontend/static/scripts/admin_mailbox.js | 邮箱列表与审计 |
| 注册流程 | src/frontend/static/scripts/register.js | 管理员验证与注册 |
| API 测试 | src/frontend/static/scripts/api_test.js | 手动接口测试 |
| 主题/语言 | src/frontend/static/scripts/theme.js | 主题切换 |
| 主题/语言 | src/frontend/static/scripts/language.js | 语言切换 |

## 约定
- fetch 调用使用相对路径 `/api` 与 `/api/admin`。
- 认证信息保存在 localStorage 或 sessionStorage。
- 没有打包工具，不使用 import/require 进行模块化。

## 反模式
- 不要依赖 `/api/create_mailbox_v2` 与 `/api/send_test_email`（后端默认缺失/禁用）。
- 不要在脚本中硬编码域名，使用后端返回的 `available_domains`。
