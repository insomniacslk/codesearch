package codesearch

import (
	"context"
	"fmt"
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
	// Limit stops the backend once it has produced this many results. Zero
	// means fetch everything. It exists because the remote backends are
	// metered: GitHub allows 30 code searches a minute, and paging through
	// every result of a search whose output is about to be truncated spends
	// that quota for nothing. Only the caller knows whether truncating early
	// is safe, so it is opt-in rather than derived from the printed limit.
	Limit uint
	// OnResult, if set, is called with each result as soon as the backend
	// has it, so that a caller can show results while a long search is still
	// running. It is optional for a backend to call it. One that does calls
	// it from a single goroutine, in the order of the results that Search
	// then returns, of which the results passed to OnResult are a prefix.
	// When Search fails, the results already passed on stay valid.
	OnResult func(Result)
}

// emit passes a result to OnResult, if there is one.
func (o SearchOptions) emit(r Result) {
	if o.OnResult != nil {
		o.OnResult(r)
	}
}

type Backend interface {
	Name() string
	Type() BackendType
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

// backendConstructors is the single list of backend types that have an
// implementation. Config.Validate consults it too, so a type that is known to
// the config format but missing here is rejected when the config is loaded
// rather than when a search first tries to use it.
var backendConstructors = map[BackendType]func(name string, params BackendParams) (Backend, error){
	BackendTypeGithub:  asBackend(NewGithub),
	BackendTypeGitlab:  asBackend(NewGitlab),
	BackendTypeCsearch: asBackend(NewCsearch),
}

// asBackend adapts a concrete constructor to the table. On failure it returns
// an untyped nil: passing the constructor's nil *T through as a Backend would
// give an interface that is not == nil.
func asBackend[T Backend](newFn func(string, BackendParams) (T, error)) func(string, BackendParams) (Backend, error) {
	return func(name string, params BackendParams) (Backend, error) {
		b, err := newFn(name, params)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

// NewBackend builds the backend called name from its configuration.
func NewBackend(name string, cfg BackendConfig) (Backend, error) {
	newFn, ok := backendConstructors[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("backend %q: type %q is not implemented", name, cfg.Type)
	}
	b, err := newFn(name, cfg.Params)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate backend %q: %w", name, err)
	}
	return b, nil
}
