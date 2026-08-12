package config

import "testing"

func TestApp_SnippetsEnabled_DefaultsTrue(t *testing.T) {
	a := App{}
	if !a.SnippetsEnabled() {
		t.Fatal("expected snippets enabled by default when store_snippets is unset")
	}
}

func TestApp_SnippetsEnabled_ExplicitFalse(t *testing.T) {
	f := false
	a := App{StoreSnippets: &f}
	if a.SnippetsEnabled() {
		t.Fatal("expected snippets disabled when store_snippets is explicitly false")
	}
}

func TestApp_SnippetsEnabled_ExplicitTrue(t *testing.T) {
	tr := true
	a := App{StoreSnippets: &tr}
	if !a.SnippetsEnabled() {
		t.Fatal("expected snippets enabled when store_snippets is explicitly true")
	}
}
