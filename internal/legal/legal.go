// Package legal renders the beta terms and the privacy notice (#125) for
// the portal. The texts are versioned here, in Portuguese; the controller's
// name and contact, which the published pages must show but the repository
// doesn't keep, are filled in at start from the api's environment
// (OTTER_LEGAL_*, docs/legal/README.md).
package legal

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

//go:embed texts/*.md
var texts embed.FS

// docs maps each page's name, as the portal's URL has it, to its text.
var docs = map[string]string{
	"termos":      "texts/termos-beta.md",
	"privacidade": "texts/privacidade.md",
}

// Fields are the values the texts leave out. All or none must be set.
type Fields struct {
	Controller  string // OTTER_LEGAL_CONTROLLER: the controller's full name
	Contact     string // OTTER_LEGAL_CONTACT: the email for requests
	EdgeCountry string // OTTER_LEGAL_EDGE_COUNTRY: the public entry point's country
	Published   string // OTTER_LEGAL_PUBLISHED: YYYY-MM-DD, this version's date
}

// IsZero reports whether no field is set: the texts aren't published.
func (f Fields) IsZero() bool { return f == Fields{} }

// Doc is one rendered page.
type Doc struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

// Pages are the rendered texts of one version.
type Pages struct {
	// Version is the publication date, YYYY-MM-DD: what a sign-up accepts.
	Version string
	docs    map[string]Doc
}

// Doc returns the page named name.
func (p *Pages) Doc(name string) (Doc, bool) {
	d, ok := p.docs[name]
	return d, ok
}

// ErrIncomplete means some fields are set and others not.
var ErrIncomplete = errors.New("the legal texts need every OTTER_LEGAL_* variable, or none")

var field = regexp.MustCompile(`\{\{[a-z_]+\}\}`)

// Render renders every text with f's values, escaped. A field left unset,
// an invalid one, or a {{field}} the texts use that Render doesn't know is
// an error: a page never goes out with a blank where the controller's
// name belongs.
func Render(f Fields) (*Pages, error) {
	if f.Controller == "" || f.Contact == "" || f.EdgeCountry == "" || f.Published == "" {
		return nil, ErrIncomplete
	}
	if a, err := mail.ParseAddress(f.Contact); err != nil || a.Address != f.Contact {
		return nil, fmt.Errorf("OTTER_LEGAL_CONTACT %q is not a plain email address", f.Contact)
	}
	date, err := time.Parse(time.DateOnly, f.Published)
	if err != nil {
		return nil, fmt.Errorf("OTTER_LEGAL_PUBLISHED %q is not a date (YYYY-MM-DD)", f.Published)
	}
	contact := html.EscapeString(f.Contact)
	values := map[string]string{
		"{{controlador}}":     html.EscapeString(f.Controller),
		"{{contato}}":         `<a href="mailto:` + contact + `">` + contact + `</a>`,
		"{{pais_entrada}}":    html.EscapeString(f.EdgeCountry),
		"{{data_publicacao}}": date.Format("02/01/2006"),
	}
	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	p := &Pages{Version: f.Published, docs: map[string]Doc{}}
	for name, path := range docs {
		src, err := texts.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := md.Convert(src, &buf); err != nil {
			return nil, fmt.Errorf("render %s: %w", path, err)
		}
		// Filled in after rendering, so a value is HTML-escaped text and
		// never Markdown.
		out := field.ReplaceAllStringFunc(buf.String(), func(k string) string {
			if v, ok := values[k]; ok {
				return v
			}
			err = fmt.Errorf("%s uses %s, which has no value", path, k)
			return k
		})
		if err != nil {
			return nil, err
		}
		title, _, _ := strings.Cut(strings.TrimPrefix(string(src), "# "), "\n")
		p.docs[name] = Doc{Name: name, Title: title, HTML: out}
	}
	return p, nil
}
