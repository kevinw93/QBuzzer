package secret

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h := HashPassword("hunter22", Fast)
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q", h)
	}
	if h == HashPassword("hunter22", Fast) {
		t.Error("two hashes of the same password are equal: salt not random")
	}
	for pw, want := range map[string]bool{"hunter22": true, "hunter23": false, "": false} {
		if ok, err := VerifyPassword(pw, h); err != nil || ok != want {
			t.Errorf("Verify(%q) = %v, %v", pw, ok, err)
		}
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=18$m=64,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=19$m=64,t=1,p=1$!!$a2V5", "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$"} {
		if _, err := VerifyPassword("x", h); err == nil {
			t.Errorf("accepted %q", h)
		}
	}
}

func TestHashToken(t *testing.T) {
	if a, b := HashToken("tok"), HashToken("tok"); a != b || len(a) != 64 || a == HashToken("tok2") {
		t.Errorf("hashes %q %q", a, b)
	}
}
