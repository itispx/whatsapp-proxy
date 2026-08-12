package handler

import (
	"net/http"
	"testing"
	"time"
)

func TestParseSendHeaders_Defaults(t *testing.T) {
	meta, err := ParseSendHeaders(http.Header{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.ExpectsReply {
		t.Fatal("expected ExpectsReply false by default")
	}
	if meta.Topic != "" {
		t.Fatalf("expected empty topic by default, got %q", meta.Topic)
	}
	if meta.ReplyTTL != defaultReplyTTL {
		t.Fatalf("expected default reply TTL %v, got %v", defaultReplyTTL, meta.ReplyTTL)
	}
}

func TestParseSendHeaders_ExpectsReplyCaseInsensitive(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"false", false},
		{"False", false},
		{"FALSE", false},
		{"0", false},
	}
	for _, c := range cases {
		h := http.Header{}
		h.Set("X-Proxy-Expects-Reply", c.value)
		meta, err := ParseSendHeaders(h)
		if err != nil {
			t.Fatalf("value %q: unexpected error: %v", c.value, err)
		}
		if meta.ExpectsReply != c.want {
			t.Fatalf("value %q: expected ExpectsReply=%v, got %v", c.value, c.want, meta.ExpectsReply)
		}
	}
}

func TestParseSendHeaders_ExpectsReplyInvalid(t *testing.T) {
	h := http.Header{}
	h.Set("X-Proxy-Expects-Reply", "maybe")
	_, err := ParseSendHeaders(h)
	if err == nil {
		t.Fatal("expected error for invalid X-Proxy-Expects-Reply value")
	}
}

func TestParseSendHeaders_Topic(t *testing.T) {
	h := http.Header{}
	h.Set("X-Proxy-Topic", "order-1234-shipping")
	meta, err := ParseSendHeaders(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Topic != "order-1234-shipping" {
		t.Fatalf("expected topic passthrough, got %q", meta.Topic)
	}
}

func TestParseSendHeaders_ReplyTTLCustom(t *testing.T) {
	h := http.Header{}
	h.Set("X-Proxy-Reply-TTL", "3600")
	meta, err := ParseSendHeaders(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.ReplyTTL != time.Hour {
		t.Fatalf("expected 1h reply TTL, got %v", meta.ReplyTTL)
	}
}

func TestParseSendHeaders_ReplyTTLCappedAt48h(t *testing.T) {
	h := http.Header{}
	h.Set("X-Proxy-Reply-TTL", "999999")
	meta, err := ParseSendHeaders(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.ReplyTTL != maxReplyTTL {
		t.Fatalf("expected reply TTL capped at %v, got %v", maxReplyTTL, meta.ReplyTTL)
	}
}

func TestParseSendHeaders_ReplyTTLInvalid(t *testing.T) {
	for _, bad := range []string{"not-a-number", "0", "-5", "1.5"} {
		h := http.Header{}
		h.Set("X-Proxy-Reply-TTL", bad)
		_, err := ParseSendHeaders(h)
		if err == nil {
			t.Fatalf("expected error for X-Proxy-Reply-TTL=%q", bad)
		}
	}
}
