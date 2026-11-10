package onboard

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/AkshatIIITD/onboardctl/internal/harbor"
	"github.com/AkshatIIITD/onboardctl/internal/openshift"
)

// One shared call log lets us assert the ORDER across all three systems.
type log struct{ calls []string }

type fakeHarbor struct {
	l           *log
	projectErr  error
	robotExists bool
	robotErr    error
}

func (f *fakeHarbor) EnsureRobot(_ context.Context, spec harbor.RobotSpec) (harbor.RobotCredential, bool, error) {
	f.l.calls = append(f.l.calls, "harbor-robot:"+spec.Project+":"+spec.Name)
	if f.robotErr != nil || f.robotExists {
		return harbor.RobotCredential{}, false, f.robotErr
	}
	return harbor.RobotCredential{Username: "robot$" + spec.Project + "+" + spec.Name, Secret: "ROBOT-SECRET"}, true, nil
}

func (f *fakeHarbor) EnsureProject(_ context.Context, spec harbor.ProjectSpec) (bool, error) {
	f.l.calls = append(f.l.calls, "harbor-project:"+spec.Name)
	return true, f.projectErr
}

func (f *fakeHarbor) EnsureGroupMember(_ context.Context, project string, g harbor.Group, r harbor.Role) (bool, error) {
	f.l.calls = append(f.l.calls, "harbor-member:"+project+":"+g.Name+":"+r.String())
	if g.Type != harbor.GroupTypeOIDC {
		return false, errors.New("group type must be OIDC")
	}
	return true, nil
}

type fakeKeycloak struct{ l *log }

func (f *fakeKeycloak) EnsureGroupPath(_ context.Context, p string) (bool, error) {
	f.l.calls = append(f.l.calls, "kc-group:"+p)
	return true, nil
}

type fakeCluster struct {
	l          *log
	err        error
	pullExists bool
	cicdErr    error
}

func (f *fakeCluster) SecretExists(_ context.Context, ns, name string) (bool, error) {
	f.l.calls = append(f.l.calls, "oc-secret-exists:"+ns+":"+name)
	return f.pullExists, nil
}

func (f *fakeCluster) ApplyPullSecret(_ context.Context, ns, name, registry, user, password string) error {
	f.l.calls = append(f.l.calls, "oc-pull-secret:"+ns+":"+name+":"+registry+":"+user+":"+password)
	return nil
}

func (f *fakeCluster) LinkPullSecret(_ context.Context, ns, sa, secret string) error {
	f.l.calls = append(f.l.calls, "oc-link:"+ns+":"+sa+":"+secret)
	return nil
}

func (f *fakeCluster) EnsureCICD(_ context.Context, ns, sa, tokenSecret string) error {
	f.l.calls = append(f.l.calls, "oc-cicd:"+ns+":"+sa+":"+tokenSecret)
	return f.cicdErr
}

func (f *fakeCluster) EnsureNamespace(_ context.Context, name string, mode openshift.CreateMode) (bool, error) {
	f.l.calls = append(f.l.calls, "oc:"+string(mode)+":"+name)
	return true, f.err
}

func testConfig() Config {
	return Config{
		Name: "new-project", GroupRoot: "/harbor/project", OpenShiftMode: openshift.ModeProject,
		HarborRegistry: "harbor.example.com", RobotName: DefaultRobotName, PullSecretName: DefaultPullSecretName,
		CICDServiceAccount: DefaultCICDAccount, CICDTokenSecret: DefaultCICDTokenName,
	}
}

func fakes() (*log, *fakeHarbor, *fakeKeycloak, *fakeCluster) {
	l := &log{}
	return l, &fakeHarbor{l: l}, &fakeKeycloak{l: l}, &fakeCluster{l: l}
}

func TestExecute_RunsStepsInOrder(t *testing.T) {
	l, h, k, c := fakes()
	steps, err := BuildPlan(testConfig(), h, k, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Execute(context.Background(), steps, false, &out); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"oc:project:new-project",
		"kc-group:/harbor/project/new-project/admin",
		"kc-group:/harbor/project/new-project/maintainer",
		"kc-group:/harbor/project/new-project/developer",
		"kc-group:/harbor/project/new-project/guest",
		"harbor-project:new-project",
		"harbor-member:new-project:/harbor/project/new-project/admin:project-admin",
		"harbor-member:new-project:/harbor/project/new-project/maintainer:maintainer",
		"harbor-member:new-project:/harbor/project/new-project/developer:developer",
		"harbor-member:new-project:/harbor/project/new-project/guest:guest",
		"harbor-robot:new-project:pull-push",
		"oc-pull-secret:new-project:harbor-pull:harbor.example.com:robot$new-project+pull-push:ROBOT-SECRET",
		"oc-link:new-project:default:harbor-pull",
		"oc-cicd:new-project:ci-cd:ci-cd-token",
	}
	if !reflect.DeepEqual(l.calls, want) {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(l.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecute_DryRunChangesNothing(t *testing.T) {
	l, h, k, c := fakes()
	steps, err := BuildPlan(testConfig(), h, k, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Execute(context.Background(), steps, true, &out); err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 0 {
		t.Errorf("dry-run made calls: %v", l.calls)
	}
	if len(steps) != 14 {
		t.Errorf("steps = %d, want 14", len(steps))
	}
	if !strings.Contains(out.String(), "/harbor/project/new-project/guest") {
		t.Errorf("dry-run output should list the group paths:\n%s", out.String())
	}
}

func TestBuildPlan_DryRunAllowsNilClients(t *testing.T) {
	if _, err := BuildPlan(testConfig(), nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_StopsAtFirstFailure(t *testing.T) {
	l, h, k, c := fakes()
	c.err = errors.New("boom")
	steps, _ := BuildPlan(testConfig(), h, k, c, nil)

	var out bytes.Buffer
	err := Execute(context.Background(), steps, false, &out)
	if err == nil || !errors.Is(err, c.err) {
		t.Fatalf("err = %v, want it to wrap the cluster error", err)
	}
	if len(l.calls) != 1 {
		t.Errorf("calls after failure = %v, want only the failing one", l.calls)
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Errorf("output should say FAILED:\n%s", out.String())
	}
}

func TestExecute_StopsWhenContextCancelled(t *testing.T) {
	l, h, k, c := fakes()
	steps, _ := BuildPlan(testConfig(), h, k, c, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(ctx, steps, false, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(l.calls) != 0 {
		t.Errorf("calls = %v, want none", l.calls)
	}
}

func TestConfigValidate(t *testing.T) {
	// Each case changes exactly one field of an otherwise valid config.
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty name", func(c *Config) { c.Name = "" }},
		{"bad name", func(c *Config) { c.Name = "Bad_Name" }},
		{"group root without slash", func(c *Config) { c.GroupRoot = "harbor/project" }},
		{"bad openshift mode", func(c *Config) { c.OpenShiftMode = "quota" }},
		{"negative storage", func(c *Config) { c.HarborStorageGiB = -1 }},
		{"bad robot name", func(c *Config) { c.RobotName = "Bad_Robot" }},
		{"empty pull secret name", func(c *Config) { c.PullSecretName = "" }},
		{"bad service account", func(c *Config) { c.CICDServiceAccount = "CI CD" }},
		{"empty token secret name", func(c *Config) { c.CICDTokenSecret = "" }},
		{"registry with scheme", func(c *Config) { c.HarborRegistry = "https://harbor.example.com" }},
		{"negative robot days", func(c *Config) { c.RobotDurationDays = -1 }},
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("baseline config must be valid: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)
			if _, err := BuildPlan(cfg, nil, nil, nil, nil); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestGroupPath(t *testing.T) {
	if got := testConfig().GroupPath("admin"); got != "/harbor/project/new-project/admin" {
		t.Errorf("GroupPath = %q", got)
	}
}

func TestExecute_RobotSecretIsCapturedOnceAndNeverPrinted(t *testing.T) {
	_, h, k, c := fakes()
	secrets := &Secrets{}
	steps, _ := BuildPlan(testConfig(), h, k, c, secrets)

	var out bytes.Buffer
	if err := Execute(context.Background(), steps, false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "ROBOT-SECRET") {
		t.Errorf("secret leaked into the progress output:\n%s", out.String())
	}
	want := []string{"Harbor robot username: robot$new-project+pull-push", "Harbor robot secret: ROBOT-SECRET"}
	if !reflect.DeepEqual(secrets.Lines(), want) {
		t.Errorf("secrets = %v, want %v", secrets.Lines(), want)
	}
}

func TestExecute_SecretsSurviveALaterFailure(t *testing.T) {
	_, h, k, c := fakes()
	c.cicdErr = errors.New("boom") // the last step fails after the robot was created
	secrets := &Secrets{}
	steps, _ := BuildPlan(testConfig(), h, k, c, secrets)

	if err := Execute(context.Background(), steps, false, &bytes.Buffer{}); err == nil {
		t.Fatal("expected a failure")
	}
	if secrets.Empty() {
		t.Error("the one-time robot secret must still be available after a later failure")
	}
}

func TestExecute_ExistingRobotAndExistingPullSecretIsFine(t *testing.T) {
	l, h, k, c := fakes()
	h.robotExists, c.pullExists = true, true
	secrets := &Secrets{}
	steps, _ := BuildPlan(testConfig(), h, k, c, secrets)

	if err := Execute(context.Background(), steps, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !secrets.Empty() {
		t.Errorf("nothing new to show, got %v", secrets.Lines())
	}
	for _, call := range l.calls {
		if strings.HasPrefix(call, "oc-pull-secret:") {
			t.Errorf("must not overwrite the pull secret without a new robot secret: %s", call)
		}
	}
}

func TestExecute_ExistingRobotButMissingPullSecretFailsWithInstructions(t *testing.T) {
	_, h, k, c := fakes()
	h.robotExists = true // pullExists stays false
	steps, _ := BuildPlan(testConfig(), h, k, c, nil)

	err := Execute(context.Background(), steps, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "delete the robot") {
		t.Fatalf("err = %v, want instructions to delete the robot", err)
	}
}

func TestSecrets_NilIsSafe(t *testing.T) {
	var s *Secrets
	s.Add("a", "b")
	if !s.Empty() || s.Lines() != nil {
		t.Error("nil Secrets should behave as empty")
	}
}
