// Package onboard turns one Config into an ordered list of Steps and runs them.
//
// It never talks to Harbor, Keycloak or OpenShift directly. It depends on three
// small interfaces that the real clients happen to satisfy. That is the Go way
// to make code testable: define the interface where it is USED, keep it tiny,
// and swap in a fake in tests.
package onboard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/AkshatIIITD/onboardctl/internal/harbor"
	"github.com/AkshatIIITD/onboardctl/internal/openshift"
	"github.com/AkshatIIITD/onboardctl/internal/validate"
)

// HarborAPI is what onboarding needs from Harbor. *harbor.Client satisfies it.
type HarborAPI interface {
	EnsureProject(ctx context.Context, spec harbor.ProjectSpec) (created bool, err error)
	EnsureGroupMember(ctx context.Context, project string, group harbor.Group, role harbor.Role) (created bool, err error)
	EnsureRobot(ctx context.Context, spec harbor.RobotSpec) (cred harbor.RobotCredential, created bool, err error)
}

// KeycloakAPI is what onboarding needs from Keycloak. *keycloak.Client satisfies it.
type KeycloakAPI interface {
	EnsureGroupPath(ctx context.Context, groupPath string) (created bool, err error)
}

// Cluster is what onboarding needs from OpenShift. *openshift.OC satisfies it.
type Cluster interface {
	EnsureNamespace(ctx context.Context, name string, mode openshift.CreateMode) (created bool, err error)
	SecretExists(ctx context.Context, ns, name string) (bool, error)
	ApplyPullSecret(ctx context.Context, ns, name, registry, user, password string) error
	LinkPullSecret(ctx context.Context, ns, serviceAccount, secret string) error
	EnsureCICD(ctx context.Context, ns, serviceAccount, tokenSecret string) error
}

// Defaults for the per-project resources. Only the robot lifetime has a flag.
const (
	DefaultRobotName      = "pull-push"
	DefaultPullSecretName = "harbor-pull"
	DefaultCICDAccount    = "ci-cd"
	DefaultCICDTokenName  = "ci-cd-token"
	pullServiceAccount    = "default" // pods use this SA unless a Deployment sets another
)

// Secrets collects values that are shown only once (today: the Harbor robot
// secret). Steps add to it; the caller prints or saves it AFTER Execute, also
// when Execute failed, because these values can never be fetched again.
type Secrets struct{ items []secretItem }

type secretItem struct{ label, value string }

// Add records one value.
func (s *Secrets) Add(label, value string) {
	if s != nil {
		s.items = append(s.items, secretItem{label, value})
	}
}

// Empty reports whether nothing was collected.
func (s *Secrets) Empty() bool { return s == nil || len(s.items) == 0 }

// Lines returns "label: value" lines for printing or saving.
func (s *Secrets) Lines() []string {
	if s == nil {
		return nil
	}
	lines := make([]string, 0, len(s.items))
	for _, it := range s.items {
		lines = append(lines, it.label+": "+it.value)
	}
	return lines
}

// RoleGroup maps a group name suffix to the Harbor role it receives.
type RoleGroup struct {
	Suffix string
	Role   harbor.Role
}

// RoleGroups are the four groups created for every project, in this order.
var RoleGroups = []RoleGroup{
	{"admin", harbor.RoleProjectAdmin},
	{"maintainer", harbor.RoleMaintainer},
	{"developer", harbor.RoleDeveloper},
	{"guest", harbor.RoleGuest},
}

// Config is everything the user tells us about the new project.
type Config struct {
	Name             string // used for the Harbor project, the namespace and the group paths
	GroupRoot        string // e.g. "/harbor/project"
	HarborStorageGiB int    // 0 = unlimited
	OpenShiftMode    openshift.CreateMode

	HarborRegistry     string // host[:port] used in image names, e.g. harbor.example.com
	RobotName          string // short robot name, e.g. "pull-push"
	RobotDurationDays  int    // 0 = never expires
	PullSecretName     string // image pull secret created in the namespace
	CICDServiceAccount string // service account for pipelines
	CICDTokenSecret    string // long-lived token secret of that service account
}

// GroupPath returns the full group path for one role, e.g.
// "/harbor/project/new-project/admin". The same string is the Keycloak group path and
// the Harbor group name (Harbor sees it as the token's group claim).
func (c Config) GroupPath(suffix string) string {
	return c.GroupRoot + "/" + c.Name + "/" + suffix
}

// Validate checks the whole config before anything is created.
func (c Config) Validate() error {
	if err := validate.Name(c.Name); err != nil {
		return err
	}
	if err := validate.GroupRoot(c.GroupRoot); err != nil {
		return err
	}
	if c.HarborStorageGiB < 0 {
		return fmt.Errorf("harbor-storage-gib must not be negative, got %d", c.HarborStorageGiB)
	}
	if _, err := openshift.ParseCreateMode(string(c.OpenShiftMode)); err != nil {
		return err
	}
	for label, v := range map[string]string{
		"robot name": c.RobotName, "pull secret name": c.PullSecretName,
		"ci-cd service account": c.CICDServiceAccount, "ci-cd token secret": c.CICDTokenSecret,
	} {
		if err := validate.Name(v); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	if c.HarborRegistry != "" && (strings.Contains(c.HarborRegistry, "/") || strings.Contains(c.HarborRegistry, " ")) {
		return fmt.Errorf("harbor registry %q must be a host[:port] without scheme or path", c.HarborRegistry)
	}
	if c.RobotDurationDays < 0 {
		return fmt.Errorf("robot-days must not be negative, got %d", c.RobotDurationDays)
	}
	return nil
}

// Step is one unit of work. Preview says what it WOULD do (used by -dry-run),
// Run actually does it and returns a short result for the log.
type Step struct {
	Name    string
	Preview string
	Run     func(ctx context.Context) (string, error)
}

// BuildPlan validates the config and returns the steps in the order they run:
//
//  1. OpenShift namespace
//  2. four Keycloak groups
//  3. Harbor project
//  4. four Harbor group memberships
//  5. Harbor robot account (secret goes into secrets, shown once)
//  6. image pull secret in the namespace, linked to the default service account
//  7. ci-cd service account with edit access and a long-lived token secret
//
// In dry-run mode h, k and c may be nil, because Run is never called.
// secrets may be nil if the caller does not need the one-time values.
func BuildPlan(cfg Config, h HarborAPI, k KeycloakAPI, c Cluster, secrets *Secrets) ([]Step, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	storage := "unlimited storage"
	if cfg.HarborStorageGiB > 0 {
		storage = fmt.Sprintf("%d GiB storage limit", cfg.HarborStorageGiB)
	}

	steps := []Step{{
		Name:    "OpenShift: ensure " + string(cfg.OpenShiftMode),
		Preview: fmt.Sprintf("ensure %s %q exists (oc, mode=%s)", cfg.OpenShiftMode, cfg.Name, cfg.OpenShiftMode),
		Run: func(ctx context.Context) (string, error) {
			created, err := c.EnsureNamespace(ctx, cfg.Name, cfg.OpenShiftMode)
			if err != nil {
				return "", err
			}
			return outcome(created), nil
		},
	}}

	for _, rg := range RoleGroups {
		steps = append(steps, keycloakGroupStep(k, cfg.GroupPath(rg.Suffix)))
	}

	steps = append(steps, Step{
		Name:    "Harbor: ensure project",
		Preview: fmt.Sprintf("ensure private project %q exists (%s)", cfg.Name, storage),
		Run: func(ctx context.Context) (string, error) {
			created, err := h.EnsureProject(ctx, harbor.ProjectSpec{
				Name:            cfg.Name,
				StorageLimitGiB: cfg.HarborStorageGiB,
			})
			if err != nil {
				return "", err
			}
			return outcome(created), nil
		},
	})

	for _, rg := range RoleGroups {
		steps = append(steps, harborGroupStep(h, cfg.Name, cfg.GroupPath(rg.Suffix), rg.Role))
	}

	// The robot step hands its credential to the pull-secret step through this
	// variable. It is empty when the robot already existed (no secret to reuse).
	var robot harbor.RobotCredential
	registry := cfg.HarborRegistry
	if registry == "" {
		registry = "(host of -harbor-url)"
	}

	steps = append(steps,
		Step{
			Name:    "Harbor: ensure robot account " + cfg.RobotName,
			Preview: fmt.Sprintf("ensure robot %q in project %q with repository pull+push (%s); its secret is shown once at the end", cfg.RobotName, cfg.Name, lifetime(cfg.RobotDurationDays)),
			Run: func(ctx context.Context) (string, error) {
				cred, created, err := h.EnsureRobot(ctx, harbor.RobotSpec{
					Name:         cfg.RobotName,
					Project:      cfg.Name,
					Description:  "Created by onboardctl: push/pull images for " + cfg.Name,
					DurationDays: cfg.RobotDurationDays,
				})
				if err != nil {
					return "", err
				}
				if !created {
					return "already exists, left unchanged (its secret cannot be read back)", nil
				}
				robot = cred
				secrets.Add("Harbor robot username", cred.Username)
				secrets.Add("Harbor robot secret", cred.Secret)
				return "created as " + cred.Username + " (secret is shown once, at the end)", nil
			},
		},
		Step{
			Name:    "OpenShift: ensure image pull secret " + cfg.PullSecretName,
			Preview: fmt.Sprintf("create secret %q in %q for registry %s using the robot credential", cfg.PullSecretName, cfg.Name, registry),
			Run: func(ctx context.Context) (string, error) {
				if robot.Secret != "" {
					if err := c.ApplyPullSecret(ctx, cfg.Name, cfg.PullSecretName, cfg.HarborRegistry, robot.Username, robot.Secret); err != nil {
						return "", err
					}
					return "created", nil
				}
				exists, err := c.SecretExists(ctx, cfg.Name, cfg.PullSecretName)
				if err != nil {
					return "", err
				}
				if exists {
					return "already exists, left unchanged", nil
				}
				return "", fmt.Errorf("robot %q already existed, so its secret is unknown, and secret %q is missing in %q: delete the robot in Harbor (project %s > Robot Accounts) and re-run", cfg.RobotName, cfg.PullSecretName, cfg.Name, cfg.Name)
			},
		},
		Step{
			Name:    fmt.Sprintf("OpenShift: link %s to service account %s for pull", cfg.PullSecretName, pullServiceAccount),
			Preview: fmt.Sprintf("oc secrets link %s %s --for=pull -n %s", pullServiceAccount, cfg.PullSecretName, cfg.Name),
			Run: func(ctx context.Context) (string, error) {
				if err := c.LinkPullSecret(ctx, cfg.Name, pullServiceAccount, cfg.PullSecretName); err != nil {
					return "", err
				}
				return "linked", nil
			},
		},
		Step{
			Name:    "OpenShift: ensure ci-cd service account, edit role and token",
			Preview: fmt.Sprintf("ensure service account %q, a RoleBinding to the \"edit\" ClusterRole, and the long-lived token secret %q in %q", cfg.CICDServiceAccount, cfg.CICDTokenSecret, cfg.Name),
			Run: func(ctx context.Context) (string, error) {
				if err := c.EnsureCICD(ctx, cfg.Name, cfg.CICDServiceAccount, cfg.CICDTokenSecret); err != nil {
					return "", err
				}
				return fmt.Sprintf("ready. Token (read it when you need it): oc get secret %s -n %s -o jsonpath='{.data.token}' | base64 -d", cfg.CICDTokenSecret, cfg.Name), nil
			},
		},
	)
	return steps, nil
}

func lifetime(days int) string {
	if days <= 0 {
		return "never expires"
	}
	return fmt.Sprintf("expires after %d days", days)
}

func keycloakGroupStep(k KeycloakAPI, path string) Step {
	return Step{
		Name:    fmt.Sprintf("Keycloak: ensure group %q", path),
		Preview: fmt.Sprintf("ensure group path %q exists (parents are created as needed)", path),
		Run: func(ctx context.Context) (string, error) {
			created, err := k.EnsureGroupPath(ctx, path)
			if err != nil {
				return "", err
			}
			return outcome(created), nil
		},
	}
}

func harborGroupStep(h HarborAPI, project, group string, role harbor.Role) Step {
	return Step{
		Name:    fmt.Sprintf("Harbor: grant %s to group %q", role, group),
		Preview: fmt.Sprintf("add OIDC group %q to project %q as %s", group, project, role),
		Run: func(ctx context.Context) (string, error) {
			created, err := h.EnsureGroupMember(ctx, project, harbor.Group{Name: group, Type: harbor.GroupTypeOIDC}, role)
			if err != nil {
				return "", err
			}
			if !created {
				return "already a member, left unchanged", nil
			}
			return "granted", nil
		},
	}
}

func outcome(created bool) string {
	if created {
		return "created"
	}
	return "already exists, left unchanged"
}

// Execute runs the steps in order and stops at the first failure.
// With dryRun set it only prints what would happen and changes nothing.
func Execute(ctx context.Context, steps []Step, dryRun bool, out io.Writer) error {
	for i, s := range steps {
		if err := ctx.Err(); err != nil {
			return err // cancelled (Ctrl+C)
		}
		label := fmt.Sprintf("[%d/%d] %s", i+1, len(steps), s.Name)

		if dryRun {
			fmt.Fprintf(out, "%s  (dry-run, nothing changed)\n%s\n\n", label, indent(s.Preview))
			continue
		}

		result, err := s.Run(ctx)
		if err != nil {
			fmt.Fprintf(out, "%s ... FAILED\n", label)
			// %w wraps the error so callers can still use errors.Is / errors.As.
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		fmt.Fprintf(out, "%s ... %s\n", label, result)
	}
	return nil
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}
