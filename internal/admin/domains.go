package admin

import (
	"context"

	"maildrop/internal/store"
)

func (s *Service) ListDomains() ([]store.Domain, error) {
	return s.store.ListDomains()
}

// Store 先提交域名，再独立提交审计；审计失败时域名变更已经生效。
// 现有接口无法合并事务，禁止在 WithTx 内调用 Store 方法。
func (s *Service) AddDomain(ctx context.Context, value string, actor Actor) (*store.Domain, error) {
	domain, err := s.store.AddDomain(value)
	if err != nil {
		return nil, err
	}
	if err := s.LogAudit(ctx, "ADD_DOMAIN", nil, map[string]any{
		"domain": domain.Domain, "enabled": domain.Enabled,
	}, actor); err != nil {
		return nil, err
	}
	return domain, nil
}

func (s *Service) SetDomainEnabled(ctx context.Context, value string, enabled bool, actor Actor) (*store.Domain, error) {
	value, err := store.NormalizeDomain(value)
	if err != nil {
		return nil, store.ErrNotFound
	}
	if err := s.store.SetDomainEnabled(value, enabled); err != nil {
		return nil, err
	}
	if err := s.LogAudit(ctx, "UPDATE_DOMAIN", nil, map[string]any{
		"domain": value, "enabled": enabled,
	}, actor); err != nil {
		return nil, err
	}
	domains, err := s.store.ListDomains()
	if err != nil {
		return nil, err
	}
	for _, domain := range domains {
		if domain.Domain == value {
			return &domain, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Service) DeleteDomain(ctx context.Context, value string, actor Actor) error {
	value, err := store.NormalizeDomain(value)
	if err != nil {
		return store.ErrNotFound
	}
	if err := s.store.DeleteDomain(value); err != nil {
		return err
	}
	return s.LogAudit(ctx, "DELETE_DOMAIN", nil, map[string]any{"domain": value}, actor)
}
func (s *Service) AddDomains(ctx context.Context, values []string, actor Actor) ([]store.Domain, []store.DomainSkip, error) {
	added, skipped, err := s.store.AddDomains(values)
	if err != nil {
		return nil, nil, err
	}
	if len(added) > 0 {
		domains := make([]string, len(added))
		for i, domain := range added {
			domains[i] = domain.Domain
		}
		if err := s.LogAudit(ctx, "BATCH_ADD_DOMAINS", nil, map[string]any{
			"domains": domains, "skipped": len(skipped),
		}, actor); err != nil {
			return nil, nil, err
		}
	}
	return added, skipped, nil
}

func (s *Service) DeleteDomains(ctx context.Context, values []string, actor Actor) ([]string, []store.DomainSkip, error) {
	deleted, failed, err := s.store.DeleteDomains(values)
	if err != nil {
		return nil, nil, err
	}
	if len(deleted) > 0 {
		if err := s.LogAudit(ctx, "BATCH_DELETE_DOMAINS", nil, map[string]any{
			"domains": deleted, "failed": len(failed),
		}, actor); err != nil {
			return nil, nil, err
		}
	}
	return deleted, failed, nil
}
