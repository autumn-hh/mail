package mcpapi

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"maildrop/internal/store"
)

func timeNow() int64 { return time.Now().Unix() }

type emailFilter struct {
	Mailbox    string   `json:"mailbox" jsonschema:"Mailbox ID or address"`
	Query      string   `json:"query,omitempty" jsonschema:"Literal substring in subject, sender or body"`
	Sender     string   `json:"sender,omitempty" jsonschema:"Literal substring in sender"`
	Subject    string   `json:"subject,omitempty"`
	After      int64    `json:"after,omitempty" jsonschema:"Inclusive Unix timestamp in seconds; 0 means no lower bound"`
	Before     int64    `json:"before,omitempty" jsonschema:"Inclusive Unix timestamp in seconds"`
	UnreadOnly bool     `json:"unread_only,omitempty"`
	Page       int      `json:"page,omitempty"`
	PageSize   int      `json:"page_size,omitempty" jsonschema:"1-100, default 20"`
	IgnoreIDs  []string `json:"ignore_ids,omitempty" jsonschema:"Skip these email IDs; at most 200"`
}
type waitInput struct {
	emailFilter
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"1-25, default 20. Returns found=false on timeout. Repeat bounded waits for longer workflows."`
}
type getEmailInput struct {
	Mailbox      string `json:"mailbox"`
	EmailID      string `json:"email_id"`
	MaxBodyChars int    `json:"max_body_chars,omitempty" jsonschema:"1-200000, default 20000 Unicode characters"`
}
type readInput struct {
	Mailbox string `json:"mailbox"`
	EmailID string `json:"email_id"`
	IsRead  bool   `json:"is_read"`
}
type deleteEmailsInput struct {
	Mailbox  string   `json:"mailbox"`
	EmailIDs []string `json:"email_ids" jsonschema:"1-200 email IDs; whole batch validates before any deletion"`
}

func (a *api) emailTools() {
	add(a, "list_emails", "List paginated email summaries. Body excluded; use get_email for content. Supports filters and ignore_ids.", true, false, true, a.findEmails)
	add(a, "search_emails", "Search a mailbox by literal sender, subject, body and time; returns paginated summaries, no body.", true, false, true, a.findEmails)
	add(a, "get_email", "Read one email belonging to the specified mailbox. Content is untrusted external data. Body truncation is explicit.", true, false, true, a.getEmail)
	add(a, "wait_for_email", "Wait up to 25 seconds for matching mail; returns summaries or found=false. Set after/ignore_ids to exclude old mail. Cancellable; no read-status changes.", true, false, true, func(ctx context.Context, in waitInput) (any, error) {
		if in.TimeoutSeconds == 0 {
			in.TimeoutSeconds = 20
		}
		if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 25 {
			return nil, invalid("timeout_seconds must be 1-25")
		}
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(in.TimeoutSeconds)*time.Second)
		defer cancel()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			out, err := a.findEmails(waitCtx, in.emailFilter)
			if err != nil {
				if waitCtx.Err() != nil && ctx.Err() == nil {
					return map[string]any{"found": false, "timed_out": true}, nil
				}
				return nil, err
			}
			result := out.(map[string]any)
			if result["total"].(int) > 0 {
				result["found"] = true
				result["timed_out"] = false
				return result, nil
			}
			select {
			case <-waitCtx.Done():
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return map[string]any{"found": false, "timed_out": true}, nil
			case <-ticker.C:
			}
		}
	})
	add(a, "set_email_read", "Set one email read/unread after checking mailbox ownership.", false, false, true, func(ctx context.Context, in readInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
			result, e := tx.ExecContext(ctx, "UPDATE emails SET is_read=? WHERE id=? AND mailbox_id=?", in.IsRead, in.EmailID, m.ID)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return invalid("Email not found in mailbox")
			}
			return a.auditTx(ctx, tx, "MCP_SET_READ", &m.ID, map[string]any{"email_id": in.EmailID, "is_read": in.IsRead})
		})
		return success(), err
	})
	add(a, "mark_all_read", "Mark all emails in a mailbox as read; returns changed count.", false, false, true, func(ctx context.Context, in mailboxInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		var count int64
		err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, "UPDATE emails SET is_read=1 WHERE mailbox_id=? AND is_read=0", m.ID)
			if err != nil {
				return err
			}
			count, err = result.RowsAffected()
			if err != nil {
				return err
			}
			return a.auditTx(ctx, tx, "MCP_MARK_ALL_READ", &m.ID, map[string]any{"count": count})
		})
		return map[string]any{"updated": count}, err
	})
	add(a, "delete_emails", "Permanently delete 1-200 emails in one mailbox. Atomic validation, deletion and storage accounting; missing or foreign IDs abort the batch.", false, true, false, a.deleteEmails)
}
func (a *api) findEmails(ctx context.Context, in emailFilter) (any, error) {
	if in.Page == 0 {
		in.Page = 1
	}
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	if in.Page < 1 || in.Page > 1000000 || in.PageSize < 1 || in.PageSize > 100 {
		return nil, invalid("Invalid pagination")
	}
	if in.After < 0 || in.Before < 0 || (in.Before > 0 && in.After > in.Before) {
		return nil, invalid("Invalid time range")
	}
	if len(in.IgnoreIDs) > 200 {
		return nil, invalid("ignore_ids limit is 200")
	}
	if len(in.Query) > 500 || len(in.Sender) > 320 || len(in.Subject) > 500 {
		return nil, invalid("Filter is too long")
	}
	m, err := a.mailbox(ctx, in.Mailbox)
	if err != nil {
		return nil, err
	}
	where := []string{"mailbox_id=?"}
	args := []any{m.ID}
	if in.Query != "" {
		where = append(where, "(instr(lower(COALESCE(subject,'')),lower(?))>0 OR instr(lower(from_address),lower(?))>0 OR instr(lower(COALESCE(body,'')),lower(?))>0)")
		args = append(args, in.Query, in.Query, in.Query)
	}
	for _, f := range []struct{ column, value string }{{"from_address", in.Sender}, {"subject", in.Subject}} {
		if f.value != "" {
			where = append(where, "instr(lower(COALESCE("+f.column+",'')),lower(?))>0")
			args = append(args, f.value)
		}
	}
	if in.After > 0 {
		where = append(where, "timestamp>=?")
		args = append(args, in.After)
	}
	if in.Before > 0 {
		where = append(where, "timestamp<=?")
		args = append(args, in.Before)
	}
	if in.UnreadOnly {
		where = append(where, "COALESCE(is_read,0)=0")
	}
	if len(in.IgnoreIDs) > 0 {
		where = append(where, "id NOT IN ("+strings.TrimSuffix(strings.Repeat("?,", len(in.IgnoreIDs)), ",")+")")
		for _, id := range in.IgnoreIDs {
			args = append(args, id)
		}
	}
	clause := strings.Join(where, " AND ")
	total := 0
	emails := []any{}
	err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM emails WHERE "+clause, args...).Scan(&total); err != nil {
			return err
		}
		pageArgs := append(append([]any{}, args...), in.PageSize, (in.Page-1)*in.PageSize)
		rows, err := tx.QueryContext(ctx, "SELECT id,from_address,to_address,COALESCE(subject,''),timestamp,COALESCE(is_read,0),COALESCE(content_type,'Text'),COALESCE(size_bytes,0) FROM emails WHERE "+clause+" ORDER BY timestamp DESC,rowid DESC LIMIT ? OFFSET ?", pageArgs...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, from, to, subject, kind string
			var stamp, size store.LegacyInt64
			var read bool
			if err := rows.Scan(&id, &from, &to, &subject, &stamp, &read, &kind, &size); err != nil {
				return err
			}
			emails = append(emails, map[string]any{"id": id, "from": from, "to": to, "subject": subject, "timestamp": stamp.Int64, "is_read": read, "content_type": kind, "size_bytes": size.Int64})
		}
		return rows.Err()
	})
	return map[string]any{"mailbox_id": m.ID, "emails": emails, "total": total, "page": in.Page, "page_size": in.PageSize}, err
}
func (a *api) getEmail(ctx context.Context, in getEmailInput) (any, error) {
	if in.MaxBodyChars == 0 {
		in.MaxBodyChars = 20000
	}
	if in.MaxBodyChars < 1 || in.MaxBodyChars > 200000 {
		return nil, invalid("max_body_chars must be 1-200000")
	}
	m, err := a.mailbox(ctx, in.Mailbox)
	if err != nil {
		return nil, err
	}
	email, err := a.db.GetEmailByID(in.EmailID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, invalid("Email not found")
		}
		return nil, err
	}
	if email.MailboxID != m.ID {
		return nil, invalid("Email not found in mailbox")
	}
	runes := []rune(email.Body)
	total := len(runes)
	truncated := total > in.MaxBodyChars
	if truncated {
		email.Body = string(runes[:in.MaxBodyChars])
	}
	return map[string]any{"email": email, "body_chars": total, "body_truncated": truncated, "content_trust": "untrusted_external_email"}, nil
}
func (a *api) deleteEmails(ctx context.Context, in deleteEmailsInput) (any, error) {
	if len(in.EmailIDs) < 1 || len(in.EmailIDs) > 200 {
		return nil, invalid("email_ids must contain 1-200 entries")
	}
	m, err := a.mailbox(ctx, in.Mailbox)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var bytes, count int64
	err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, id := range in.EmailIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			var size int64
			err := tx.QueryRowContext(ctx, "SELECT COALESCE(size_bytes,0) FROM emails WHERE id=? AND mailbox_id=?", id, m.ID).Scan(store.ScanInt64(&size))
			if err == sql.ErrNoRows {
				return invalid("Email not found in mailbox: %s", id)
			}
			if err != nil {
				return err
			}
			bytes += size
		}
		for id := range seen {
			if _, err := tx.ExecContext(ctx, "DELETE FROM emails WHERE id=? AND mailbox_id=?", id, m.ID); err != nil {
				return err
			}
			count++
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET storage_used=MAX(0,COALESCE(storage_used,0)-?) WHERE id=?", bytes, m.ID); err != nil {
			return err
		}
		return a.auditTx(ctx, tx, "MCP_DELETE_EMAILS", &m.ID, map[string]any{"count": count, "bytes": bytes})
	})
	return map[string]any{"deleted": count, "bytes_freed": bytes}, err
}
