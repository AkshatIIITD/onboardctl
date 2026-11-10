package httpx

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewClient_Defaults(t *testing.T) {
	c, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("default timeout = %v, want 30s", c.Timeout)
	}
}

func TestNewClient_CustomTimeout(t *testing.T) {
	c, err := NewClient(Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", c.Timeout)
	}
}

func TestNewClient_MissingCAFile(t *testing.T) {
	if _, err := NewClient(Options{CAFile: filepath.Join(t.TempDir(), "nope.pem")}); err == nil {
		t.Fatal("expected an error for a missing CA file")
	}
}

func TestNewClient_InvalidCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(Options{CAFile: path}); err == nil {
		t.Fatal("expected an error for a CA file with no certificates")
	}
}
