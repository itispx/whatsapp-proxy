package registry

import (
	"testing"

	"github.com/itispx/whatsapp-proxy/attribution"
)

func TestCandidateFromHash_EmptySkipped(t *testing.T) {
	_, ok := candidateFromHash("wamid.1", map[string]string{})
	if ok {
		t.Fatal("expected ok=false for an empty (expired) hash")
	}
}

func TestCandidateFromHash_Parses(t *testing.T) {
	c, ok := candidateFromHash("wamid.1", map[string]string{
		"app_id":        "app-a",
		"expects_reply": "1",
		"topic":         "order-1234",
		"snippet":       "Your order shipped",
		"sent_at":       "1700000000",
	})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.AppID != "app-a" || c.MessageID != "wamid.1" || !c.ExpectsReply ||
		c.Topic != "order-1234" || c.Snippet != "Your order shipped" || c.SentAt != 1700000000 {
		t.Fatalf("unexpected candidate: %+v", c)
	}
}

func TestSortCandidates_FlaggedFirstThenNewest(t *testing.T) {
	c := []attribution.Candidate{
		{AppID: "unflagged-new", ExpectsReply: false, SentAt: 300},
		{AppID: "flagged-old", ExpectsReply: true, SentAt: 100},
		{AppID: "flagged-new", ExpectsReply: true, SentAt: 200},
		{AppID: "unflagged-old", ExpectsReply: false, SentAt: 50},
	}

	sortCandidates(c)

	want := []string{"flagged-new", "flagged-old", "unflagged-new", "unflagged-old"}
	for i, appID := range want {
		if c[i].AppID != appID {
			t.Fatalf("position %d: expected %q, got %q", i, appID, c[i].AppID)
		}
	}
}
