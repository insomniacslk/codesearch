package codesearch

type Backend interface {
	New(name string, params BackendParams) (Backend, error)
	Name() string
	Type() string
	SetLinesBefore(n int)
	SetLinesAfter(n int)
	SetCaseInsensitive(v bool)
	SetSearchInFilenames(v bool)
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
