package mcpapi

import (
	"context"
	"net"
	"time"

	"maildrop/internal/ipguard"
)

type domainsInput struct {
	Domains []string `json:"domains" jsonschema:"1-200 receiving domains"`
}
type domainStateInput struct {
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
}
type auditInput struct {
	Mailbox string `json:"mailbox,omitempty" jsonschema:"Optional mailbox ID or address; omit for global audit logs"`
	Limit   int    `json:"limit,omitempty" jsonschema:"1-1000, default 100"`
}
type ipInput struct {
	IP string `json:"ip"`
}

type securityInput struct {
	BlockDuration *int `json:"block_duration,omitempty"`
	MaxAttempts   *int `json:"max_attempts,omitempty"`
	AttemptWindow *int `json:"attempt_window,omitempty"`
}

func (a *api) systemTools() {
	add(a, "health", "Check database availability within the authenticated MCP connection.", true, false, true, func(ctx context.Context, _ empty) (any, error) {
		err := a.db.DB().PingContext(ctx)
		return map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)}, err
	})
	add(a, "server_info", "Report MCP identity, capabilities and limits without configuration secrets.", true, false, true, func(context.Context, empty) (any, error) {
		return map[string]any{"name": "maildrop", "mcp_version": "1.0.0", "role": "superadmin", "transport": "streamable-http", "endpoint": "/mcp", "receive_only": true, "tool_count": 35, "limits": map[string]any{"mailbox_batch": 100, "domain_batch": 200, "email_delete_batch": 200, "email_page_size": 100, "wait_seconds": 25}, "security_config_persistent": false}, nil
	})
	add(a, "list_domains", "List all receiving domains and enabled state.", true, false, true, func(context.Context, empty) (any, error) { return a.admin.ListDomains() })
	add(a, "add_domains", "Add 1-200 domains. Partial success; reports added and skipped reasons; names normalized to Punycode.", false, false, false, func(ctx context.Context, in domainsInput) (any, error) {
		if len(in.Domains) < 1 || len(in.Domains) > 200 {
			return nil, invalid("domains must contain 1-200 entries")
		}
		added, skipped, err := a.admin.AddDomains(ctx, in.Domains, actor(ctx))
		return map[string]any{"added": added, "skipped": skipped}, err
	})
	add(a, "set_domain_enabled", "Set receiving-domain enabled state; refuses disabling the last enabled domain.", false, true, true, func(ctx context.Context, in domainStateInput) (any, error) {
		out, err := a.admin.SetDomainEnabled(ctx, in.Domain, in.Enabled, actor(ctx))
		return out, domainError(err)
	})
	add(a, "delete_domains", "Delete 1-200 domains. Refuses the entire batch if no enabled domain would remain; reports missing domains.", false, true, false, func(ctx context.Context, in domainsInput) (any, error) {
		if len(in.Domains) < 1 || len(in.Domains) > 200 {
			return nil, invalid("domains must contain 1-200 entries")
		}
		deleted, failed, err := a.admin.DeleteDomains(ctx, in.Domains, actor(ctx))
		return map[string]any{"deleted": deleted, "failed": failed}, domainError(err)
	})
	add(a, "get_stats", "Get global mailbox/email/unread counts.", true, false, true, func(ctx context.Context, _ empty) (any, error) { return a.admin.Stats(ctx) })
	add(a, "get_source_stats", "Get mailbox creation counts grouped by source.", true, false, true, func(ctx context.Context, _ empty) (any, error) { return a.admin.SourceStats(ctx) })
	add(a, "list_audit_logs", "Read recent global or mailbox audit entries. Credential values are redacted, including historical token resets.", true, false, true, func(ctx context.Context, in auditInput) (any, error) {
		if in.Limit == 0 {
			in.Limit = 100
		}
		if in.Limit < 1 || in.Limit > 1000 {
			return nil, invalid("limit must be 1-1000")
		}
		id := ""
		if in.Mailbox != "" {
			m, e := a.mailbox(ctx, in.Mailbox)
			if e != nil {
				return nil, e
			}
			id = m.ID
		}
		out, err := a.admin.GetAuditLogs(ctx, id, in.Limit)
		return redact(out), err
	})
	add(a, "list_blocked_ips", "List active IP authentication bans and seconds remaining.", true, false, true, func(context.Context, empty) (any, error) { return a.blocker.GetBlockedIPs(), nil })
	add(a, "unblock_ip", "Remove an IP ban and its failed-attempt history.", false, true, true, func(ctx context.Context, in ipInput) (any, error) {
		if net.ParseIP(in.IP) == nil {
			return nil, invalid("Invalid IP address")
		}
		if err := a.admin.LogAudit(ctx, "MCP_UNBLOCK_IP", nil, map[string]any{"ip": in.IP}, actor(ctx)); err != nil {
			return nil, err
		}
		a.blocker.UnblockIP(in.IP)
		return success(), nil
	})
	add(a, "get_security_config", "Get in-process authentication failure thresholds. Changes do not persist across restart.", true, false, true, func(context.Context, empty) (any, error) {
		return map[string]any{"config": a.blocker.GetConfig(), "persistent": false}, nil
	})
	add(a, "update_security_config", "Update in-process IP ban thresholds: duration 60-86400s, attempts 1-10, window 30-600s. Not persistent across restart.", false, true, true, func(ctx context.Context, in securityInput) (any, error) {
		if in.BlockDuration == nil && in.MaxAttempts == nil && in.AttemptWindow == nil {
			return nil, invalid("At least one setting is required")
		}
		if in.BlockDuration != nil && (*in.BlockDuration < 60 || *in.BlockDuration > 86400) {
			return nil, invalid("block_duration must be 60-86400")
		}
		if in.MaxAttempts != nil && (*in.MaxAttempts < 1 || *in.MaxAttempts > 10) {
			return nil, invalid("max_attempts must be 1-10")
		}
		if in.AttemptWindow != nil && (*in.AttemptWindow < 30 || *in.AttemptWindow > 600) {
			return nil, invalid("attempt_window must be 30-600")
		}
		changes := map[string]any{}
		if in.BlockDuration != nil {
			changes["block_duration"] = *in.BlockDuration
		}
		if in.MaxAttempts != nil {
			changes["max_attempts"] = *in.MaxAttempts
		}
		if in.AttemptWindow != nil {
			changes["attempt_window"] = *in.AttemptWindow
		}
		if err := a.admin.LogAudit(ctx, "MCP_SECURITY_CONFIG", nil, changes, actor(ctx)); err != nil {
			return nil, err
		}
		if err := a.blocker.UpdateConfig(ipguard.ConfigUpdate{BlockDuration: in.BlockDuration, MaxAttempts: in.MaxAttempts, AttemptWindow: in.AttemptWindow}); err != nil {
			return nil, business(err)
		}
		return map[string]any{"config": a.blocker.GetConfig(), "persistent": false}, nil
	})
}
func domainError(err error) error {
	if err == nil {
		return nil
	}
	switch err.Error() {
	case "cannot remove the last enabled domain":
		return invalid("At least one enabled receiving domain must remain")
	case "not found":
		return invalid("Domain not found")
	case "invalid domain":
		return invalid("Invalid domain")
	}
	return err
}
