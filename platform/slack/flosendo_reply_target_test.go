package slack

import "testing"

// EDEN-1618: after a restart, a queued message's "Not processed" notice must land in that message's own thread. The
// key ReplyTargetKey writes is one ReconstructReplyCtx reads back to the same thread.
func TestReplyTargetKey_RoundTripsToTheThread(t *testing.T) {
	p := &Platform{}
	cases := []struct {
		name string
		rc   replyContext
		key  string
		ts   string
	}{
		{"a reply in a thread", replyContext{channel: "C1", timestamp: "111.1", messageTS: "222.2"}, "slack:C1:t:111.1", "111.1"},
		{"a top-level message: its own ts starts the thread", replyContext{channel: "C1", messageTS: "222.2"}, "slack:C1:t:222.2", "222.2"},
		{"no ts at all: the channel", replyContext{channel: "C1"}, "slack:C1", ""},
	}
	for _, c := range cases {
		key, ok := p.ReplyTargetKey(c.rc)
		if !ok || key != c.key {
			t.Fatalf("%s: ReplyTargetKey = %q, %v; want %q", c.name, key, ok, c.key)
		}
		back, err := p.ReconstructReplyCtx(key)
		if err != nil {
			t.Fatalf("%s: ReconstructReplyCtx(%q): %v", c.name, key, err)
		}
		if got := back.(replyContext); got.channel != "C1" || got.timestamp != c.ts {
			t.Fatalf("%s: reconstructed %+v, want channel C1 and thread %q", c.name, got, c.ts)
		}
	}
	if _, ok := p.ReplyTargetKey("not a slack context"); ok {
		t.Fatal("a reply context that is not Slack's has no target")
	}
}
