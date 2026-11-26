package codesearch

import (
	"strings"
	"testing"
)

func TestResolveTokenLiteral(t *testing.T) {
	p := BackendParams{"token": "ghp_literaltoken"}
	got, err := p.ResolveToken()
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if want := "ghp_literaltoken"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveTokenFromEnvironment(t *testing.T) {
	t.Setenv("CS_TEST_TOKEN", "from-the-environment")
	for _, ref := range []string{"${CS_TEST_TOKEN}", "$CS_TEST_TOKEN"} {
		t.Run(ref, func(t *testing.T) {
			p := BackendParams{"token": ref}
			got, err := p.ResolveToken()
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if want := "from-the-environment"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestResolveTokenLeavesLiteralsAlone checks that only a whole-value reference
// is expanded, so a token that happens to contain a $ is not mangled.
func TestResolveTokenLeavesLiteralsAlone(t *testing.T) {
	t.Setenv("CS_TEST_TOKEN", "should-not-be-used")
	for _, literal := range []string{
		"abc$CS_TEST_TOKEN",
		"$CS_TEST_TOKEN-suffix",
		"gh$p_token",
		"$",
		"${}",
		"a$b$c",
	} {
		t.Run(literal, func(t *testing.T) {
			p := BackendParams{"token": literal}
			got, err := p.ResolveToken()
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if got != literal {
				t.Errorf("got %q, want it left as %q", got, literal)
			}
		})
	}
}

func TestResolveTokenMissingEnvironmentVariable(t *testing.T) {
	p := BackendParams{"token": "${CS_TEST_DEFINITELY_UNSET}"}
	_, err := p.ResolveToken()
	if err == nil {
		t.Fatal("got nil error, want a failure for an unset variable")
	}
	if !strings.Contains(err.Error(), "CS_TEST_DEFINITELY_UNSET") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestResolveTokenEmptyEnvironmentVariable(t *testing.T) {
	t.Setenv("CS_TEST_EMPTY", "")
	p := BackendParams{"token": "${CS_TEST_EMPTY}"}
	if _, err := p.ResolveToken(); err == nil {
		t.Fatal("got nil error, want a failure for an empty variable")
	}
}

func TestResolveTokenFromCommand(t *testing.T) {
	p := BackendParams{"token_command": "printf 'secret-from-helper\\n'"}
	got, err := p.ResolveToken()
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if want := "secret-from-helper"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolveTokenCommandTrimsOnlyTrailingNewlines checks that a secret with
// meaningful leading or internal whitespace survives.
func TestResolveTokenCommandTrimsOnlyTrailingNewlines(t *testing.T) {
	for _, tt := range []struct {
		cmdline, want string
	}{
		{`printf 'tok\n'`, "tok"},
		{`printf 'tok\n\n'`, "tok"},
		{`printf 'tok\r\n'`, "tok"},
		{`printf 'tok'`, "tok"},
		{`printf ' tok '`, " tok "},
	} {
		t.Run(tt.cmdline, func(t *testing.T) {
			p := BackendParams{"token_command": tt.cmdline}
			got, err := p.ResolveToken()
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTokenCommandTakesPrecedence(t *testing.T) {
	p := BackendParams{
		"token":         "ignored-literal",
		"token_command": "printf 'from-helper'",
	}
	got, err := p.ResolveToken()
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if want := "from-helper"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveTokenCommandFailure(t *testing.T) {
	p := BackendParams{"token_command": "echo 'no such secret' >&2; exit 1"}
	_, err := p.ResolveToken()
	if err == nil {
		t.Fatal("got nil error, want a failure")
	}
	// the helper's own message is the useful part of the diagnosis
	if !strings.Contains(err.Error(), "no such secret") {
		t.Errorf("error %q does not carry the command's stderr", err)
	}
}

func TestResolveTokenCommandEmptyOutput(t *testing.T) {
	p := BackendParams{"token_command": "true"}
	if _, err := p.ResolveToken(); err == nil {
		t.Fatal("got nil error, want a failure for a helper that printed nothing")
	}
}

func TestResolveTokenMissingEntirely(t *testing.T) {
	p := BackendParams{"org": "someorg"}
	_, err := p.ResolveToken()
	if err == nil {
		t.Fatal("got nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "token_command") {
		t.Errorf("error %q does not mention the alternative", err)
	}
}

// TestBackendsResolveTokens checks the backends actually go through the
// resolver rather than reading the raw parameter.
func TestBackendsResolveTokens(t *testing.T) {
	t.Setenv("CS_TEST_TOKEN", "resolved-token")
	for _, tt := range []struct {
		name   string
		params BackendParams
		build  func(BackendParams) (Backend, error)
		token  func(Backend) string
	}{
		{
			name: "github",
			params: BackendParams{
				"org": "o", "api_endpoint": "https://api.github.com",
				"token": "${CS_TEST_TOKEN}",
			},
			build: func(p BackendParams) (Backend, error) { return (&Github{}).New("t", p) },
			token: func(b Backend) string { return b.(*Github).token },
		},
		{
			name: "gitlab",
			params: BackendParams{
				"api_endpoint":  "https://gitlab.example.com/api/v4",
				"token_command": "printf 'resolved-token'",
			},
			build: func(p BackendParams) (Backend, error) { return (&Gitlab{}).New("t", p) },
			token: func(b Backend) string { return b.(*Gitlab).token },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := tt.build(tt.params)
			if err != nil {
				t.Fatalf("failed to build backend: %v", err)
			}
			if got, want := tt.token(b), "resolved-token"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestValidateRejectsUnimplementedType covers bitbucket passing validation and
// failing only once a search tried to instantiate it.
func TestValidateRejectsUnimplementedType(t *testing.T) {
	c := Config{Backends: map[string]BackendConfig{"bb": {Type: BackendTypeBitbucket}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("got nil error, want a failure for bitbucket")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error %q does not say the type is not implemented", err)
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	c := Config{Backends: map[string]BackendConfig{"x": {Type: "svn"}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("got nil error, want a failure for an unknown type")
	}
	if !strings.Contains(err.Error(), `unknown backend type "svn"`) {
		t.Errorf("error %q does not report the type as unknown", err)
	}
}

func TestValidateAcceptsImplementedTypes(t *testing.T) {
	for _, typ := range []BackendType{BackendTypeGithub, BackendTypeGitlab, BackendTypeCsearch} {
		c := Config{Backends: map[string]BackendConfig{"x": {Type: typ}}}
		if err := c.Validate(); err != nil {
			t.Errorf("type %q: got error %v, want nil", typ, err)
		}
	}
}

// TestValidateBackendNamesIgnoreCase covers a default backend written with
// different case from the key under backends, which viper lowercases.
func TestValidateBackendNamesIgnoreCase(t *testing.T) {
	c := Config{
		DefaultBackends: []string{"GitHub_Me"},
		Backends:        map[string]BackendConfig{"github_me": {Type: BackendTypeCsearch}},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("got error %v, want nil", err)
	}
	for _, name := range []string{"all", "ALL", "All"} {
		c := Config{Backends: map[string]BackendConfig{name: {Type: BackendTypeCsearch}}}
		if err := c.Validate(); err == nil {
			t.Errorf("%q: got nil error, want the name to be reserved", name)
		}
	}
	c = Config{
		DefaultBackends: []string{"ALL", "x"},
		Backends:        map[string]BackendConfig{"x": {Type: BackendTypeCsearch}},
	}
	if err := c.Validate(); err == nil {
		t.Error("got nil error, want ALL combined with other backends to fail")
	}
}
