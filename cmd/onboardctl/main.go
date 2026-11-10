// onboardctl sets up a new project on the platform in one idempotent run:
// an OpenShift namespace, a private Harbor project with the same name, and four
// role groups (admin, maintainer, developer, guest) created in Keycloak and
// granted access in Harbor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AkshatIIITD/onboardctl/internal/harbor"
	"github.com/AkshatIIITD/onboardctl/internal/httpx"
	"github.com/AkshatIIITD/onboardctl/internal/keycloak"
	"github.com/AkshatIIITD/onboardctl/internal/onboard"
	"github.com/AkshatIIITD/onboardctl/internal/openshift"
)

func main() {
	err := run(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return // -h / -help is not a failure
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run holds the real logic so main() stays tiny and run() is easy to test.
func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("onboardctl", flag.ContinueOnError)

	var (
		name      = fs.String("name", "", "project name; used for the OpenShift namespace, the Harbor project AND the group paths (required)")
		groupRoot = fs.String("group-root", "/harbor/project", "group path prefix; groups become <group-root>/<name>/{admin,maintainer,developer,guest}")

		harborURL     = fs.String("harbor-url", os.Getenv("HARBOR_URL"), "Harbor base URL (env: HARBOR_URL)")
		harborUser    = fs.String("harbor-user", os.Getenv("HARBOR_USERNAME"), "Harbor username (env: HARBOR_USERNAME); the password is read from HARBOR_PASSWORD")
		harborStorage = fs.Int("harbor-storage-gib", 0, "Harbor project storage limit in GiB (0 = unlimited)")

		harborRegistry = fs.String("harbor-registry", os.Getenv("HARBOR_REGISTRY"), "registry host[:port] as used in image names (env: HARBOR_REGISTRY; default: host of -harbor-url)")
		robotDays      = fs.Int("robot-days", 0, "Harbor robot account lifetime in days (0 = never expires)")
		secretsFile    = fs.String("secrets-file", "", "save the one-time credentials to this NEW file (mode 0600) instead of printing them")

		kcURL      = fs.String("keycloak-url", os.Getenv("KEYCLOAK_URL"), "Keycloak base URL (env: KEYCLOAK_URL); older versions need a /auth suffix")
		kcRealm    = fs.String("keycloak-realm", os.Getenv("KEYCLOAK_REALM"), "realm NAME (as in the URL, not the display name) that holds the groups (env: KEYCLOAK_REALM)")
		kcTokenRlm = fs.String("keycloak-token-realm", os.Getenv("KEYCLOAK_TOKEN_REALM"), "realm the service-account client lives in (default: same as -keycloak-realm)")
		kcClientID = fs.String("keycloak-client-id", os.Getenv("KEYCLOAK_CLIENT_ID"), "service-account client ID (env: KEYCLOAK_CLIENT_ID); the secret is read from KEYCLOAK_CLIENT_SECRET")

		caFile   = fs.String("ca-file", os.Getenv("ONBOARDCTL_CA_FILE"), "PEM file with your internal CA, used for Harbor and Keycloak (env: ONBOARDCTL_CA_FILE)")
		insecure = fs.Bool("insecure", false, "skip TLS verification for Harbor and Keycloak (avoid; prefer -ca-file)")

		ocBinary  = fs.String("oc-binary", "oc", "oc executable")
		ocContext = fs.String("oc-context", "", "kubeconfig context to use (default: current)")
		ocMode    = fs.String("openshift-mode", "project", "how to create the namespace: project (oc new-project, works with self-provisioner) or namespace (oc create namespace)")

		dryRun = fs.Bool("dry-run", false, "print what would be done without changing anything or needing credentials")
	)
	if err := fs.Parse(args); err != nil {
		return err // flag already printed usage (or help)
	}

	mode, err := openshift.ParseCreateMode(*ocMode)
	if err != nil {
		return err
	}
	registry := *harborRegistry
	if registry == "" && *harborURL != "" {
		if registry, err = registryHost(*harborURL); err != nil {
			return err
		}
	}
	cfg := onboard.Config{
		Name:               *name,
		GroupRoot:          *groupRoot,
		HarborStorageGiB:   *harborStorage,
		OpenShiftMode:      mode,
		HarborRegistry:     registry,
		RobotName:          onboard.DefaultRobotName,
		RobotDurationDays:  *robotDays,
		PullSecretName:     onboard.DefaultPullSecretName,
		CICDServiceAccount: onboard.DefaultCICDAccount,
		CICDTokenSecret:    onboard.DefaultCICDTokenName,
	}

	// Real clients are only needed when we actually change things. In dry-run
	// mode we pass nil: Execute never calls Run, so it works without credentials.
	var (
		h onboard.HarborAPI
		k onboard.KeycloakAPI
		c onboard.Cluster
	)
	if !*dryRun {
		harborPass := os.Getenv("HARBOR_PASSWORD") // env only: keeps secrets out of shell history and `ps`
		kcSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")

		// Report every missing setting at once instead of one per run.
		var missing []string
		for _, m := range []struct {
			ok   bool
			what string
		}{
			{*harborURL != "", "-harbor-url (HARBOR_URL)"},
			{*harborUser != "", "-harbor-user (HARBOR_USERNAME)"},
			{harborPass != "", "HARBOR_PASSWORD"},
			{*kcURL != "", "-keycloak-url (KEYCLOAK_URL)"},
			{*kcRealm != "", "-keycloak-realm (KEYCLOAK_REALM)"},
			{*kcClientID != "", "-keycloak-client-id (KEYCLOAK_CLIENT_ID)"},
			{kcSecret != "", "KEYCLOAK_CLIENT_SECRET"},
		} {
			if !m.ok {
				missing = append(missing, m.what)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
		}

		httpClient, err := httpx.NewClient(httpx.Options{CAFile: *caFile, Insecure: *insecure})
		if err != nil {
			return err
		}
		hc, err := harbor.New(harbor.Config{BaseURL: *harborURL, Username: *harborUser, Password: harborPass, HTTPClient: httpClient})
		if err != nil {
			return err
		}
		kc, err := keycloak.New(keycloak.Config{
			BaseURL: *kcURL, Realm: *kcRealm, TokenRealm: *kcTokenRlm,
			ClientID: *kcClientID, ClientSecret: kcSecret, HTTPClient: httpClient,
		})
		if err != nil {
			return err
		}
		h, k = hc, kc
		c = &openshift.OC{Binary: *ocBinary, Context: *ocContext}
	}

	secrets := &onboard.Secrets{}
	steps, err := onboard.BuildPlan(cfg, h, k, c, secrets)
	if err != nil {
		return err
	}

	// Open the secrets file BEFORE changing anything, so a bad path cannot make
	// us lose the one-time robot secret later.
	var secretsOut *os.File
	if !*dryRun && *secretsFile != "" {
		if secretsOut, err = openSecretsFile(*secretsFile); err != nil {
			return err
		}
	}

	// Cancel everything cleanly on Ctrl+C or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	execErr := onboard.Execute(ctx, steps, *dryRun, stdout)

	// Show or save the one-time secrets even if a later step failed: Harbor
	// will never show the robot secret again.
	emitSecrets(stdout, secrets, secretsOut)

	if execErr != nil {
		return fmt.Errorf("%w\nEvery step is idempotent: fix the problem and re-run the same command", execErr)
	}
	if !*dryRun {
		fmt.Fprintf(stdout, "\nDone. Project %q is onboarded.\n", cfg.Name)
		fmt.Fprintf(stdout, "Next:\n  - add users to these Keycloak groups (realm %q):\n", *kcRealm)
		for _, rg := range onboard.RoleGroups {
			fmt.Fprintf(stdout, "      %s\n", cfg.GroupPath(rg.Suffix))
		}
		fmt.Fprintf(stdout, "  - put the Harbor robot credentials and the ci-cd token into your CI/CD variables\n")
	}
	return nil
}

// registryHost extracts host[:port] from a URL such as https://harbor.example.com.
func registryHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("cannot get a registry host from %q: pass a full URL like https://harbor.example.com or set -harbor-registry", raw)
	}
	return u.Host, nil
}

// openSecretsFile creates a new file readable only by the current user.
// O_EXCL refuses to overwrite an existing file.
func openSecretsFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("-secrets-file: %w", err)
	}
	return f, nil
}

// emitSecrets writes the one-time values to f if given, otherwise to w. If
// writing to the file fails it falls back to w, because losing them means
// deleting and re-creating the robot.
func emitSecrets(w io.Writer, s *onboard.Secrets, f *os.File) {
	if s.Empty() {
		if f != nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name) // do not leave an empty file behind
		}
		return
	}
	text := strings.Join(s.Lines(), "\n") + "\n"

	if f != nil {
		_, werr := f.WriteString(text)
		cerr := f.Close()
		if werr == nil && cerr == nil {
			fmt.Fprintf(w, "\nOne-time credentials saved to %s (mode 0600). Move them into your CI/CD variables, then delete the file.\n", f.Name())
			return
		}
		fmt.Fprintf(w, "\nWARNING: could not write %s; printing the credentials instead.\n", f.Name())
	}
	fmt.Fprintf(w, "\nOne-time credentials (Harbor will not show the robot secret again; store them now):\n%s", indentLines(text))
}

func indentLines(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}
