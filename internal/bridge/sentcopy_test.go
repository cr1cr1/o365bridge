package bridge

import "testing"

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
