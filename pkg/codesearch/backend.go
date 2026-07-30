package codesearch

import (
	"context"
	"time"
)

type Backend interface {
	New(name string, params BackendParams) (Backend, error)
	Name() string
	Type() string
	SetLinesBefore(n int)
	SetLinesAfter(n int)
	SetCaseInsensitive(v bool)
	SetSearchInFilenames(v bool)
	SetTimeout(d time.Duration)
	Search(terms string, opts ...Opt) (Results, error)
}

type Opt func(b Backend)

func WithLinesBefore(n int) Opt {
	return func(b Backend) {
		b.SetLinesBefore(n)
	}
}

func WithLinesAfter(n int) Opt {
	return func(b Backend) {
		b.SetLinesAfter(n)
	}
}

func WithCaseInsensitive(v bool) Opt {
	return func(b Backend) {
		b.SetCaseInsensitive(v)
	}
}

func WithSearchInFilenames(v bool) Opt {
	return func(b Backend) {
		b.SetSearchInFilenames(v)
	}
}

func WithTimeout(d time.Duration) Opt {
	return func(b Backend) {
		b.SetTimeout(d)
	}
}

// searchContext returns a context bounding a single search. A non-positive
// timeout means no deadline.
func searchContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), timeout)
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
