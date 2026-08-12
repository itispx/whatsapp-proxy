// Package snippet extracts recipient and content-context evidence from
// outbound Meta message bodies, for attribution purposes only. It parses a
// copy of the body — callers must never let this influence what is
// forwarded to Meta.
package snippet

import (
	"encoding/json"
	"strings"
)

const maxLen = 200

// Extracted holds the evidence pulled from an outbound message body.
type Extracted struct {
	To      string
	Type    string
	Snippet string
}

type outboundBody struct {
	To   string `json:"to"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Template struct {
		Name       string `json:"name"`
		Components []struct {
			Type       string `json:"type"`
			Parameters []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parameters"`
		} `json:"components"`
	} `json:"template"`
	Interactive struct {
		Body struct {
			Text string `json:"text"`
		} `json:"body"`
		Action struct {
			Buttons []struct {
				Reply struct {
					Title string `json:"title"`
				} `json:"reply"`
			} `json:"buttons"`
			Sections []struct {
				Rows []struct {
					Title string `json:"title"`
				} `json:"rows"`
			} `json:"sections"`
		} `json:"action"`
	} `json:"interactive"`
}

// Extract parses a copy of an outbound Meta message body and derives the
// recipient and a content snippet for attribution context. ok is false when
// the body has no usable recipient — callers should skip registry writes
// but still forward the message to Meta unmodified.
func Extract(body []byte) (Extracted, bool) {
	var b outboundBody
	if err := json.Unmarshal(body, &b); err != nil || b.To == "" {
		return Extracted{}, false
	}

	var snip string
	switch b.Type {
	case "text":
		snip = b.Text.Body
	case "template":
		snip = templateSnippet(b)
	case "interactive":
		snip = interactiveSnippet(b)
	default:
		snip = b.Type
	}

	return Extracted{To: b.To, Type: b.Type, Snippet: truncate(snip)}, true
}

func templateSnippet(b outboundBody) string {
	var params []string
	for _, comp := range b.Template.Components {
		for _, p := range comp.Parameters {
			if p.Text != "" {
				params = append(params, p.Text)
			}
		}
	}
	if len(params) == 0 {
		return b.Template.Name
	}
	return b.Template.Name + ": " + strings.Join(params, ", ")
}

func interactiveSnippet(b outboundBody) string {
	var parts []string
	if b.Interactive.Body.Text != "" {
		parts = append(parts, b.Interactive.Body.Text)
	}
	for _, btn := range b.Interactive.Action.Buttons {
		if btn.Reply.Title != "" {
			parts = append(parts, btn.Reply.Title)
		}
	}
	for _, sec := range b.Interactive.Action.Sections {
		for _, row := range sec.Rows {
			if row.Title != "" {
				parts = append(parts, row.Title)
			}
		}
	}
	return strings.Join(parts, " | ")
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}
