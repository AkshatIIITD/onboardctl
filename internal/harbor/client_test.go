package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient starts a throwaway local HTTP server (httptest) that plays the
// role of Harbor, and returns a Client pointed at it. No real Harbor needed.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL, Username: "robot", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureProject_Creates(t *testing.T) {
	var gotBody projectRequest
	posted := false

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "robot" || pass != "secret" {
			t.Errorf("missing or wrong basic auth")
		}
		switch r.Method {
		case http.MethodHead:
			if r.URL.Query().Get("project_name") != "my-project" {
				t.Errorf("unexpected project_name query: %q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			posted = true
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("bad JSON body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		}
	})

	created, err := c.EnsureProject(context.Background(), ProjectSpec{Name: "my-project", StorageLimitGiB: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !created || !posted {
		t.Fatalf("expected project to be created (created=%v posted=%v)", created, posted)
	}
	if gotBody.ProjectName != "my-project" || gotBody.Public {
		t.Errorf("unexpected body: %+v", gotBody)
	}
	if want := int64(50) << 30; gotBody.StorageLimit != want {
		t.Errorf("storage limit = %d, want %d", gotBody.StorageLimit, want)
	}
}

func TestEnsureProject_UnlimitedStorage(t *testing.T) {
	var gotBody projectRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})
	if _, err := c.EnsureProject(context.Background(), ProjectSpec{Name: "my-project"}); err != nil {
		t.Fatal(err)
	}
	if gotBody.StorageLimit != -1 {
		t.Errorf("storage limit = %d, want -1 (unlimited)", gotBody.StorageLimit)
	}
}

func TestEnsureProject_AlreadyExists_DoesNotPost(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("must not POST when the project already exists")
		}
		w.WriteHeader(http.StatusOK)
	})

	created, err := c.EnsureProject(context.Background(), ProjectSpec{Name: "my-project"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created should be false for an existing project")
	}
}

func TestEnsureProject_RaceConflictIsNotAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusConflict)
	})

	created, err := c.EnsureProject(context.Background(), ProjectSpec{Name: "my-project"})
	if err != nil {
		t.Fatalf("409 on create should be treated as success, got %v", err)
	}
	if created {
		t.Error("created should be false when we lost the race")
	}
}

func TestEnsureProject_Unauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := c.EnsureProject(context.Background(), ProjectSpec{Name: "my-project"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.Status)
	}
}

func TestEnsureGroupMember(t *testing.T) {
	var gotBody memberRequest
	var gotPath, gotResourceNameHeader string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResourceNameHeader = r.Header.Get("X-Is-Resource-Name")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})

	created, err := c.EnsureGroupMember(context.Background(), "my-project",
		Group{Name: "/harbor/project/my-project/developer", Type: GroupTypeOIDC}, RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected created=true on 201")
	}
	if gotPath != "/api/v2.0/projects/my-project/members" {
		t.Errorf("path = %q", gotPath)
	}
	// Needed so a numeric-looking project name is not mistaken for an ID.
	if gotResourceNameHeader != "true" {
		t.Errorf("X-Is-Resource-Name = %q, want true", gotResourceNameHeader)
	}
	if gotBody.RoleID != 2 || gotBody.MemberGroup.GroupName != "/harbor/project/my-project/developer" || gotBody.MemberGroup.GroupType != 3 {
		t.Errorf("unexpected body: %+v", gotBody)
	}
}

func TestEnsureGroupMember_AlreadyMember(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	created, err := c.EnsureGroupMember(context.Background(), "my-project",
		Group{Name: "team-dev", Type: GroupTypeOIDC}, RoleDeveloper)
	if err != nil || created {
		t.Fatalf("created=%v err=%v, want created=false err=nil", created, err)
	}
}

func TestEnsureGroupMember_ErrorBodyIsSurfaced(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"NOT_FOUND","message":"project my-project not found"}]}`))
	})
	_, err := c.EnsureGroupMember(context.Background(), "my-project",
		Group{Name: "g", Type: GroupTypeOIDC}, RoleDeveloper)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "NOT_FOUND") || !strings.Contains(got, "project my-project not found") {
		t.Errorf("error message should include Harbor's message, got: %s", got)
	}
}
