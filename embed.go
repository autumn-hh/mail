// Package maildrop 打包前端模板与静态资源，供 Go 服务直接内嵌发布。
package maildrop

import "embed"

// FrontendFS 只包含页面模板与公开静态资源，不包含目录内的 AGENTS.md 等说明文件。
//
//go:embed src/frontend/templates/*.html
//go:embed src/frontend/static/scripts src/frontend/static/styles src/frontend/static/icons src/frontend/static/resources
var FrontendFS embed.FS
