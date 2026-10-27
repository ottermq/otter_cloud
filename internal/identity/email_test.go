package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ana@example.com":            "ana@example.com",
		"  Ana.Souza@Example.COM \n": "ana.souza@example.com",
		"ana+otter@mail.example.br":  "ana+otter@mail.example.br",
	} {
		got, err := NormalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"",
		"   ",
		"ana",
		"ana@",
		"@example.com",
		"ana@localhost",
		"Ana <ana@example.com>",
		"ana@example.com, bob@example.com",
		"ana@@example.com",
		strings.Repeat("a", 250) + "@example.com",
	} {
		if got, err := NormalizeEmail(in); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrInvalidEmail", in, got, err)
		}
	}
}
