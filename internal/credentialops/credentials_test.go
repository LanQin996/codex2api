package credentialops

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	rows, err := Parse("\ufeff Test@EXAMPLE.com---- $synthetic----password ----jbswy3dpehpk3pxp  \r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Email != "test@example.com" || rows[0].Password != " $synthetic----password " || rows[0].TOTP != "JBSWY3DPEHPK3PXP" {
		t.Fatal("normalization changed credentials")
	}
}
func TestParseRejectsWithoutEchoingSecrets(t *testing.T) {
	for _, raw := range []string{"", "person@example.com----synthetic-private-value", "person@example.com----synthetic-private-value----123456", "Name <a@b.com>----synthetic-private-value----JBSWY3DPEHPK3PXP", "a@b.com----synthetic-private-value----JBSWY3DPEHPK3PXP\na@b.com----another----JBSWY3DPEHPK3PXP"} {
		_, err := Parse(raw)
		if err == nil {
			t.Fatal("accepted invalid input")
		}
		if strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("secret in error")
		}
	}
}
func TestCipherBinding(t *testing.T) {
	a, err := NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	value := Login{Email: "test@example.com", Password: "synthetic", TOTP: "JBSWY3DPEHPK3PXP"}
	sealed, err := Seal(a, "record", "login", value)
	if err != nil {
		t.Fatal(err)
	}
	var got Login
	if err := Open(a, "record", "login", sealed, &got); err != nil || got != value {
		t.Fatal("roundtrip failed")
	}
	if Open(a, "other", "login", sealed, &got) == nil || Open(a, "record", "result", sealed, &got) == nil {
		t.Fatal("ciphertext swap accepted")
	}
	other, _ := Seal(a, "record", "login", value)
	if other == sealed {
		t.Fatal("nonce reused")
	}
	if _, err := NewCipher("bad"); err == nil {
		t.Fatal("bad key accepted")
	}
}
