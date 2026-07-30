package codesearch

import (
	"context"
	"time"
)

// Capability is an optional search feature. Backends declare the ones they
// actually implement so the CLI can reject options that would otherwise be
// accepted and then silently ignored.
type Capability string

const (
	CapCaseInsensitive   Capability = "case-insensitive search"
	CapSearchInFilenames Capability = "searching in file names"
)

// SearchOptions are the per-search settings a backend honours. They are passed
// by value so that a backend holds nothing but its configuration: several
// backends are searched concurrently, and options that were applied by mutating
// the backend had to be written before the search read them, which is an
// ordering the type could not enforce.
type SearchOptions struct {
	LinesBefore       int
	LinesAfter        int
	CaseInsensitive   bool
	SearchInFilenames bool
	// Timeout bounds a single backend. Zero means no deadline of its own.
	Timeout time.Duration
}

type Backend interface {
	New(name string, params BackendParams) (Backend, error)
	Name() string
	Type() string
	Supports(c Capability) bool
	Search(ctx context.Context, terms string, opts SearchOptions) (Results, error)
}

// searchContext bounds a single search with the backend timeout, on top of
// whatever deadline or cancellation the caller already imposes. A non-positive
// timeout means no deadline of its own.
func searchContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func BackendByType(t BackendType) Backend {
	switch t {
	case BackendTypeGithub:
		return &Github{}
	case BackendTypeGitlab:
		return &Gitlab{}
		/*
			case BackendTypeBitbucket:
				return &Bitbucket{}
		*/
	case BackendTypeCsearch:
		return &Csearch{}
	default:
		return nil
	}
}
