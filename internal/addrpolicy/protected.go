// Package addrpolicy 提供所有创建邮箱入口共用的地址策略，避免各入口规则不一致。
package addrpolicy

import (
	"errors"
	"regexp"
	"strings"
)

var (
	// ErrProtected 表示地址命中 PROTECTED_ADDRESSES，任何入口都不得创建。
	ErrProtected = errors.New("该邮箱地址受保护，无法创建")
	// ErrInvalidPattern 表示保护规则无法解析；此时拒绝创建，避免错误配置绕过保护。
	ErrInvalidPattern = errors.New("保护地址正则表达式无效")
)

// Protected 按 PROTECTED_ADDRESSES 匹配邮箱 local-part，语义等同 Python re.match（从开头匹配）。
// 构造后只读，可并发使用。
type Protected struct {
	pattern *regexp.Regexp
	err     error
}

// NewProtected 编译保护规则；空字符串表示不保护任何地址。
func NewProtected(pattern string) *Protected {
	if strings.TrimSpace(pattern) == "" {
		return &Protected{}
	}
	compiled, err := regexp.Compile(pattern)
	return &Protected{pattern: compiled, err: err}
}

// Check 返回 nil 表示允许创建；local-part 比较不区分大小写之外的处理保持原样。
func (p *Protected) Check(address string) error {
	if p == nil {
		return nil
	}
	if p.err != nil {
		return ErrInvalidPattern
	}
	if p.pattern == nil {
		return nil
	}
	local, _, _ := strings.Cut(address, "@")
	// 最左匹配从 0 开始即等同 re.match；不要求匹配到末尾。
	if match := p.pattern.FindStringIndex(local); match != nil && match[0] == 0 {
		return ErrProtected
	}
	return nil
}
