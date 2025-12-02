package cache

import (
	"bytes"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		password string
	}{
		{
			name:     "simple text",
			data:     []byte("Hello, World!"),
			password: "password123",
		},
		{
			name:     "empty data",
			data:     []byte(""),
			password: "password",
		},
		{
			name:     "binary data",
			data:     []byte{0x00, 0x01, 0x02, 0xFF, 0xFE},
			password: "secret",
		},
		{
			name:     "long text",
			data:     bytes.Repeat([]byte("Lorem ipsum dolor sit amet. "), 100),
			password: "longpassword",
		},
		{
			name:     "unicode text",
			data:     []byte("Test unicode text"),
			password: "password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := encrypt(tt.data, tt.password)
			if err != nil {
				t.Fatalf("encrypt failed: %v", err)
			}

			if bytes.Equal(encrypted, tt.data) && len(tt.data) > 0 {
				t.Error("encrypted data should differ from original")
			}

			decrypted, err := decrypt(encrypted, tt.password)
			if err != nil {
				t.Fatalf("decrypt failed: %v", err)
			}

			if !bytes.Equal(decrypted, tt.data) {
				t.Errorf("decrypted data doesn't match original")
			}
		})
	}
}

func TestDecryptWithWrongPassword(t *testing.T) {
	data := []byte("Secret message")
	password := "correct"
	wrongPassword := "incorrect"

	encrypted, err := encrypt(data, password)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	_, err = decrypt(encrypted, wrongPassword)
	if err == nil {
		t.Error("expected error when decrypting with wrong password")
	}
}

func TestDecryptTooShortData(t *testing.T) {
	shortData := []byte("abc")
	_, err := decrypt(shortData, "password")
	if err == nil {
		t.Error("expected error when decrypting too short data")
	}
}

func TestCreateHash(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{
			name: "simple key",
			key:  "password",
		},
		{
			name: "empty key",
			key:  "",
		},
		{
			name: "long key",
			key:  "this is a very long password with many characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := createHash(tt.key)

			if len(hash) != 32 {
				t.Errorf("expected hash length 32, got %d", len(hash))
			}

			hash2 := createHash(tt.key)
			if !bytes.Equal(hash, hash2) {
				t.Error("same input should produce same hash")
			}
		})
	}
}

func TestEncryptDecryptDeterminism(t *testing.T) {
	data := []byte("Test data")
	password := "password"

	encrypted1, err := encrypt(data, password)
	if err != nil {
		t.Fatalf("first encrypt failed: %v", err)
	}

	encrypted2, err := encrypt(data, password)
	if err != nil {
		t.Fatalf("second encrypt failed: %v", err)
	}

	if bytes.Equal(encrypted1, encrypted2) {
		t.Error("encrypting same data twice should produce different ciphertext")
	}

	decrypted1, err := decrypt(encrypted1, password)
	if err != nil {
		t.Fatalf("first decrypt failed: %v", err)
	}

	decrypted2, err := decrypt(encrypted2, password)
	if err != nil {
		t.Fatalf("second decrypt failed: %v", err)
	}

	if !bytes.Equal(decrypted1, data) {
		t.Error("first decryption doesn't match original")
	}

	if !bytes.Equal(decrypted2, data) {
		t.Error("second decryption doesn't match original")
	}
}
