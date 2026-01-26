# 前端知识库

**Generated:** 2026-01-25
**Commit:** e22c636
**Branch:** master

## 概览
前端为服务端渲染模板 + 原生 JS，无构建流程。

## 目录结构
```
src/frontend/
├── templates/   # HTML 模板
└── static/      # JS/CSS/资源
```

## 去哪里找
| 任务 | 位置 | 说明 |
| --- | --- | --- |
| 管理端页面 | src/frontend/templates/admin.html | 登录与白名单管理 |
| 邮箱页面 | src/frontend/templates/mailbox_manager.html | 用户邮箱 UI |
| 注册页面 | src/frontend/templates/register.html | 用户注册 |
| 主页/入口 | src/frontend/templates/index.html | 当前重定向到管理页面 |
| 脚本入口 | src/frontend/static/scripts/*.js | 直接加载 | 

## 约定
- 无构建工具，脚本直接通过模板引入。
- 所有 API 调用使用 `/api` 或 `/api/admin` 前缀。
- 交互逻辑以原生 JS 为主，避免引入额外框架。

## 反模式
- 不要在模板中硬编码静态路径，使用现有的静态资源引用方式。
- 不要新增未实现的 API 调用，先确认后端路由存在。
