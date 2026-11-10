package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// RobotSpec describes a project-level robot account that can pull and push
// images in one project.
type RobotSpec struct {
	Name         string // short name, e.g. "pull-push"; Harbor makes it robot$<project>+<name>
	Project      string
	Description  string
	DurationDays int // 0 or less = never expires
}

// RobotCredential is what Harbor returns ONCE, when the robot is created.
// The secret can never be fetched again. String() redacts it so that it does
// not leak into logs by accident (%v, %s, %+v and %#v all stay safe).
type RobotCredential struct {
	Username string // full name, e.g. robot$new-project+pull-push
	Secret   string
}

func (r RobotCredential) String() string   { return r.Username + ":<redacted>" }
func (r RobotCredential) GoString() string { return r.String() }

// EnsureRobot creates the robot if it does not exist yet. created=true means
// the returned credential is valid and must be stored by the caller NOW.
// created=false means the robot already existed and no secret is available.
func (c *Client) EnsureRobot(ctx context.Context, spec RobotSpec) (cred RobotCredential, created bool, err error) {
	duration := -1 // Harbor: -1 = never expires
	if spec.DurationDays > 0 {
		duration = spec.DurationDays
	}
	body := robotRequest{
		Name:        spec.Name,
		Description: spec.Description,
		Duration:    duration,
		Level:       "project",
		Permissions: []robotPermission{{
			Kind:      "project",
			Namespace: spec.Project,
			Access: []robotAccess{
				{Resource: "repository", Action: "pull"},
				{Resource: "repository", Action: "push"},
			},
		}},
	}

	path := "/api/v2.0/robots"
	status, resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return RobotCredential{}, false, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var out struct {
			Name   string `json:"name"`
			Secret string `json:"secret"`
		}
		// Never put resp in an error: it contains the secret.
		if json.Unmarshal(resp, &out) != nil || out.Name == "" || out.Secret == "" {
			return RobotCredential{}, false, errors.New("harbor: robot was created but the response had no name/secret; delete the robot in Harbor and retry")
		}
		return RobotCredential{Username: out.Name, Secret: out.Secret}, true, nil
	case http.StatusConflict:
		return RobotCredential{}, false, nil
	default:
		return RobotCredential{}, false, newAPIError(http.MethodPost, path, status, resp)
	}
}

type robotRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Duration    int               `json:"duration"`
	Level       string            `json:"level"`
	Disable     bool              `json:"disable"`
	Permissions []robotPermission `json:"permissions"`
}

type robotPermission struct {
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace"`
	Access    []robotAccess `json:"access"`
}

type robotAccess struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}
