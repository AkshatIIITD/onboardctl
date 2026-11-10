// Package openshift creates the team's namespace by running the `oc` CLI.
//
// Shelling out to `oc` (instead of using client-go) keeps the tool dependency-free
// and reuses your existing `oc login` session. It deliberately does NOT create
// ResourceQuotas or LimitRanges: those usually need cluster-admin rights.
package openshift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CreateMode selects how the namespace is created.
type CreateMode string

const (
	// ModeProject uses `oc new-project`. This goes through the ProjectRequest
	// API, so it works with the "self-provisioner" role and also makes you the
	// project admin. It is the right default on OpenShift.
	ModeProject CreateMode = "project"
	// ModeNamespace uses `oc create namespace`. Needs permission to create
	// Namespace objects directly (usually cluster-admin).
	ModeNamespace CreateMode = "namespace"
)

// ParseCreateMode converts a flag value into a CreateMode.
func ParseCreateMode(s string) (CreateMode, error) {
	switch CreateMode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeProject:
		return ModeProject, nil
	case ModeNamespace:
		return ModeNamespace, nil
	default:
		return "", fmt.Errorf("invalid openshift mode %q (use %q or %q)", s, ModeProject, ModeNamespace)
	}
}

// ErrNameCollision means the name is taken by something you cannot see.
// OpenShift hides projects you have no access to, so "get" says NotFound
// while "create" says AlreadyExists.
var ErrNameCollision = errors.New("name is already taken by a project/namespace you cannot see")

// RunFunc runs a command (feeding it stdin, which may be nil) and returns its
// combined stdout+stderr. It exists so tests can replace the real `oc` with a
// fake. Secrets are sent on stdin, never as arguments, so they do not show up
// in `ps` output.
type RunFunc func(ctx context.Context, stdin []byte, binary string, args ...string) (output string, err error)

// OC runs the `oc` binary.
type OC struct {
	Binary  string  // path or name of the binary, default "oc"
	Context string  // optional kubeconfig context (--context)
	Run     RunFunc // optional; defaults to really executing the binary

	// How long EnsureCICD waits for the token controller to fill the token
	// secret. Defaults: 1s interval, 30s timeout.
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// EnsureNamespace makes sure the namespace exists. created=true means this
// call created it. An existing namespace is left untouched.
func (o *OC) EnsureNamespace(ctx context.Context, name string, mode CreateMode) (created bool, err error) {
	kind := "project"
	if mode == ModeNamespace {
		kind = "namespace"
	}

	out, err := o.run(ctx, nil, "get", kind, name, "-o", "name")
	if err == nil {
		return false, nil
	}
	// "Forbidden" on `get project` is expected for users who may create projects
	// but not list or read them: OpenShift answers Forbidden for any project you
	// have no access to, whether or not it exists. So in project mode we try to
	// create it and let "AlreadyExists" tell us about a name collision.
	cannotRead := mode == ModeProject && containsFold(out, "forbidden")
	if !cannotRead && !containsFold(out, "notfound", "not found") {
		// Login expired, no access to the cluster, oc missing, etc. Do not
		// try to create anything; show what oc said.
		return false, fmt.Errorf("checking %s %q: %w\n%s", kind, name, err, out)
	}

	var createArgs []string
	switch mode {
	case ModeProject:
		// --skip-config-write stops oc from switching your current kube context.
		createArgs = []string{"new-project", name, "--skip-config-write"}
	case ModeNamespace:
		createArgs = []string{"create", "namespace", name}
	default:
		return false, fmt.Errorf("unknown openshift mode %q", mode)
	}

	out, err = o.run(ctx, nil, createArgs...)
	if err != nil {
		if containsFold(out, "alreadyexists", "already exists") {
			return false, fmt.Errorf("%q: %w", name, ErrNameCollision)
		}
		return false, fmt.Errorf("creating %s %q: %w\n%s", kind, name, err, out)
	}
	return true, nil
}

// run adds the optional --context flag and dispatches to Run or exec.
func (o *OC) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	binary := o.Binary
	if binary == "" {
		binary = "oc"
	}
	if o.Context != "" {
		args = append(args, "--context", o.Context)
	}
	run := o.Run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, stdin, binary, args...)
	return strings.TrimSpace(out), err
}

// execRun really runs the command. exec.CommandContext kills it on Ctrl+C or
// timeout, and arguments are passed as a slice, never through a shell, so a
// strange name cannot inject commands.
func execRun(ctx context.Context, stdin []byte, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func containsFold(s string, needles ...string) bool {
	s = strings.ToLower(s)
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
