package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	ctx := context.Background()
	h, err := HashPassword(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash %q does not use the documented parameters", h)
	}
	if NeedsRehash(h) {
		t.Error("a fresh hash needs rehashing")
	}

	ok, err := VerifyPassword(ctx, "correct horse battery staple", h)
	if err != nil || !ok {
		t.Errorf("right password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(ctx, "correct horse battery stapler", h)
	if err != nil || ok {
		t.Errorf("wrong password: ok=%v err=%v", ok, err)
	}

	h2, err := HashPassword(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h {
		t.Error("two hashes of one password are equal: the salt is not random")
	}
}

func TestVerifyOtherParameters(t *testing.T) {
	// Made with m=8192,t=1,p=1; hashes from older parameters must still verify.
	salt := make([]byte, 16)
	h := encodeForTest(t, "an older password", salt, 8*1024, 1, 1)
	ok, err := VerifyPassword(context.Background(), "an older password", h)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !NeedsRehash(h) {
		t.Error("a hash with old parameters does not need rehashing")
	}
}

func TestVerifyMalformed(t *testing.T) {
	good, err := HashPassword(context.Background(), "a fine long password")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for name, h := range map[string]string{
		"empty":         "",
		"bcrypt":        "$2a$10$abcdefghijklmnopqrstuuJ3Q5d1Z0kq3x7d8ZpQ1Y6l1m2n3o4p5",
		"argon2i":       with(1, "argon2i"),
		"old version":   with(2, "v=16"),
		"huge memory":   with(3, "m=4194304,t=3,p=4"),
		"zero time":     with(3, "m=65536,t=0,p=4"),
		"many threads":  with(3, "m=65536,t=3,p=255"),
		"bad params":    with(3, "m=65536"),
		"bad salt":      with(4, "!!!"),
		"short salt":    with(4, "AAAA"),
		"short key":     with(5, "AAAA"),
		"extra field":   good + "$x",
		"missing field": strings.Join(parts[:5], "$"),
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(context.Background(), "a fine long password", h)
			if ok || err == nil {
				t.Errorf("ok=%v err=%v, want an error", ok, err)
			}
			if !NeedsRehash(h) {
				t.Error("a malformed hash does not need rehashing")
			}
		})
	}
}

func TestVerifyCancelled(t *testing.T) {
	good, err := HashPassword(context.Background(), "a fine long password")
	if err != nil {
		t.Fatal(err)
	}
	// Fill every slot, so a cancelled caller must give up rather than wait.
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyPassword(ctx, "a fine long password", good); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := HashPassword(ctx, "a fine long password"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCheckPassword(t *testing.T) {
	for _, tc := range []struct {
		password, email string
		want            error
	}{
		{"correct horse battery staple", "ana@example.com", nil},
		{"Tr0ub4dor&3xy", "", nil},
		{"senhasegura1", "", nil},
		{"çãõéàüñßøåæ€", "", nil}, // 12 characters, more bytes
		{"", "", ErrPasswordTooShort},
		{"elevenchars", "", ErrPasswordTooShort},
		{"çãõéàüñßøåæ", "", ErrPasswordTooShort}, // 11 characters, 22 bytes
		{strings.Repeat("ab", 513), "", ErrPasswordTooLong},
		{"aaaaaaaaaaaaaaaa", "", ErrPasswordCommon},
		{"€€€€€€€€€€€€", "", ErrPasswordCommon},
		{"qwertyuiop123", "", ErrPasswordCommon},
		{"QwertyUiop123", "", ErrPasswordCommon}, // listed entries match any case
		{"password1234", "", ErrPasswordCommon},
		{"ana.souza@example.com", "Ana.Souza@example.com", ErrPasswordIsEmail},
	} {
		if got := CheckPassword(tc.password, tc.email); !errors.Is(got, tc.want) {
			t.Errorf("CheckPassword(%q, %q) = %v, want %v", tc.password, tc.email, got, tc.want)
		}
	}
}

func TestCommonPasswordsList(t *testing.T) {
	if n := len(commonPasswords); n < 1000 {
		t.Fatalf("only %d common passwords loaded", n)
	}
	for p := range commonPasswords {
		if p != strings.ToLower(p) || len([]rune(p)) < MinPasswordLength {
			t.Errorf("entry %q is not lowercase or is shorter than %d characters", p, MinPasswordLength)
		}
	}
}
