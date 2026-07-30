package codesearch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	DefaultBackends []string                 `mapstructure:"default_backends"`
	Backends        map[string]BackendConfig `mapstructure:"backends"`
}

type BackendConfig struct {
	Type   BackendType   `mapstructure:"type"`
	Params BackendParams `mapstructure:"params"`
}

type BackendParams map[string]interface{}

func (b *BackendParams) Get(name string) interface{} {
	p := map[string]interface{}(*b)
	return p[name]
}

func (b *BackendParams) GetString(name string) *string {
	p := map[string]interface{}(*b)
	sp, ok := p[name]
	if !ok {
		return nil
	}
	s, ok := sp.(string)
	if !ok {
		return nil
	}
	return &s
}

// envRef matches a value that is exactly one environment variable reference,
// like ${GITHUB_TOKEN} or $GITHUB_TOKEN. Only a whole-value reference is
// expanded, so a literal token that happens to contain a $ is left alone
// rather than being silently mangled.
var envRef = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

// tokenCommandTimeout bounds the helper, so a command that waits for input
// cannot hang the search indefinitely.
const tokenCommandTimeout = 30 * time.Second

// ResolveToken returns the authentication token for a backend. It comes either
// from token_command, which is run through the shell and whose standard output
// is used, or from token, which may be a single environment variable
// reference. Both exist so that the secret does not have to be written into a
// config file that often ends up in a dotfiles repository.
func (b *BackendParams) ResolveToken() (string, error) {
	if cmdline := b.GetString("token_command"); cmdline != nil && *cmdline != "" {
		return runTokenCommand(*cmdline)
	}
	token := b.GetString("token")
	if token == nil {
		return "", fmt.Errorf("missing 'token' or 'token_command' parameter")
	}
	m := envRef.FindStringSubmatch(*token)
	if m == nil {
		return *token, nil
	}
	value, ok := os.LookupEnv(m[1])
	if !ok {
		return "", fmt.Errorf("token refers to environment variable %q, which is not set", m[1])
	}
	if value == "" {
		return "", fmt.Errorf("environment variable %q is empty", m[1])
	}
	return value, nil
}

func runTokenCommand(cmdline string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCommandTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("token_command %q failed: %w: %s", cmdline, err, msg)
		}
		return "", fmt.Errorf("token_command %q failed: %w", cmdline, err)
	}
	// helpers conventionally print the secret on its own line
	token := strings.TrimRight(string(out), "\r\n")
	if token == "" {
		return "", fmt.Errorf("token_command %q produced no output", cmdline)
	}
	return token, nil
}

type BackendType string

const (
	BackendTypeGithub    = "github"
	BackendTypeGitlab    = "gitlab"
	BackendTypeBitbucket = "bitbucket"
	BackendTypeCsearch   = "csearch"
	BackendTypeUnknown   = "unknown"
)

func BackendTypeByName(name string) BackendType {
	switch name {
	case string(BackendTypeGithub):
		return BackendTypeGithub
	case string(BackendTypeGitlab):
		return BackendTypeGitlab
	case string(BackendTypeBitbucket):
		return BackendTypeBitbucket
	case string(BackendTypeCsearch):
		return BackendTypeCsearch
	default:
		return BackendTypeUnknown
	}
}

func (c *Config) Validate() error {
	// ensure that default_backends is either "all" or a list of backend names
	for _, name := range c.DefaultBackends {
		if name == "all" {
			if len(c.DefaultBackends) > 1 {
				return fmt.Errorf("default_backends: all backends requested, but other backends are specified too")
			}
		} else {
			found := false
			for bname := range c.Backends {
				if name == bname {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("default_backends: unknown backend %q", name)
			}
		}
	}
	for name, backend := range c.Backends {
		if name == "all" {
			return fmt.Errorf("backend name 'all' is reserved")
		}
		if BackendTypeByName(string(backend.Type)) == BackendTypeUnknown {
			return fmt.Errorf("unknown backend type %q", backend.Type)
		}
	}
	return nil
}
