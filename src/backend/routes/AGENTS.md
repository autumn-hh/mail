# 路由知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
路由层分为页面路由与 API 路由，统一通过 Blueprint 注册。

## 去哪里找
| 任务 | 位置 | 说明 |
| --- | --- | --- |
| 页面渲染 | src/backend/routes/pages.py | 管理端与邮箱页面 |
| 用户 API | src/backend/routes/api.py | `/api/*` 接口 |
| 管理 API | src/backend/routes/admin_api.py | `/api/admin/*` 接口 |

## 约定
- 用户 API Blueprint：`/api` 前缀。
- 管理 API Blueprint：`/api/admin` 前缀。
- 管理接口鉴权使用 `Authorization` 头（支持 `Bearer ` 前缀）。
- 用户邮箱访问使用 token 查询参数或管理员密码（`/api/get_inbox`）。
- 对外接口统一先做 IP 白名单检查。

## 反模式
- 不要重新启用 `/api/create_mailbox_v2`（已禁用）。
- 不要绕过管理员鉴权或 IP 白名单。
- 若新增接口，避免与 `/api` 和 `/api/admin` 既有命名冲突。
