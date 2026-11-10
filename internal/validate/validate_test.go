package validate

import "testing"

// Table-driven tests are the standard Go style: a slice of cases, one loop.
func TestName(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "my-project", false},
		{"with dash", "my-project-payments", false},
		{"with digits", "team42", false},
		{"all digits", "2024", false},
		{"empty", "", true},
		{"uppercase", "my-project", true},
		{"underscore", "my-project_payments", true},
		{"leading dash", "-my-project", true},
		{"trailing dash", "my-project-", true},
		{"space", "my-project payments", true},
		{"dot", "my-project.payments", true},
		{"slash", "my-project/payments", true},
		{"63 chars ok", long[:63], false},
		{"64 chars too long", long, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Name(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Name(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestGroupRoot(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"default", "/harbor/project", false},
		{"single segment", "/projects", false},
		{"with dots and dashes", "/org.name/team-a", false},
		{"empty", "", true},
		{"just slash", "/", true},
		{"no leading slash", "harbor/project", true},
		{"trailing slash", "/harbor/project/", true},
		{"empty segment", "/harbor//project", true},
		{"space", "/harbor/my project", true},
		{"segment starts with dash", "/harbor/-project", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := GroupRoot(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GroupRoot(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
