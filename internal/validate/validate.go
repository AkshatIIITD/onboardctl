// Package validate holds small input checks that run BEFORE we touch any
// external system. Failing early with a clear message is much nicer than
// getting a confusing 400 from Harbor or a rejection from OpenShift halfway
// through onboarding.
package validate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const maxNameLen = 63

var (
	// One name is used for the Harbor project, the OpenShift namespace AND a
	// Keycloak group path segment, so it must satisfy the strictest of those
	// rules: a Kubernetes "DNS-1123 label" (lowercase letters, digits, '-',
	// max 63 chars, must start and end with a letter or digit).
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	// One segment of a group path such as /harbor/project.
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Name checks the shared project / namespace name.
func Name(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("name %q is too long (%d chars, max %d)", name, len(name), maxNameLen)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q is invalid: use lowercase letters, digits and '-', and start/end with a letter or digit", name)
	}
	return nil
}

// GroupRoot checks the Keycloak group path under which each project's groups
// are created, for example "/harbor/project". The full group for a project is
// <root>/<project>/<role>.
func GroupRoot(root string) error {
	if !strings.HasPrefix(root, "/") {
		return fmt.Errorf("group root %q must start with '/'", root)
	}
	if strings.HasSuffix(root, "/") {
		return fmt.Errorf("group root %q must not end with '/'", root)
	}
	for _, segment := range strings.Split(root[1:], "/") {
		if !segmentRe.MatchString(segment) {
			return fmt.Errorf("group root %q has an invalid segment %q: use letters, digits, '.', '_' or '-'", root, segment)
		}
	}
	return nil
}
