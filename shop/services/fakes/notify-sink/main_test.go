package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInboxCaptureQueryClear exercises the queryable-inbox contract: /send
// captures, /inbox?recipient filters in insertion order, DELETE clears.
func TestInboxCaptureQueryClear(t *testing.T) {
	srv := httptest.NewServer(NewMux(newSink()))
	defer srv.Close()

	send := func(channel, recipient, subject string) int {
		b, _ := json.Marshal(map[string]string{"channel": channel, "recipient": recipient, "subject": subject})
		resp, err := http.Post(srv.URL+"/v1/send", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	inbox := func(recipient string) (int, []map[string]any) {
		resp, err := http.Get(srv.URL + "/v1/inbox?recipient=" + recipient)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Count    int              `json:"count"`
			Messages []map[string]any `json:"messages"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out.Count, out.Messages
	}

	if s := send("PUSH", "usr_a", "one"); s != 200 {
		t.Fatalf("send status %d", s)
	}
	send("SMS", "usr_a", "two")
	send("EMAIL", "usr_b", "three")

	// bad channel -> 400 error envelope.
	if s := send("TELEPATHY", "usr_a", "nope"); s != 400 {
		t.Fatalf("invalid channel status %d want 400", s)
	}

	count, msgs := inbox("usr_a")
	if count != 2 {
		t.Fatalf("usr_a inbox count %d want 2", count)
	}
	if msgs[0]["subject"] != "one" || msgs[1]["subject"] != "two" {
		t.Fatalf("insertion order wrong: %v", msgs)
	}

	// DELETE only usr_a.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/inbox?recipient=usr_a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cr struct {
		Cleared int `json:"cleared"`
	}
	json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if cr.Cleared != 2 {
		t.Fatalf("cleared %d want 2", cr.Cleared)
	}
	if c, _ := inbox("usr_a"); c != 0 {
		t.Fatalf("usr_a not cleared: %d", c)
	}
	if c, _ := inbox("usr_b"); c != 1 {
		t.Fatalf("usr_b should be untouched: %d", c)
	}
	t.Logf("inbox capture/query/clear OK (insertion-ordered, per-recipient clear)")
}
