package bridge

import "testing"

func TestPlainTaskNotes(t *testing.T) {
	for _, tc := range []struct{ kind, in, want string }{
		{"text", "literal <tag> & text\nSecond line", "literal <tag> & text\nSecond line"},
		{"html", "<p>First &amp; &lt;tag&gt;</p><p>Second<br>第三行</p>", "First & <tag>\nSecond\n第三行"},
		{"HTML", `<div title="a > b">A</div><!-- comment --><style>hidden</style><script>hidden()</script><div>B</div>`, "A\nB"},
		{"html", "<p><br></p>", ""},
		{"html", "<p>1 < 2 and &#128512;</p>", "1 < 2 and 😀"},
		{"html", "<div>Unclosed text", "Unclosed text"},
		{"html", `<script>var s="</scriptx>"; secret()</script><p>Notes</p>`, "Notes"},
		{"html", `<p>Kİ</p><SCRIPT>K hidden</SCRIPT><p>Notes</p>`, "Kİ\nNotes"},
	} {
		if got := PlainTextBody(tc.in, tc.kind); got != tc.want {
			t.Errorf("kind=%s in=%q got=%q want=%q", tc.kind, tc.in, got, tc.want)
		}
	}
	plain := "literal <script> & \"text\"\n第二行"
	encoded := TaskNoteHTML(plain)
	if got := PlainTextBody(encoded, "html"); got != plain {
		t.Fatalf("plain note HTML roundtrip=%q want=%q", got, plain)
	}
}
