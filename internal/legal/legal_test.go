package legal

import (
	"errors"
	"strings"
	"testing"
)

var fields = Fields{
	Controller:  "Ana <b>Souza</b>",
	Contact:     "contato@example.com",
	EdgeCountry: "Brasil",
	Published:   "2026-11-20",
}

func TestRender(t *testing.T) {
	p, err := Render(fields)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "2026-11-20" {
		t.Errorf("version = %q", p.Version)
	}
	for _, name := range []string{"termos", "privacidade"} {
		d, ok := p.Doc(name)
		if !ok {
			t.Fatalf("no %s", name)
		}
		if strings.Contains(d.HTML, "{{") {
			t.Errorf("%s has a field left unfilled", name)
		}
		// A value is text, escaped, never markup.
		if !strings.Contains(d.HTML, "Ana &lt;b&gt;Souza&lt;/b&gt;") || strings.Contains(d.HTML, "<b>Souza") {
			t.Errorf("%s: the controller's name isn't escaped text", name)
		}
		if !strings.Contains(d.HTML, `<a href="mailto:contato@example.com">contato@example.com</a>`) {
			t.Errorf("%s: no mailto link", name)
		}
		if !strings.Contains(d.HTML, "20/11/2026") {
			t.Errorf("%s: no date", name)
		}
		if !strings.HasPrefix(d.HTML, "<h1>") || d.Title == "" || strings.Contains(d.Title, "\n") {
			t.Errorf("%s: title %q", name, d.Title)
		}
	}
	priv, _ := p.Doc("privacidade")
	if !strings.Contains(priv.HTML, "<table>") || !strings.Contains(priv.HTML, "Brasil") || !strings.Contains(priv.HTML, `href="/termos"`) {
		t.Error("privacidade: no table, country or link to the terms")
	}
	terms, _ := p.Doc("termos")
	if !strings.Contains(terms.HTML, `href="/privacidade"`) {
		t.Error("termos: no link to the notice")
	}
	if _, ok := p.Doc("other"); ok {
		t.Error("an unknown page exists")
	}
}

func TestRenderRefuses(t *testing.T) {
	partial := fields
	partial.EdgeCountry = ""
	if _, err := Render(partial); !errors.Is(err, ErrIncomplete) {
		t.Errorf("partial fields: %v", err)
	}
	if !(Fields{}).IsZero() || fields.IsZero() {
		t.Error("IsZero")
	}
	for _, f := range []Fields{
		{Controller: "A", Contact: "Ana <a@example.com>", EdgeCountry: "B", Published: "2026-11-20"},
		{Controller: "A", Contact: "not an address", EdgeCountry: "B", Published: "2026-11-20"},
		{Controller: "A", Contact: "a@example.com", EdgeCountry: "B", Published: "20/11/2026"},
	} {
		if _, err := Render(f); err == nil {
			t.Errorf("%+v rendered", f)
		}
	}
}
