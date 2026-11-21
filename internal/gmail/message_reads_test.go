package gmail

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeGmail map[string]string

func (f fakeGmail) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := f[strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")]
	code := 200
	if !ok {
		code, body = 404, "{}"
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

// The connector's message reads (single, thread, reply) through the HTTP
// interface, pinned on the output the callers see.
func TestMessageReadsPinned(t *testing.T) {
	long := strings.Repeat("x", 520)
	m1 := `{"id":"m1","threadId":"t1","internalDate":"1600000000000","snippet":"s1","labelIds":["INBOX","UNREAD"],
	"payload":{"headers":[{"name":"From","value":"A <a@example.com>"},{"name":"Subject","value":"first"},{"name":"Subject","value":"second"},
	{"name":"To","value":"b@example.com"},{"name":"Date","value":"Mon, 1 Jan"},{"name":"Message-Id","value":"<id1>"},{"name":"Message-ID","value":"<id2>"},{"name":"References","value":"<r>"}],
	"parts":[{"mimeType":"text/html","body":{"data":"` + b64("<p>h</p>") + `"}},{"mimeType":"text/plain","body":{"data":""}},{"mimeType":"text/plain","body":{"data":"` + b64(long) + `"}}]}}`
	m2 := `{"id":"m2","threadId":"t1","internalDate":"1500000000000","snippet":"s2","payload":{"headers":[{"name":"From","value":"c@example.com"}],"body":{"data":"` + b64("plain body") + `"}}}`
	loc := time.Local
	time.Local = time.UTC
	defer func() { time.Local = loc }()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = fakeGmail{"messages/m1": m1, "messages/m2": m2, "threads/t1": `{"id":"t1","messages":[` + m1 + `,` + m2 + `]}`}
	defer func() { http.DefaultClient.Transport = old }()
	g := &GmailConnector{accessToken: "tok", expiresAt: time.Now().Add(time.Hour)}

	var out []interface{}
	e1, err := g.GetMessage("m1")
	out = append(out, e1, err)
	e2, err := g.GetMessage("m2")
	out = append(out, e2, err)
	th, err := g.FetchThread("t1")
	out = append(out, th, err)
	f, tid, mid, refs, err := g.GetFullMessage("m1")
	out = append(out, f, tid, mid, refs, err)
	f2, tid2, mid2, refs2, err := g.GetFullMessage("m2")
	out = append(out, f2, tid2, mid2, refs2, err)
	got, _ := json.Marshal(out)
	t.Logf("PIN %s", got)
	if want := pinnedMessageReads; string(got) != want {
		t.Fatalf("message reads drifted:\n got %s\nwant %s", got, want)
	}
}

// pinnedMessageReads was recorded from the connector before its message
// decoding was consolidated.
const pinnedMessageReads = `[{"ID":"m1","From":"A \u003ca@example.com\u003e","To":"b@example.com","Subject":"second","Date":"Sep 13","Snippet":"s1","Body":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...","Labels":["INBOX","UNREAD"]},null,{"ID":"m2","From":"c@example.com","To":"","Subject":"","Date":"Jul 14","Snippet":"s2","Body":"plain body","Labels":null},null,[{"ID":"m2","From":"c@example.com","To":"","Subject":"","Date":"2017-07-14 02:40","Snippet":"s2","Body":"plain body","Labels":null},{"ID":"m1","From":"A \u003ca@example.com\u003e","To":"b@example.com","Subject":"second","Date":"2020-09-13 12:26","Snippet":"s1","Body":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...","Labels":["INBOX","UNREAD"]}],null,{"ID":"m1","From":"A \u003ca@example.com\u003e","To":"b@example.com","Subject":"second","Date":"Mon, 1 Jan","Snippet":"s1","Body":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","Labels":null},"t1","\u003cid2\u003e","\u003cr\u003e",null,{"ID":"m2","From":"c@example.com","To":"","Subject":"","Date":"","Snippet":"s2","Body":"plain body","Labels":null},"t1","","",null]`
