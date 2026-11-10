package openshift

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type rec struct {
	args  []string
	stdin []byte
}

// scripted answers calls in order and records args and stdin.
func scripted(replies ...reply) (RunFunc, *[]rec) {
	var calls []rec
	i := 0
	return func(_ context.Context, stdin []byte, binary string, args ...string) (string, error) {
		calls = append(calls, rec{append([]string{binary}, args...), stdin})
		if i >= len(replies) {
			return "", errors.New("unexpected extra call")
		}
		r := replies[i]
		i++
		return r.out, r.err
	}, &calls
}

func TestApplyPullSecret(t *testing.T) {
	run, calls := scripted(reply{out: "secret/harbor-registry created"})
	oc := &OC{Run: run}
	if err := oc.ApplyPullSecret(context.Background(), "new-project", "harbor-registry", "harbor.example.com", "robot$new-project+pull-push", "S3CRET"); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if strings.Join(c.args, " ") != "oc apply -f - -n new-project" {
		t.Errorf("args = %v", c.args)
	}
	if strings.Contains(strings.Join(c.args, " "), "S3CRET") {
		t.Error("password must not be in arguments")
	}

	var m struct {
		Type string                           `json:"type"`
		Data map[string]string                `json:"data"`
		Meta struct{ Name, Namespace string } `json:"metadata"`
	}
	if err := json.Unmarshal(c.stdin, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "kubernetes.io/dockerconfigjson" || m.Meta.Name != "harbor-registry" || m.Meta.Namespace != "new-project" {
		t.Errorf("manifest = %+v", m)
	}
	raw, _ := base64.StdEncoding.DecodeString(m.Data[".dockerconfigjson"])
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	e := cfg.Auths["harbor.example.com"]
	wantAuth := base64.StdEncoding.EncodeToString([]byte("robot$new-project+pull-push:S3CRET"))
	if e.Username != "robot$new-project+pull-push" || e.Password != "S3CRET" || e.Auth != wantAuth {
		t.Errorf("auth entry = %+v", e)
	}
}

func TestApplyPullSecret_ErrorOutputIsRedacted(t *testing.T) {
	_, b64, _ := pullSecretManifest("new-project", "s", "h", "u", "S3CRET")
	run, _ := scripted(reply{out: "invalid: S3CRET and " + b64, err: errExit})
	err := (&OC{Run: run}).ApplyPullSecret(context.Background(), "new-project", "s", "h", "u", "S3CRET")
	if err == nil || strings.Contains(err.Error(), "S3CRET") || strings.Contains(err.Error(), b64) {
		t.Errorf("err = %v", err)
	}
}

func TestLinkPullSecret(t *testing.T) {
	run, calls := scripted(reply{})
	if err := (&OC{Run: run}).LinkPullSecret(context.Background(), "new-project", "default", "harbor-registry"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((*calls)[0].args, " "); got != "oc secrets link default harbor-registry --for=pull -n new-project" {
		t.Errorf("args = %q", got)
	}
}

func TestSecretExists(t *testing.T) {
	run, _ := scripted(reply{out: "secret/x"}, reply{out: `Error from server (NotFound): secrets "x" not found`, err: errExit}, reply{out: "Unauthorized", err: errExit})
	oc := &OC{Run: run}
	if ok, err := oc.SecretExists(context.Background(), "new-project", "x"); !ok || err != nil {
		t.Errorf("present: %v %v", ok, err)
	}
	if ok, err := oc.SecretExists(context.Background(), "new-project", "x"); ok || err != nil {
		t.Errorf("missing: %v %v", ok, err)
	}
	if _, err := oc.SecretExists(context.Background(), "new-project", "x"); err == nil {
		t.Error("other errors must be reported")
	}
}

func TestEnsureCICD_AppliesAndWaitsForToken(t *testing.T) {
	run, calls := scripted(reply{out: "applied"}, reply{out: ""}, reply{out: "dG9rZW4="})
	oc := &OC{Run: run, PollInterval: time.Millisecond}
	if err := oc.EnsureCICD(context.Background(), "new-project", "ci-cd", "ci-cd-token"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls = %d, want apply + 2 polls", len(*calls))
	}

	var list struct {
		Items []struct {
			Kind     string
			Type     string
			Metadata struct {
				Name        string
				Annotations map[string]string
			}
			RoleRef  struct{ Kind, Name string }
			Subjects []struct{ Kind, Name, Namespace string }
		}
	}
	if err := json.Unmarshal((*calls)[0].stdin, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("items = %d", len(list.Items))
	}
	sa, rb, sec := list.Items[0], list.Items[1], list.Items[2]
	if sa.Kind != "ServiceAccount" || sa.Metadata.Name != "ci-cd" {
		t.Errorf("sa = %+v", sa)
	}
	if rb.Kind != "RoleBinding" || rb.RoleRef.Name != "edit" || rb.RoleRef.Kind != "ClusterRole" ||
		rb.Subjects[0].Name != "ci-cd" || rb.Subjects[0].Namespace != "new-project" {
		t.Errorf("rolebinding = %+v", rb)
	}
	if sec.Type != "kubernetes.io/service-account-token" || sec.Metadata.Name != "ci-cd-token" ||
		sec.Metadata.Annotations["kubernetes.io/service-account.name"] != "ci-cd" {
		t.Errorf("secret = %+v", sec)
	}
}

func TestEnsureCICD_TimesOutWithoutToken(t *testing.T) {
	run := RunFunc(func(_ context.Context, _ []byte, _ string, args ...string) (string, error) { return "", nil })
	oc := &OC{Run: run, PollInterval: time.Millisecond, PollTimeout: 5 * time.Millisecond}
	err := oc.EnsureCICD(context.Background(), "new-project", "ci-cd", "ci-cd-token")
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureCICD_ApplyFailure(t *testing.T) {
	run, calls := scripted(reply{out: "Error from server (Forbidden): cannot create rolebindings", err: errExit})
	err := (&OC{Run: run}).EnsureCICD(context.Background(), "new-project", "ci-cd", "ci-cd-token")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") || len(*calls) != 1 {
		t.Errorf("err = %v, calls = %d", err, len(*calls))
	}
}
