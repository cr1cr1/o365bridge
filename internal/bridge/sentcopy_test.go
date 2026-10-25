package bridge

import (
	"context"
	"testing"
	"time"
)

func TestHeaderMessageID(t *testing.T) {
	cases := []struct {
		name string
		mime string
		want string
	}{
		{"plain", "From: a@b\r\nMessage-ID: <x@y>\r\nSubject: s\r\n\r\nbody\r\n", "<x@y>"},
		{"case-insensitive", "message-id:<z@w>\n\nbody", "<z@w>"},
		{"folded", "Message-ID:\r\n <fold@ed>\r\n\r\nbody", "<fold@ed>"},
		{"absent", "From: a@b\r\nSubject: s\r\n\r\nMessage-ID: <nope@body>\r\n", ""},
		{"empty-headers", "\r\nMessage-ID: <late@body>\r\n", ""},
		{"empty-value", "Message-ID:  \r\n\r\nbody", ""},
		{"no-body", "Message-ID: <only@header>\r\n", "<only@header>"},
	}
	for _, c := range cases {
		if got := HeaderMessageID([]byte(c.mime)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func newSentTestRegistry(t *testing.T) (*Registry, *time.Time) {
	t.Helper()
	now := time.Now()
	reg, err := NewRegistry(Config{
		StateDir: t.TempDir(),
		Login:    nopLogin{},
		Factory:  nopFactory{},
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg, &now
}

func TestSentCopyRecordAndMatch(t *testing.T) {
	reg, _ := newSentTestRegistry(t)
	reg.RecordSentCopy("User@Example.com", "<dup@x>")
	if !reg.SentCopyStored("user@example.com", "<DUP@X>") {
		t.Fatal("stored copy not matched after normalization")
	}
	if reg.SentCopyStored("other@example.com", "<dup@x>") {
		t.Fatal("matched another account")
	}
	if reg.SentCopyStored("user@example.com", "<other@x>") {
		t.Fatal("matched another message-id")
	}
	if reg.SentCopyStored("user@example.com", "") {
		t.Fatal("empty message-id matched")
	}
	reg.RecordSentCopy("user@example.com", "")
	if reg.SentCopyStored("user@example.com", "") {
		t.Fatal("empty record matched")
	}
}

func TestSentCopyExpires(t *testing.T) {
	reg, now := newSentTestRegistry(t)
	reg.RecordSentCopy("user@example.com", "<old@x>")
	*now = now.Add(sentCopyWindow + time.Second)
	if reg.SentCopyStored("user@example.com", "<old@x>") {
		t.Fatal("entry survived past the window")
	}
}

func TestSentCopyPrunesOtherEntries(t *testing.T) {
	reg, now := newSentTestRegistry(t)
	reg.RecordSentCopy("user@example.com", "<a@x>")
	*now = now.Add(sentCopyWindow + time.Second)
	reg.RecordSentCopy("user@example.com", "<b@x>")
	reg.sentMu.Lock()
	n := len(reg.sent)
	reg.sentMu.Unlock()
	if n != 1 {
		t.Fatalf("prune left %d entries, want 1", n)
	}
}

type nopLogin struct{}

func (nopLogin) Login(_ context.Context, _ string, _ Channel) (Session, error) {
	return Session{}, nil
}
func (nopLogin) Renew(_ context.Context, _ string, s Session) (Session, error) {
	return s, nil
}

type nopFactory struct{}

func (nopFactory) New(_ string, _ Session) Provider { return nil }
