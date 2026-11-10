package openshift

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SecretExists reports whether a secret exists in the namespace.
func (o *OC) SecretExists(ctx context.Context, ns, name string) (bool, error) {
	out, err := o.run(ctx, nil, "get", "secret", name, "-n", ns, "-o", "name")
	if err == nil {
		return true, nil
	}
	if containsFold(out, "notfound", "not found") {
		return false, nil
	}
	return false, fmt.Errorf("checking secret %q: %w\n%s", name, err, out)
}

// ApplyPullSecret creates or replaces a kubernetes.io/dockerconfigjson secret.
// The credentials travel on stdin and are redacted from any error output.
func (o *OC) ApplyPullSecret(ctx context.Context, ns, name, registry, user, password string) error {
	manifest, cfgB64, err := pullSecretManifest(ns, name, registry, user, password)
	if err != nil {
		return err
	}
	out, err := o.run(ctx, manifest, "apply", "-f", "-", "-n", ns)
	if err != nil {
		return fmt.Errorf("applying secret %q: %w\n%s", name, err, redact(out, password, cfgB64))
	}
	return nil
}

// LinkPullSecret lets the service account pull images using the secret.
// Linking twice is harmless.
func (o *OC) LinkPullSecret(ctx context.Context, ns, serviceAccount, secret string) error {
	out, err := o.run(ctx, nil, "secrets", "link", serviceAccount, secret, "--for=pull", "-n", ns)
	if err != nil {
		return fmt.Errorf("linking secret %q to service account %q: %w\n%s", secret, serviceAccount, err, out)
	}
	return nil
}

// EnsureCICD creates a service account with the "edit" role in the namespace
// and a long-lived token secret for it, then waits until OpenShift has filled
// in the token. Everything is applied declaratively, so re-running is safe.
// The token itself is never read into memory or printed.
func (o *OC) EnsureCICD(ctx context.Context, ns, serviceAccount, tokenSecret string) error {
	manifest, err := cicdManifest(ns, serviceAccount, tokenSecret)
	if err != nil {
		return err
	}
	if out, err := o.run(ctx, manifest, "apply", "-f", "-", "-n", ns); err != nil {
		return fmt.Errorf("applying service account %q: %w\n%s", serviceAccount, err, out)
	}

	interval, timeout := o.PollInterval, o.PollTimeout
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		// jsonpath prints the (base64) token only if it exists; we only test for non-empty.
		out, err := o.run(ctx, nil, "get", "secret", tokenSecret, "-n", ns, "-o", "jsonpath={.data.token}")
		if err != nil {
			return fmt.Errorf("reading token secret %q: %w", tokenSecret, err)
		}
		if out != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("secret %q still has no token after %s; is the service-account token controller running?", tokenSecret, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func pullSecretManifest(ns, name, registry, user, password string) (manifest []byte, cfgB64 string, err error) {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	cfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			registry: map[string]string{"username": user, "password": password, "auth": auth},
		},
	})
	if err != nil {
		return nil, "", err
	}
	cfgB64 = base64.StdEncoding.EncodeToString(cfg)
	manifest, err = json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "kubernetes.io/dockerconfigjson",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       map[string]string{".dockerconfigjson": cfgB64},
	})
	return manifest, cfgB64, err
}

func cicdManifest(ns, sa, tokenSecret string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items": []any{
			map[string]any{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata":   map[string]any{"name": sa, "namespace": ns},
			},
			map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "RoleBinding",
				"metadata":   map[string]any{"name": sa + "-edit", "namespace": ns},
				"roleRef":    map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "edit"},
				"subjects":   []any{map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": ns}},
			},
			map[string]any{
				"apiVersion": "v1",
				"kind":       "Secret",
				"type":       "kubernetes.io/service-account-token",
				"metadata": map[string]any{
					"name":        tokenSecret,
					"namespace":   ns,
					"annotations": map[string]string{"kubernetes.io/service-account.name": sa},
				},
			},
		},
	})
}

func redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	return s
}
