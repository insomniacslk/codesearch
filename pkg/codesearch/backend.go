package codesearch

type Backend interface {
	New(name string, params BackendParams) (Backend, error)
	Name() string
	Type() string
	SetLinesBefore(n int)
	SetLinesAfter(n int)
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

func BackendByType(t BackendType) Backend {
	switch t {
	case BackendTypeGithub:
		return &Github{}
	case BackendTypeGitlab:
		return &Gitlab{}
		/*
			case BackendTypeBitbucket:
				return &Bitbucket{}
			case BackendTypeLocal:
				return &Local{}
		*/
	default:
		return nil
	}
}
