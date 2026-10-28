# 前端知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
前端为纯静态 HTML 模板 + 原生 JS，无构建流程；Go 服务通过 go:embed（仓库根目录 embed.go）内嵌并原样输出。

## 目录结构
```
src/frontend/
├── templates/   # HTML 模板
└── static/      # JS/CSS/资源
```

## 去哪里找
| 任务 | 位置 | 说明 |
| --- | --- | --- |
| 管理端页面 | src/frontend/templates/admin_mailbox.html | `/admin` 唯一管理入口 |
| 邮箱页面 | src/frontend/templates/mailbox_manager.html | `/web/<邮箱>----<密钥>` 收件 UI |
| 注册页面 | src/frontend/templates/register.html | 用户注册 |
| 主页/入口 | src/frontend/templates/mailbox_login.html | `/` 与 `/web` 公开登录入口，右上角含管理员登录弹窗 |
| 脚本入口 | src/frontend/static/scripts/*.js | 直接加载 | 

## 约定
- 无构建工具，脚本直接通过模板引入。
- 所有 API 调用使用 `/api` 或 `/api/admin` 前缀。
- 交互逻辑以原生 JS 为主，避免引入额外框架。
- 公开邮箱只提供收件功能，不提供写信、回复、转发或已发送视图。

## 反模式
- 模板中不得使用 Jinja/Go 模板语法（`{{`、`{%`）；静态资源一律写成 `/static/...` 绝对路径。
- 新增静态资源目录时，需同步更新 embed.go 中的 go:embed 列表，否则不会打包进二进制。
- 不要新增未实现的 API 调用，先确认后端路由存在。
