package codesearch

import "fmt"

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

type BackendType string

const (
	BackendTypeGithub    = "github"
	BackendTypeGitlab    = "gitlab"
	BackendTypeBitbucket = "bitbucket"
	BackendTypeLocal     = "local"
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
	case string(BackendTypeLocal):
		return BackendTypeLocal
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
