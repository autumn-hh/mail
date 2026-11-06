package inbox

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRawStorageSharedContentAndCleanup(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	one := f.mailbox(t, "one@test.example", CreateMailboxOptions{})
	two := f.mailbox(t, "two@test.example", CreateMailboxOptions{})
	raw := "From: a@sender.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n--x\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename=cafe.txt\r\n\r\ncaf\xe9\r\n--x--\r\n"
	message := f.message(t, raw)
	result, err := f.service.Deliver(ctx, "", []string{one.Address, two.Address}, message)
	if err != nil || result.Accepted != 2 {
		t.Fatal(result, err)
	}
	if assertStorage(t, f, one.ID, 1) != int64(len(raw)) || assertStorage(t, f, two.ID, 1) != int64(len(raw)) {
		t.Fatal("raw bytes not accounted")
	}
	var count int
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 1 {
		t.Fatal("raw duplicated", count)
	}
	content, err := f.db.ContentForEmail(ctx, one.ID, result.Recipients[0].EmailID)
	if err != nil || !bytes.Equal(content.Raw, []byte(raw)) {
		t.Fatal(content, err)
	}
	if content.Details.Attachments[0].Size != 4 {
		t.Fatal("text attachment charset changed", content)
	}
	if _, err = f.service.DeleteEmail(ctx, result.Recipients[0].EmailID); err != nil {
		t.Fatal(err)
	}
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 1 {
		t.Fatal("shared content deleted early")
	}
	if _, err = f.db.DB().Exec(`DELETE FROM mailboxes WHERE id=?`, two.ID); err != nil {
		t.Fatal(err)
	}
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan raw content", count)
	}
}
func TestBatchPolicyFailureStoresNothing(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	one := f.mailbox(t, "one@test.example", CreateMailboxOptions{})
	two := f.mailbox(t, "two@test.example", CreateMailboxOptions{SenderWhitelist: []string{"allowed@sender.test"}})
	result, err := f.service.Deliver(ctx, "", []string{one.Address, two.Address}, f.message(t, "From: rejected@sender.test\r\n\r\nbody"))
	if err != nil || result.Accepted != 0 || result.Rejected != 2 {
		t.Fatal(result, err)
	}
	assertStorage(t, f, one.ID, 0)
	assertStorage(t, f, two.ID, 0)
	var count int
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan content after rejection")
	}
	if _, err = f.db.DB().Exec(`CREATE TRIGGER reject_second BEFORE INSERT ON emails WHEN NEW.mailbox_id='` + two.ID + `' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	result, err = f.service.Deliver(ctx, "", []string{one.Address, two.Address}, f.message(t, "From: allowed@sender.test\r\n\r\nbody"))
	if err == nil || result.Accepted != 0 {
		t.Fatal("storage failure did not roll back")
	}
	assertStorage(t, f, one.ID, 0)
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan content after rollback")
	}
}
func TestPruningRawAndFullMessageLimit(t *testing.T) {
	f := setup(t, nil)
	f.service.cfg.MaxEmailsPerAddress = 1
	one := f.mailbox(t, "one@test.example", CreateMailboxOptions{})
	for range 3 {
		result, err := f.service.Deliver(context.Background(), "", []string{one.Address}, f.message(t, "From: a@sender.test\r\n\r\nbody"))
		if err != nil || result.Accepted != 1 {
			t.Fatal(result, err)
		}
	}
	var count int
	f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count)
	if count != 1 {
		t.Fatal("pruned content not removed", count)
	}
	f.service.cfg.MaxEmailSizeBytes = 50
	result, err := f.service.Deliver(context.Background(), "", []string{one.Address}, f.message(t, "X-Large: "+strings.Repeat("a", 100)+"\r\n\r\nx"))
	if err != nil || result.Accepted != 0 || result.Recipients[0].Code != 552 {
		t.Fatal("raw header bytes bypassed limit", result, err)
	}
}
