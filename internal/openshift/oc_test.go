package openshift

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type reply struct {
	out string
	err error
}

// fakeRun answers commands in order and records every call.
func fakeRun(replies ...reply) (RunFunc, *[][]string) {
	var calls [][]string
	i := 0
	return func(_ context.Context, _ []byte, binary string, args ...string) (string, error) {
		calls = append(calls, append([]string{binary}, args...))
		if i >= len(replies) {
			return "", errors.New("unexpected extra call")
		}
		r := replies[i]
		i++
		return r.out, r.err
	}, &calls
}

var errExit = errors.New("exit status 1")

func TestEnsureNamespace_AlreadyExists(t *testing.T) {
	run, calls := fakeRun(reply{out: "project.project.openshift.io/cpv"})
	oc := &OC{Run: run}
	created, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if err != nil || created {
		t.Fatalf("created=%v err=%v, want false,nil", created, err)
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %v, want only the get", *calls)
	}
}

func TestEnsureNamespace_CreatesProject(t *testing.T) {
	run, calls := fakeRun(
		reply{out: `Error from server (NotFound): projects "cpv" not found`, err: errExit},
		reply{out: "Now using project", err: nil},
	)
	oc := &OC{Run: run}
	created, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v, want true,nil", created, err)
	}
	want := [][]string{
		{"oc", "get", "project", "cpv", "-o", "name"},
		{"oc", "new-project", "cpv", "--skip-config-write"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestEnsureNamespace_NamespaceMode(t *testing.T) {
	run, calls := fakeRun(
		reply{out: `Error from server (NotFound): namespaces "cpv" not found`, err: errExit},
		reply{out: "namespace/cpv created"},
	)
	oc := &OC{Run: run}
	if _, err := oc.EnsureNamespace(context.Background(), "cpv", ModeNamespace); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"oc", "get", "namespace", "cpv", "-o", "name"},
		{"oc", "create", "namespace", "cpv"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestEnsureNamespace_HiddenCollision(t *testing.T) {
	run, _ := fakeRun(
		reply{out: `Error from server (NotFound): projects "cpv" not found`, err: errExit},
		reply{out: `Error from server (AlreadyExists): project.project.openshift.io "cpv" already exists`, err: errExit},
	)
	oc := &OC{Run: run}
	_, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if !errors.Is(err, ErrNameCollision) {
		t.Fatalf("err = %v, want ErrNameCollision", err)
	}
}

func TestEnsureNamespace_UnauthorizedDoesNotCreate(t *testing.T) {
	run, calls := fakeRun(reply{out: "error: You must be logged in to the server (Unauthorized)", err: errExit})
	oc := &OC{Run: run}
	_, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err = %v, want it to mention Unauthorized", err)
	}
	if len(*calls) != 1 {
		t.Errorf("must not attempt to create after a non-NotFound error; calls = %v", *calls)
	}
}

func TestEnsureNamespace_CreateFailureShowsOutput(t *testing.T) {
	run, _ := fakeRun(
		reply{out: "NotFound", err: errExit},
		reply{out: "Error from server (Forbidden): cannot create projectrequests", err: errExit},
	)
	oc := &OC{Run: run}
	_, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err = %v, want it to include oc's output", err)
	}
}

func TestEnsureNamespace_ContextFlag(t *testing.T) {
	run, calls := fakeRun(reply{out: "ok"})
	oc := &OC{Binary: "/usr/bin/oc", Context: "prod", Run: run}
	if _, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/oc", "get", "project", "cpv", "-o", "name", "--context", "prod"}
	if !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("call = %v, want %v", (*calls)[0], want)
	}
}

func TestParseCreateMode(t *testing.T) {
	for in, want := range map[string]CreateMode{"project": ModeProject, " Namespace ": ModeNamespace} {
		got, err := ParseCreateMode(in)
		if err != nil || got != want {
			t.Errorf("ParseCreateMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseCreateMode("quota"); err == nil {
		t.Error("expected error for invalid mode")
	}
}

const forbiddenGet = `Error from server (Forbidden): projects.project.openshift.io "cpv" is forbidden: User "akshat.singh" cannot get resource "projects" in API group "project.openshift.io" in the namespace "cpv"`

// Users with only self-provisioner can create projects but not read them.
func TestEnsureNamespace_ForbiddenGetStillCreatesInProjectMode(t *testing.T) {
	run, calls := fakeRun(
		reply{out: forbiddenGet, err: errExit},
		reply{out: "Created project cpv"},
	)
	oc := &OC{Run: run}
	created, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v, want true,nil", created, err)
	}
	if got := (*calls)[1]; !reflect.DeepEqual(got, []string{"oc", "new-project", "cpv", "--skip-config-write"}) {
		t.Errorf("second call = %v", got)
	}
}

func TestEnsureNamespace_ForbiddenGetThenAlreadyExistsIsACollision(t *testing.T) {
	run, _ := fakeRun(
		reply{out: forbiddenGet, err: errExit},
		reply{out: `Error from server (AlreadyExists): project.project.openshift.io "cpv" already exists`, err: errExit},
	)
	oc := &OC{Run: run}
	_, err := oc.EnsureNamespace(context.Background(), "cpv", ModeProject)
	if !errors.Is(err, ErrNameCollision) {
		t.Fatalf("err = %v, want ErrNameCollision", err)
	}
}

func TestEnsureNamespace_ForbiddenGetInNamespaceModeIsAnError(t *testing.T) {
	run, calls := fakeRun(reply{out: `Error from server (Forbidden): namespaces "cpv" is forbidden`, err: errExit})
	oc := &OC{Run: run}
	if _, err := oc.EnsureNamespace(context.Background(), "cpv", ModeNamespace); err == nil {
		t.Fatal("expected an error")
	}
	if len(*calls) != 1 {
		t.Errorf("must not create after a forbidden get in namespace mode; calls = %v", *calls)
	}
}
