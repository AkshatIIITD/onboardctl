// Package keycloak is a tiny client for the Keycloak Admin REST API. It only
// does one job: make sure a group path such as /harbor/project/my-project/admin
// exists in a realm, creating any missing parent groups on the way.
//
// Authentication uses a confidential client with "Service accounts enabled"
// (OAuth2 client-credentials grant). The service account needs the
// realm-management role "manage-users" (which covers groups) in the target realm.
package keycloak

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
	"sync"
	"time"
)

// Config holds everything needed to build a Client.
type Config struct {
	// BaseURL is the Keycloak root, e.g. https://sso.example.com
	// (older Keycloak versions need the /auth suffix: https://sso.example.com/auth).
	BaseURL string
	// Realm is the REAL realm name (the one in the URL), which can differ from
	// the display name shown in the admin console.
	Realm string
	// TokenRealm is the realm the service-account client lives in. Defaults to Realm.
	TokenRealm   string
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client // optional; build one with httpx.NewClient for a custom CA
}

// Client talks to one Keycloak realm.
type Client struct {
	baseURL      string
	realm        string
	tokenRealm   string
	clientID     string
	clientSecret string
	http         *http.Client

	mu    sync.Mutex
	token string
}

// New builds a Client. It does not contact Keycloak yet.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("keycloak: base URL is required")
	}
	if cfg.Realm == "" {
		return nil, errors.New("keycloak: realm is required")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("keycloak: client ID and client secret are required")
	}
	tokenRealm := cfg.TokenRealm
	if tokenRealm == "" {
		tokenRealm = cfg.Realm
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		realm:        cfg.Realm,
		tokenRealm:   tokenRealm,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		http:         httpClient,
	}, nil
}

// EnsureGroupPath makes sure the group at groupPath exists, creating every
// missing level. groupPath must look like "/a/b/c". It returns created=true
// if this call created the final group, false if it already existed.
func (c *Client) EnsureGroupPath(ctx context.Context, groupPath string) (created bool, err error) {
	segments, err := splitPath(groupPath)
	if err != nil {
		return false, err
	}

	parentID := "" // "" means top level
	current := ""
	for i, name := range segments {
		current += "/" + name
		last := i == len(segments)-1

		id, found, err := c.groupIDByPath(ctx, current)
		if err != nil {
			return false, err
		}
		if found {
			parentID = id
			continue
		}

		id, err = c.createGroup(ctx, parentID, name)
		if errors.Is(err, errGroupExists) {
			// Lost a race with someone else; look the group up again.
			id, found, err = c.groupIDByPath(ctx, current)
			if err != nil {
				return false, err
			}
			if !found {
				return false, fmt.Errorf("keycloak: group %q reported as existing but not found", current)
			}
			parentID = id
			continue
		}
		if err != nil {
			return false, err
		}
		parentID = id
		if last {
			created = true
		}
	}
	return created, nil
}

func splitPath(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") || p == "/" {
		return nil, fmt.Errorf("keycloak: group path %q must start with '/' and name at least one group", p)
	}
	segments := strings.Split(p[1:], "/")
	for _, s := range segments {
		if s == "" {
			return nil, fmt.Errorf("keycloak: group path %q has an empty segment", p)
		}
	}
	return segments, nil
}

var errGroupExists = errors.New("group already exists")

func (c *Client) groupIDByPath(ctx context.Context, groupPath string) (id string, found bool, err error) {
	// Each segment is escaped separately so the '/' separators stay literal.
	parts := strings.Split(groupPath[1:], "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	apiPath := c.adminPath("/group-by-path/" + strings.Join(parts, "/"))

	status, body, err := c.doAuthed(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", false, err
	}
	switch status {
	case http.StatusOK:
		var g struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &g); err != nil || g.ID == "" {
			return "", false, fmt.Errorf("keycloak: unexpected response for group %q", groupPath)
		}
		return g.ID, true, nil
	case http.StatusNotFound:
		return "", false, nil
	default:
		return "", false, newAPIError(http.MethodGet, apiPath, status, body)
	}
}

// createGroup creates a top-level group (parentID == "") or a child group, and
// returns the new group's ID taken from the Location header.
func (c *Client) createGroup(ctx context.Context, parentID, name string) (string, error) {
	apiPath := c.adminPath("/groups")
	if parentID != "" {
		apiPath = c.adminPath("/groups/" + url.PathEscape(parentID) + "/children")
	}

	status, body, hdr, err := c.send(ctx, http.MethodPost, apiPath, map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated:
		loc := hdr.Get("Location")
		id := loc[strings.LastIndex(loc, "/")+1:]
		if loc == "" || id == "" {
			return "", fmt.Errorf("keycloak: group %q created but no Location header returned", name)
		}
		return id, nil
	case http.StatusConflict:
		return "", errGroupExists
	default:
		return "", newAPIError(http.MethodPost, apiPath, status, body)
	}
}

func (c *Client) adminPath(suffix string) string {
	return "/admin/realms/" + url.PathEscape(c.realm) + suffix
}

// doAuthed sends an authenticated request, refreshing the token once on 401.
func (c *Client) doAuthed(ctx context.Context, method, path string, body any) (int, []byte, error) {
	status, data, _, err := c.send(ctx, method, path, body)
	return status, data, err
}

func (c *Client) send(ctx context.Context, method, path string, body any) (int, []byte, http.Header, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.getToken(ctx, attempt > 0)
		if err != nil {
			return 0, nil, nil, err
		}
		status, data, hdr, err := c.rawDo(ctx, method, path, body, tok)
		if err != nil {
			return 0, nil, nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			continue // token probably expired; fetch a new one and retry once
		}
		return status, data, hdr, nil
	}
	return 0, nil, nil, errors.New("keycloak: unreachable")
}

func (c *Client) rawDo(ctx context.Context, method, path string, body any, token string) (int, []byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("reading response of %s %s: %w", method, path, err)
	}
	return resp.StatusCode, data, resp.Header, nil
}

// getToken returns the cached access token, fetching one if needed or if
// refresh is true. Errors never include the client secret.
func (c *Client) getToken(ctx context.Context, refresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && !refresh {
		return c.token, nil
	}

	tokenPath := "/realms/" + url.PathEscape(c.tokenRealm) + "/protocol/openid-connect/token"
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak: requesting token: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("keycloak: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "check KEYCLOAK_CLIENT_ID / KEYCLOAK_CLIENT_SECRET, that the client has 'Service accounts enabled', and the base URL (older Keycloak needs a /auth suffix)"
		return "", fmt.Errorf("keycloak: token request to %s returned HTTP %d (%s)", tokenPath, resp.StatusCode, msg)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("keycloak: token response had no access_token")
	}
	c.token = t.AccessToken
	return c.token, nil
}

// APIError is returned when Keycloak answers with an unexpected status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("keycloak: %s %s returned HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("keycloak: %s %s returned HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

func newAPIError(method, path string, status int, body []byte) *APIError {
	msg := strings.TrimSpace(string(body))
	var parsed struct {
		ErrorMessage string `json:"errorMessage"`
		Error        string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		switch {
		case parsed.ErrorMessage != "":
			msg = parsed.ErrorMessage
		case parsed.Error != "":
			msg = parsed.Error
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if status == http.StatusForbidden {
		msg = strings.TrimSpace(msg + " (the service account likely lacks the realm-management role 'manage-users')")
	}
	return &APIError{Method: method, Path: path, Status: status, Message: msg}
}
