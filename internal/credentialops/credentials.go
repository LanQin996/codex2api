// Package credentialops handles login secrets separately from exportable account tokens.
package credentialops

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

type Login struct {
	ProxyURL string `json:"proxy_url,omitempty"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTP     string `json:"totp_secret"`
}

// Parse accepts one email----password----TOTP per line. The password is never
// trimmed or interpolated into a command. Splitting at the outer delimiters
// also preserves passwords containing ----. Errors never include input text.
func Parse(raw string) ([]Login, error) {
	if len(raw) > 256*1024 {
		return nil, errors.New("input exceeds 256 KiB")
	}
	var out []Login
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		first, last := strings.Index(line, "----"), strings.LastIndex(line, "----")
		if first < 1 || last < first+4 {
			return nil, fmt.Errorf("line %d: expected email----password----TOTP", i+1)
		}
		v := Login{Email: strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line[:first], "\ufeff"))), Password: line[first+4 : last], TOTP: strings.ToUpper(strings.TrimSpace(line[last+4:]))}
		addr, err := mail.ParseAddress(v.Email)
		if err != nil || addr.Address != v.Email || !strings.Contains(v.Email, "@") || len(v.Email) > 254 || v.Password == "" || len(v.Password) > 4096 || strings.ContainsAny(v.Password, "\r\n\x00") {
			return nil, fmt.Errorf("line %d: invalid email or password", i+1)
		}
		v.TOTP = strings.TrimRight(v.TOTP, "=")
		key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(v.TOTP)
		if err != nil || len(key) < 10 || len(key) > 128 {
			return nil, fmt.Errorf("line %d: invalid TOTP base32 secret", i+1)
		}
		if seen[v.Email] {
			return nil, fmt.Errorf("line %d: duplicate email", i+1)
		}
		seen[v.Email] = true
		out = append(out, v)
		if len(out) > 100 {
			return nil, errors.New("at most 100 accounts per import")
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no login entries")
	}
	return out, nil
}

func NewCipher(encodedKey string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(key) != 32 {
		return nil, errors.New("CREDENTIAL_OPS_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Purpose and record ID are authenticated to prevent ciphertext swapping.
func Seal(a cipher.AEAD, id, purpose string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer clear(data)
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := a.Seal(nonce, nonce, data, []byte("credential-ops-v1/"+id+"/"+purpose))
	return base64.StdEncoding.EncodeToString(sealed), nil
}
func Open(a cipher.AEAD, id, purpose, encoded string, value any) error {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) < a.NonceSize()+a.Overhead() {
		return errors.New("invalid encrypted credential")
	}
	plain, err := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], []byte("credential-ops-v1/"+id+"/"+purpose))
	if err != nil {
		return errors.New("credential decryption failed")
	}
	defer clear(plain)
	if json.Unmarshal(plain, value) != nil {
		return errors.New("invalid credential payload")
	}
	return nil
}
