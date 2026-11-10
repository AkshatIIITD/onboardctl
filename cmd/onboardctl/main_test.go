package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AkshatIIITD/onboardctl/internal/onboard"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HARBOR_URL", "HARBOR_USERNAME", "HARBOR_PASSWORD", "KEYCLOAK_URL", "KEYCLOAK_REALM",
		"KEYCLOAK_TOKEN_REALM", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET", "ONBOARDCTL_CA_FILE", "HARBOR_REGISTRY",
	} {
		t.Setenv(k, "")
	}
}

func TestRun_DryRunNeedsNoCredentials(t *testing.T) {
	clearEnv(t)
	var out bytes.Buffer
	if err := run([]string{"-name", "new-project", "-dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[1/14]", "/harbor/project/new-project/admin", "/harbor/project/new-project/guest", "dry-run", "robot", "pull-push", "oc secrets link default harbor-pull", "ci-cd"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRun_ReportsAllMissingSettingsTogether(t *testing.T) {
	clearEnv(t)
	err := run([]string{"-name", "new-project"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"HARBOR_URL", "HARBOR_PASSWORD", "KEYCLOAK_URL", "KEYCLOAK_REALM", "KEYCLOAK_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestRun_RejectsBadInput(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{
		{"-dry-run"},                      // no name
		{"-name", "Bad_Name", "-dry-run"}, // invalid name
		{"-name", "new-project", "-openshift-mode", "x", "-dry-run"},
		{"-name", "new-project", "-group-root", "no-slash", "-dry-run"},
	} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("args %v: expected an error", args)
		}
	}
}

func TestRun_HelpIsNotAFailure(t *testing.T) {
	clearEnv(t)
	err := run([]string{"-h"}, &bytes.Buffer{})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("err = %v, want flag.ErrHelp", err)
	}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"https://harbor.example.com":      "harbor.example.com",
		"https://harbor.example.com:8443": "harbor.example.com:8443",
		"https://harbor.example.com/":     "harbor.example.com",
	}
	for in, want := range cases {
		got, err := registryHost(in)
		if err != nil || got != want {
			t.Errorf("registryHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "harbor.example.com", "::"} {
		if _, err := registryHost(bad); err == nil {
			t.Errorf("registryHost(%q): expected an error", bad)
		}
	}
}

func someSecrets() *onboard.Secrets {
	s := &onboard.Secrets{}
	s.Add("Harbor robot username", "robot$new-project+pull-push")
	s.Add("Harbor robot secret", "TOPSECRET")
	return s
}

func TestEmitSecrets_PrintsToStdoutByDefault(t *testing.T) {
	var out bytes.Buffer
	emitSecrets(&out, someSecrets(), nil)
	for _, want := range []string{"robot$new-project+pull-push", "TOPSECRET", "will not show"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestEmitSecrets_WritesFileWithoutPrintingSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.txt")
	f, err := openSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	emitSecrets(&out, someSecrets(), f)

	if strings.Contains(out.String(), "TOPSECRET") {
		t.Errorf("secret must not be printed when saved to a file:\n%s", out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "Harbor robot secret: TOPSECRET") {
		t.Fatalf("file content = %q, err %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestEmitSecrets_NothingCollectedRemovesEmptyFileAndPrintsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.txt")
	f, err := openSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	emitSecrets(&out, &onboard.Secrets{}, f)
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty secrets file should be removed, stat err = %v", err)
	}
}

func TestEmitSecrets_FallsBackToStdoutWhenFileWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.txt")
	f, err := openSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // writing to a closed file fails
	var out bytes.Buffer
	emitSecrets(&out, someSecrets(), f)
	if !strings.Contains(out.String(), "TOPSECRET") || !strings.Contains(out.String(), "WARNING") {
		t.Errorf("expected a warning and the secrets on stdout:\n%s", out.String())
	}
}

func TestOpenSecretsFile_RefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSecretsFile(path); err == nil {
		t.Fatal("expected an error for an existing file")
	}
}
