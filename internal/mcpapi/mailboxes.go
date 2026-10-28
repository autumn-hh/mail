package mcpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"maildrop/internal/admin"
	"maildrop/internal/store"
)

type mailboxInput struct {
	Mailbox string `json:"mailbox" jsonschema:"Mailbox ID or full email address"`
}
type getMailboxInput struct {
	Mailbox            string `json:"mailbox"`
	IncludeCredentials bool   `json:"include_credentials,omitempty" jsonschema:"Explicitly include mailbox API token and web key; default false"`
}
type createInput struct {
	Address         string   `json:"address,omitempty" jsonschema:"Full email address; omit to generate a random address"`
	Domain          string   `json:"domain,omitempty" jsonschema:"Enabled receiving domain for a random address; omit for the default"`
	RetentionDays   *int     `json:"retention_days,omitempty"`
	SenderWhitelist []string `json:"sender_whitelist,omitempty"`
	AllowedDomains  []string `json:"allowed_domains,omitempty" jsonschema:"Allowed sender domains, independent of receiving domain"`
}
type updateInput struct {
	Mailbox          string    `json:"mailbox"`
	RetentionDays    *int      `json:"retention_days,omitempty"`
	SenderWhitelist  *[]string `json:"sender_whitelist,omitempty"`
	AllowedDomains   *[]string `json:"allowed_domains,omitempty"`
	WhitelistEnabled *bool     `json:"whitelist_enabled,omitempty"`
}
type statusInput struct {
	Mailbox string `json:"mailbox"`
	Enabled bool   `json:"enabled"`
}
type deleteMailboxInput struct {
	Mailbox   string `json:"mailbox"`
	Permanent bool   `json:"permanent,omitempty" jsonschema:"False disables the mailbox; true irreversibly deletes mailbox and emails"`
}
type deleteBatchInput struct {
	Mailboxes []string `json:"mailboxes"`
	Permanent bool     `json:"permanent,omitempty"`
}
type createBatchInput struct {
	Items []createInput `json:"items" jsonschema:"1 to 100 mailboxes; partial success, per-item results"`
}
type listMailboxInput struct {
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"1 to 200; default 20"`
	Search   string `json:"search,omitempty"`
	Status   string `json:"status,omitempty" jsonschema:"active, expired, disabled, or empty for all"`
	Source   string `json:"source,omitempty"`
}
type senderInput struct {
	Mailbox string `json:"mailbox"`
	Sender  string `json:"sender"`
}

func (a *api) mailbox(ctx context.Context, ref string) (*store.Mailbox, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, invalid("mailbox is required")
	}
	var m *store.Mailbox
	var err error
	if strings.Contains(ref, "@") {
		m, err = a.db.GetMailboxByAddress(ref)
	} else {
		m, err = a.db.GetMailboxByID(ref)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalid("Mailbox not found")
	}
	return m, err
}
func (a *api) address(in createInput) (string, error) {
	domains, err := a.db.ListEnabledDomains()
	if err != nil {
		return "", err
	}
	if len(domains) == 0 {
		return "", invalid("No receiving domain enabled")
	}
	domain := in.Domain
	if in.Address != "" {
		_, parsed, ok := strings.Cut(in.Address, "@")
		if !ok || parsed == "" || strings.Contains(parsed, "@") {
			return "", invalid("address must be a full email address")
		}
		if domain != "" && !strings.EqualFold(domain, parsed) {
			return "", invalid("address and domain disagree")
		}
		domain = parsed
	}
	if domain == "" {
		domain = domains[0].Domain
	}
	domain, err = store.NormalizeDomain(domain)
	if err != nil {
		return "", invalid("Invalid domain")
	}
	found := false
	for _, d := range domains {
		if d.Domain == domain {
			found = true
		}
	}
	if !found {
		return "", invalid("Receiving domain is not enabled")
	}
	address := in.Address
	if address == "" {
		prefix, err := randomPrefix()
		if err != nil {
			return "", err
		}
		address = prefix + "@" + domain
	} else {
		local, _, _ := strings.Cut(address, "@")
		address = local + "@" + domain
	}
	if err := a.admin.ValidateLocalPart(address); err != nil {
		return "", business(err)
	}
	return address, nil
}
func (a *api) create(ctx context.Context, in createInput) (any, error) {
	address, err := a.address(in)
	if err != nil {
		return nil, err
	}
	out, err := a.admin.CreateMailbox(ctx, admin.CreateOptions{Address: address, RetentionDays: in.RetentionDays, SenderWhitelist: in.SenderWhitelist, AllowedDomains: in.AllowedDomains}, actor(ctx))
	return out, business(err)
}
func (a *api) mailboxTools() {
	add(a, "generate_address", "Generate a candidate address on an enabled domain. Does not create a mailbox.", true, false, false, func(ctx context.Context, in struct {
		Domain string `json:"domain,omitempty"`
	}) (any, error) {
		address, err := a.address(createInput{Domain: in.Domain})
		return map[string]any{"address": address}, err
	})
	add(a, "create_mailbox", "Create a mailbox on an enabled receiving domain; returns its new credentials.", false, false, false, a.create)
	add(a, "create_mailboxes_batch", "Create 1-100 mailboxes with per-item results. Partial success; never blindly retry a completed batch.", false, false, false, func(ctx context.Context, in createBatchInput) (any, error) {
		if len(in.Items) < 1 || len(in.Items) > 100 {
			return nil, invalid("items must contain 1-100 entries")
		}
		results := []any{}
		for i, item := range in.Items {
			if ctx.Err() != nil {
				return map[string]any{"results": results, "interrupted": true}, nil
			}
			out, err := a.create(ctx, item)
			if err != nil {
				results = append(results, map[string]any{"index": i, "success": false, "error": batchError(err)})
			} else {
				results = append(results, map[string]any{"index": i, "success": true, "mailbox": out})
			}
		}
		return map[string]any{"results": results}, nil
	})
	add(a, "list_mailboxes", "List mailbox summaries with pagination and filters. Credentials are never returned.", true, false, true, func(ctx context.Context, in listMailboxInput) (any, error) {
		if in.Page == 0 {
			in.Page = 1
		}
		if in.PageSize == 0 {
			in.PageSize = 20
		}
		if in.Page < 1 || in.Page > 1000000 || in.PageSize < 1 || in.PageSize > 200 {
			return nil, invalid("Invalid pagination")
		}
		if in.Status != "" && in.Status != "active" && in.Status != "expired" && in.Status != "disabled" {
			return nil, invalid("Invalid status")
		}
		out, err := a.admin.ListMailboxes(ctx, admin.ListOptions{Page: in.Page, PageSize: in.PageSize, Search: in.Search, Status: in.Status, Source: in.Source})
		return redact(out), err
	})
	add(a, "get_mailbox", "Get mailbox settings, expiry, counts and storage. Credentials hidden unless include_credentials=true.", true, false, true, func(ctx context.Context, in getMailboxInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		out, err := a.admin.GetMailboxDetail(ctx, m.ID)
		if in.IncludeCredentials {
			return out, err
		}
		return redact(out), err
	})
	add(a, "update_mailbox", "Update retention, sender whitelist, allowed sender domains or whitelist enabled state. Empty lists clear rules.", false, true, false, func(ctx context.Context, in updateInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		fields := map[string]any{}
		if in.RetentionDays != nil {
			fields["retention_days"] = *in.RetentionDays
		}
		if in.SenderWhitelist != nil {
			fields["sender_whitelist"] = *in.SenderWhitelist
		}
		if in.AllowedDomains != nil {
			fields["allowed_domains"] = *in.AllowedDomains
		}
		if in.WhitelistEnabled != nil {
			fields["whitelist_enabled"] = *in.WhitelistEnabled
		}
		err = a.admin.UpdateMailbox(ctx, m.ID, fields, actor(ctx))
		return success(), business(err)
	})
	add(a, "set_mailbox_status", "Set administrator enabled state explicitly; enabling clears the admin-disabled marker.", false, true, true, func(ctx context.Context, in statusInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		err = a.admin.UpdateMailbox(ctx, m.ID, map[string]any{"is_active": in.Enabled}, actor(ctx))
		return success(), business(err)
	})
	add(a, "delete_mailbox", "Disable a mailbox by default. permanent=true irreversibly deletes the mailbox and all its emails.", false, true, false, func(ctx context.Context, in deleteMailboxInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		err = a.admin.DeleteMailbox(ctx, m.ID, !in.Permanent, actor(ctx))
		return success(), business(err)
	})
	add(a, "delete_mailboxes_batch", "Disable or permanently delete 1-100 mailboxes. Returns per-item success/failure.", false, true, false, func(ctx context.Context, in deleteBatchInput) (any, error) {
		if len(in.Mailboxes) < 1 || len(in.Mailboxes) > 100 {
			return nil, invalid("mailboxes must contain 1-100 entries")
		}
		results := []any{}
		for _, ref := range in.Mailboxes {
			if ctx.Err() != nil {
				return map[string]any{"results": results, "interrupted": true}, nil
			}
			m, err := a.mailbox(ctx, ref)
			if err == nil {
				err = a.admin.DeleteMailbox(ctx, m.ID, !in.Permanent, actor(ctx))
			}
			item := map[string]any{"mailbox": ref, "success": err == nil}
			if err != nil {
				item["error"] = batchError(business(err))
			}
			results = append(results, item)
		}
		return map[string]any{"results": results}, nil
	})
	add(a, "reset_mailbox_token", "Rotate a mailbox API token using 32 random bytes. Old token stops working; returns new token once.", false, true, false, func(ctx context.Context, in mailboxInput) (any, error) { return a.rotate(ctx, in.Mailbox, false) })
	add(a, "rotate_mailbox_key", "Rotate a mailbox web key as super-admin; existing web links stop working. Returns new key.", false, true, false, func(ctx context.Context, in mailboxInput) (any, error) { return a.rotate(ctx, in.Mailbox, true) })
	add(a, "get_sender_whitelist", "Get sender whitelist and enabled state for a mailbox.", true, false, true, func(ctx context.Context, in mailboxInput) (any, error) {
		m, e := a.mailbox(ctx, in.Mailbox)
		if e != nil {
			return nil, e
		}
		return map[string]any{"senders": m.SenderWhitelist, "enabled": m.WhitelistEnabled}, nil
	})
	add(a, "add_sender_whitelist", "Add a sender address or @domain rule without changing whitelist enabled state.", false, false, true, func(ctx context.Context, in senderInput) (any, error) { return a.changeSender(ctx, in, true) })
	add(a, "remove_sender_whitelist", "Remove a sender rule without changing whitelist enabled state.", false, true, true, func(ctx context.Context, in senderInput) (any, error) { return a.changeSender(ctx, in, false) })
	add(a, "set_whitelist_enabled", "Enable or disable a mailbox sender whitelist explicitly.", false, true, true, func(ctx context.Context, in statusInput) (any, error) {
		m, e := a.mailbox(ctx, in.Mailbox)
		if e != nil {
			return nil, e
		}
		e = a.admin.UpdateMailbox(ctx, m.ID, map[string]any{"whitelist_enabled": in.Enabled}, actor(ctx))
		return success(), business(e)
	})
}
func (a *api) auditTx(ctx context.Context, tx *sql.Tx, action string, id *string, changes any) error {
	raw, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	aid, err := token()
	if err != nil {
		return err
	}
	who := actor(ctx)
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_logs(id,timestamp,action,mailbox_id,admin_user,changes,ip_address) VALUES(?,?,?,?,?,?,?)", aid, timeNow(), action, id, who.User, string(raw), who.IP)
	return err
}
func (a *api) rotate(ctx context.Context, ref string, key bool) (any, error) {
	m, err := a.mailbox(ctx, ref)
	if err != nil {
		return nil, err
	}
	column, action := "access_token", "MCP_RESET_TOKEN"
	value, err := token()
	if key {
		column, action = "mailbox_key", "MCP_ROTATE_KEY"
		value, err = store.GenerateMailboxKey()
	}
	if err != nil {
		return nil, err
	}
	err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE mailboxes SET "+column+"=? WHERE id=?", value, m.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return invalid("Mailbox not found")
		}
		return a.auditTx(ctx, tx, action, &m.ID, map[string]any{"rotated": true})
	})
	return map[string]any{"mailbox_id": m.ID, column: value}, err
}
func (a *api) changeSender(ctx context.Context, in senderInput, add bool) (any, error) {
	m, err := a.mailbox(ctx, in.Mailbox)
	if err != nil {
		return nil, err
	}
	sender := strings.TrimSpace(in.Sender)
	if len(sender) == 0 || len(sender) > 320 || strings.ContainsAny(sender, "\r\n") {
		return nil, invalid("Invalid sender rule")
	}
	if strings.HasPrefix(sender, "@") {
		if _, err := store.NormalizeDomain(sender[1:]); err != nil {
			return nil, invalid("Invalid sender domain")
		}
	} else if !strings.Contains(sender, "@") {
		if _, err := store.NormalizeDomain(sender); err != nil {
			return nil, invalid("Invalid sender domain")
		}
	} else {
		local, domain, _ := strings.Cut(sender, "@")
		if local == "" {
			return nil, invalid("Invalid sender")
		}
		if _, err := store.NormalizeDomain(domain); err != nil {
			return nil, invalid("Invalid sender domain")
		}
	}
	err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT sender_whitelist FROM mailboxes WHERE id=?", m.ID).Scan(&raw); err != nil {
			return err
		}
		list := store.DecodeMailboxList(raw, m.ID, "sender_whitelist")
		next := []string{}
		found := false
		for _, s := range list {
			if s == sender {
				found = true
				if !add {
					continue
				}
			}
			next = append(next, s)
		}
		if add && !found {
			next = append(next, sender)
		}
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET sender_whitelist=? WHERE id=?", string(data), m.ID); err != nil {
			return err
		}
		return a.auditTx(ctx, tx, "MCP_WHITELIST", &m.ID, map[string]any{"sender": sender, "add": add})
	})
	return success(), err
}
