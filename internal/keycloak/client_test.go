package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeKC is a minimal in-memory Keycloak: token endpoint plus group APIs.
type fakeKC struct {
	mu         sync.Mutex
	groups     map[string]string // path -> id
	tokenCalls int
	rejectOnce bool // answer the next authed call with 401
	raceOnPost bool // answer the next POST with 409 after silently creating the group
	badSecret  bool
	posts      []string
}

// fakeID makes a slash-free ID, as real Keycloak UUIDs are.
func fakeID(path string) string { return "id-" + strings.ReplaceAll(path[1:], "/", "_") }

func newFake(t *testing.T) (*fakeKC, *Client) {
	t.Helper()
	f := &fakeKC{groups: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Realm: "harbor", ClientID: "onboardctl", ClientSecret: "s3cret", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeKC) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/realms/harbor/protocol/openid-connect/token" {
		f.tokenCalls++
		_ = r.ParseForm()
		if f.badSecret || r.Form.Get("client_secret") != "s3cret" || r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, `{"error":"unauthorized_client"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" || f.rejectOnce {
		f.rejectOnce = false
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	const byPath = "/admin/realms/harbor/group-by-path"
	const groups = "/admin/realms/harbor/groups"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, byPath):
		p := strings.TrimPrefix(r.URL.Path, byPath)
		if id, ok := f.groups[p]; ok {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "path": p})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, groups):
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		parentPath := ""
		rest := strings.TrimPrefix(r.URL.Path, groups)
		if rest != "" { // /{id}/children
			parentID := strings.Split(strings.Trim(rest, "/"), "/")[0]
			for p, id := range f.groups {
				if id == parentID {
					parentPath = p
				}
			}
			if parentPath == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		full := parentPath + "/" + body.Name
		f.posts = append(f.posts, full)
		_, exists := f.groups[full]
		if f.raceOnPost {
			f.raceOnPost = false
			f.groups[full] = fakeID(full)
			exists = true
		}
		if exists {
			w.WriteHeader(http.StatusConflict)
			return
		}
		id := fakeID(full)
		f.groups[full] = id
		w.Header().Set("Location", "http://kc"+groups+"/"+id)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func TestEnsureGroupPath_CreatesHierarchy(t *testing.T) {
	f, c := newFake(t)
	created, err := c.EnsureGroupPath(context.Background(), "/harbor/project/my-project/admin")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected created=true")
	}
	want := []string{"/harbor", "/harbor/project", "/harbor/project/my-project", "/harbor/project/my-project/admin"}
	if strings.Join(f.posts, ",") != strings.Join(want, ",") {
		t.Errorf("created %v, want %v", f.posts, want)
	}
	if f.tokenCalls != 1 {
		t.Errorf("token fetched %d times, want 1", f.tokenCalls)
	}
}

func TestEnsureGroupPath_Idempotent(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if _, err := c.EnsureGroupPath(ctx, "/harbor/project/my-project/admin"); err != nil {
		t.Fatal(err)
	}
	before := len(f.posts)
	created, err := c.EnsureGroupPath(ctx, "/harbor/project/my-project/admin")
	if err != nil {
		t.Fatal(err)
	}
	if created || len(f.posts) != before {
		t.Errorf("second run created=%v, extra posts=%d; want none", created, len(f.posts)-before)
	}
}

func TestEnsureGroupPath_SiblingsReuseParents(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	for _, r := range []string{"admin", "maintainer", "developer", "guest"} {
		created, err := c.EnsureGroupPath(ctx, "/harbor/project/my-project/"+r)
		if err != nil || !created {
			t.Fatalf("%s: created=%v err=%v", r, created, err)
		}
	}
	// 3 parents + 4 leaves, each created exactly once.
	if len(f.posts) != 7 {
		t.Errorf("posts = %v, want 7", f.posts)
	}
}

func TestEnsureGroupPath_ReauthOn401(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if _, err := c.EnsureGroupPath(ctx, "/a"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.rejectOnce = true
	f.mu.Unlock()
	if _, err := c.EnsureGroupPath(ctx, "/b"); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls != 2 {
		t.Errorf("token fetched %d times, want 2 (initial + refresh)", f.tokenCalls)
	}
}

func TestEnsureGroupPath_BadSecretDoesNotLeakIt(t *testing.T) {
	f, c := newFake(t)
	f.badSecret = true
	_, err := c.EnsureGroupPath(context.Background(), "/a")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the client secret: %v", err)
	}
}

func TestEnsureGroupPath_RaceOn409(t *testing.T) {
	f, c := newFake(t)
	f.raceOnPost = true
	created, err := c.EnsureGroupPath(context.Background(), "/a")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("409 means someone else created it; created should be false")
	}
}

func TestEnsureGroupPath_InvalidPaths(t *testing.T) {
	_, c := newFake(t)
	for _, p := range []string{"", "/", "a/b", "/a//b", "/a/"} {
		if _, err := c.EnsureGroupPath(context.Background(), p); err == nil {
			t.Errorf("path %q: expected error", p)
		}
	}
}

func TestNew_Validation(t *testing.T) {
	cases := []Config{
		{Realm: "r", ClientID: "i", ClientSecret: "s"},
		{BaseURL: "u", ClientID: "i", ClientSecret: "s"},
		{BaseURL: "u", Realm: "r", ClientSecret: "s"},
		{BaseURL: "u", Realm: "r", ClientID: "i"},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
