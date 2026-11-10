package harbor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestEnsureRobot_Creates(t *testing.T) {
	var got robotRequest
	var path, hdr string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, hdr = r.URL.Path, r.Header.Get("X-Is-Resource-Name")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"name":"robot$new-project+pull-push","secret":"S3CRET"}`))
	})
	cred, created, err := c.EnsureRobot(context.Background(), RobotSpec{Name: "pull-push", Project: "new-project"})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if cred.Username != "robot$new-project+pull-push" || cred.Secret != "S3CRET" {
		t.Errorf("cred = %q / %q", cred.Username, cred.Secret)
	}
	if path != "/api/v2.0/robots" || hdr != "true" {
		t.Errorf("path=%q header=%q", path, hdr)
	}
	if got.Level != "project" || got.Duration != -1 || got.Name != "pull-push" {
		t.Errorf("request = %+v", got)
	}
	perm := got.Permissions[0]
	if perm.Kind != "project" || perm.Namespace != "new-project" || len(perm.Access) != 2 ||
		perm.Access[0] != (robotAccess{"repository", "pull"}) || perm.Access[1] != (robotAccess{"repository", "push"}) {
		t.Errorf("permissions = %+v", perm)
	}
}

func TestEnsureRobot_Duration(t *testing.T) {
	var got robotRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"robot$new-project+x","secret":"s"}`))
	})
	if _, _, err := c.EnsureRobot(context.Background(), RobotSpec{Name: "x", Project: "new-project", DurationDays: 90}); err != nil {
		t.Fatal(err)
	}
	if got.Duration != 90 {
		t.Errorf("duration = %d, want 90", got.Duration)
	}
}

func TestEnsureRobot_AlreadyExists(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })
	cred, created, err := c.EnsureRobot(context.Background(), RobotSpec{Name: "pull-push", Project: "new-project"})
	if err != nil || created || cred.Secret != "" {
		t.Errorf("cred=%v created=%v err=%v; want empty,false,nil", cred, created, err)
	}
}

func TestEnsureRobot_ResponseWithoutSecretIsAnErrorThatDoesNotEchoBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"robot$new-project+pull-push","secret":"","note":"LEAKME"}`))
	})
	_, _, err := c.EnsureRobot(context.Background(), RobotSpec{Name: "pull-push", Project: "new-project"})
	if err == nil || strings.Contains(err.Error(), "LEAKME") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureRobot_ForbiddenIsAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	_, _, err := c.EnsureRobot(context.Background(), RobotSpec{Name: "pull-push", Project: "new-project"})
	if _, ok := err.(*APIError); !ok {
		t.Errorf("err = %T %v, want *APIError", err, err)
	}
}

func TestRobotCredential_NeverPrintsSecret(t *testing.T) {
	cred := RobotCredential{Username: "robot$new-project+pull-push", Secret: "S3CRET"}
	for _, s := range []string{fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred), fmt.Sprintf("%s", cred), fmt.Sprintf("%#v", cred)} {
		if strings.Contains(s, "S3CRET") {
			t.Errorf("formatted credential leaks secret: %q", s)
		}
	}
}
