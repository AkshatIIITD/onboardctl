// Package harbor is a tiny client for the parts of the Harbor v2 REST API
// that onboarding needs: create a project, and grant a group access to it.
//
// It uses only the standard library (net/http + encoding/json). The API
// reference for your Harbor version is served by Harbor itself at
// https://<your-harbor>/devcenter-api-2.0
package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrAlreadyExists is returned (wrapped) when Harbor answers 409 Conflict.
// Callers check for it with errors.Is(err, harbor.ErrAlreadyExists).
var ErrAlreadyExists = errors.New("already exists")

// Role is a Harbor project role ID.
type Role int

const (
	RoleProjectAdmin Role = 1
	RoleDeveloper    Role = 2
	RoleGuest        Role = 3
	RoleMaintainer   Role = 4
)

func (r Role) String() string {
	switch r {
	case RoleProjectAdmin:
		return "project-admin"
	case RoleDeveloper:
		return "developer"
	case RoleGuest:
		return "guest"
	case RoleMaintainer:
		return "maintainer"
	default:
		return fmt.Sprintf("role(%d)", int(r))
	}
}

// GroupType tells Harbor where a group comes from.
type GroupType int

const (
	GroupTypeLDAP GroupType = 1
	GroupTypeHTTP GroupType = 2
	GroupTypeOIDC GroupType = 3 // what you get when Harbor logs in via Keycloak
)

// Group identifies a user group in Harbor. For OIDC groups the name is the
// value of the group claim in the token, e.g. "/harbor/project/new-project/admin".
type Group struct {
	Name string
	Type GroupType
}

// ProjectSpec describes the project we want to exist.
type ProjectSpec struct {
	Name            string
	StorageLimitGiB int // 0 means unlimited
}

// Config holds everything needed to build a Client.
type Config struct {
	BaseURL    string // e.g. https://harbor.internal.example
	Username   string
	Password   string       // prefer a robot or service account, not a human admin
	HTTPClient *http.Client // optional; build one with httpx.NewClient for a custom CA
}

// Client talks to one Harbor instance.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New builds a Client. It does not contact Harbor yet.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("harbor: base URL is required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("harbor: username and password are required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		username: cfg.Username,
		password: cfg.Password,
		http:     httpClient,
	}, nil
}

// EnsureProject makes sure a private project exists. It returns created=true
// if this call created it, and created=false if it was already there.
func (c *Client) EnsureProject(ctx context.Context, spec ProjectSpec) (created bool, err error) {
	exists, err := c.projectExists(ctx, spec.Name)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	err = c.createProject(ctx, spec)
	if errors.Is(err, ErrAlreadyExists) {
		// Someone created it between our check and our create. That's fine.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// EnsureGroupMember adds a group to a project with the given role.
// If the group is already a member, nothing is changed (including its role).
func (c *Client) EnsureGroupMember(ctx context.Context, project string, group Group, role Role) (created bool, err error) {
	path := "/api/v2.0/projects/" + url.PathEscape(project) + "/members"
	body := memberRequest{
		RoleID: int(role),
		MemberGroup: memberGroup{
			GroupName: group.Name,
			GroupType: int(group.Type),
		},
	}

	status, respBody, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return true, nil
	case http.StatusConflict:
		return false, nil
	default:
		return false, newAPIError(http.MethodPost, path, status, respBody)
	}
}

func (c *Client) projectExists(ctx context.Context, name string) (bool, error) {
	path := "/api/v2.0/projects?project_name=" + url.QueryEscape(name)
	status, body, err := c.do(ctx, http.MethodHead, path, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, newAPIError(http.MethodHead, path, status, body)
	}
}

func (c *Client) createProject(ctx context.Context, spec ProjectSpec) error {
	limit := int64(-1) // Harbor uses -1 for "unlimited"
	if spec.StorageLimitGiB > 0 {
		limit = int64(spec.StorageLimitGiB) << 30 // GiB -> bytes
	}
	body := projectRequest{
		ProjectName:  spec.Name,
		Public:       false,
		Metadata:     map[string]string{"public": "false"},
		StorageLimit: limit,
	}

	path := "/api/v2.0/projects"
	status, respBody, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("project %q: %w", spec.Name, ErrAlreadyExists)
	default:
		return newAPIError(http.MethodPost, path, status, respBody)
	}
}

// do sends one request and returns the status code and body. It does not
// decide what a status code means; each caller does that.
func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	// Harbor accepts a project name OR a numeric ID in the same URL position.
	// Without this header a project called "2024" would be read as ID 2024.
	req.Header.Set("X-Is-Resource-Name", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close() // always close response bodies

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // cap at 1 MiB
	if err != nil {
		return 0, nil, fmt.Errorf("reading response of %s %s: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}

// --- request/response shapes (JSON field names come from the Harbor API) ---

type projectRequest struct {
	ProjectName  string            `json:"project_name"`
	Public       bool              `json:"public"`
	Metadata     map[string]string `json:"metadata"`
	StorageLimit int64             `json:"storage_limit"`
}

type memberRequest struct {
	RoleID      int         `json:"role_id"`
	MemberGroup memberGroup `json:"member_group"`
}

type memberGroup struct {
	GroupName string `json:"group_name"`
	GroupType int    `json:"group_type"`
}

// APIError is returned when Harbor answers with an unexpected status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("harbor: %s %s returned HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("harbor: %s %s returned HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

func newAPIError(method, path string, status int, body []byte) *APIError {
	msg := strings.TrimSpace(string(body))

	// Harbor usually replies {"errors":[{"code":"...","message":"..."}]}.
	var parsed struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &parsed) == nil && len(parsed.Errors) > 0 {
		parts := make([]string, 0, len(parsed.Errors))
		for _, e := range parsed.Errors {
			parts = append(parts, e.Code+": "+e.Message)
		}
		msg = strings.Join(parts, "; ")
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		msg = strings.TrimSpace(msg + " (check HARBOR_USERNAME / HARBOR_PASSWORD and that the account may create projects, manage members and create robot accounts)")
	}
	return &APIError{Method: method, Path: path, Status: status, Message: msg}
}
