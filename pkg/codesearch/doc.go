// Package codesearch searches code across several kinds of backend: the GitHub
// and GitLab search APIs, and local trees indexed with google/codesearch's
// cindex. Each backend is built from its configuration with NewBackend and
// returns its matches as Results.
package codesearch
