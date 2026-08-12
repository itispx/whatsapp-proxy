package snippet

import (
	"strings"
	"testing"
)

func TestExtract_Text(t *testing.T) {
	body := []byte(`{"messaging_product":"whatsapp","to":"5511999999999","type":"text","text":{"body":"Your order has shipped"}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if e.To != "5511999999999" {
		t.Fatalf("expected to=5511999999999, got %q", e.To)
	}
	if e.Snippet != "Your order has shipped" {
		t.Fatalf("expected snippet, got %q", e.Snippet)
	}
}

func TestExtract_Template(t *testing.T) {
	body := []byte(`{"to":"5511999999999","type":"template","template":{"name":"order_shipped","components":[{"type":"body","parameters":[{"type":"text","text":"1234"},{"type":"text","text":"Friday"}]}]}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := "order_shipped: 1234, Friday"
	if e.Snippet != want {
		t.Fatalf("expected %q, got %q", want, e.Snippet)
	}
}

func TestExtract_TemplateNoParameters(t *testing.T) {
	body := []byte(`{"to":"5511999999999","type":"template","template":{"name":"opt_in"}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if e.Snippet != "opt_in" {
		t.Fatalf("expected template name only, got %q", e.Snippet)
	}
}

func TestExtract_Interactive(t *testing.T) {
	body := []byte(`{"to":"5511999999999","type":"interactive","interactive":{"body":{"text":"Pick a slot"},"action":{"buttons":[{"reply":{"title":"Friday"}},{"reply":{"title":"Saturday"}}]}}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := "Pick a slot | Friday | Saturday"
	if e.Snippet != want {
		t.Fatalf("expected %q, got %q", want, e.Snippet)
	}
}

func TestExtract_InteractiveRows(t *testing.T) {
	body := []byte(`{"to":"5511999999999","type":"interactive","interactive":{"body":{"text":"Choose"},"action":{"sections":[{"rows":[{"title":"Option A"},{"title":"Option B"}]}]}}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := "Choose | Option A | Option B"
	if e.Snippet != want {
		t.Fatalf("expected %q, got %q", want, e.Snippet)
	}
}

func TestExtract_OtherTypeUsesTypeString(t *testing.T) {
	body := []byte(`{"to":"5511999999999","type":"image","image":{"id":"123"}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if e.Snippet != "image" {
		t.Fatalf("expected snippet 'image', got %q", e.Snippet)
	}
}

func TestExtract_TruncatesAt200Chars(t *testing.T) {
	long := strings.Repeat("a", 250)
	body := []byte(`{"to":"5511999999999","type":"text","text":{"body":"` + long + `"}}`)
	e, ok := Extract(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len([]rune(e.Snippet)) != 200 {
		t.Fatalf("expected 200 chars, got %d", len([]rune(e.Snippet)))
	}
}

func TestExtract_MissingRecipientSkips(t *testing.T) {
	body := []byte(`{"type":"text","text":{"body":"no recipient"}}`)
	_, ok := Extract(body)
	if ok {
		t.Fatal("expected ok=false when 'to' is missing")
	}
}

func TestExtract_InvalidJSONSkips(t *testing.T) {
	_, ok := Extract([]byte(`not json`))
	if ok {
		t.Fatal("expected ok=false for invalid JSON")
	}
}
