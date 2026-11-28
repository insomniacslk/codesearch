# Test data

## `index-format-1`

A codesearch index in format 1, the format that `cindex` wrote up to v1.2.0.
The library that `cs` uses now writes format 2 only, so the tests cannot build
a format 1 index themselves, and this file is what keeps format 1 covered.

The index holds three files under the root `/fixture`. The files do not exist
on disk: the tests use the index for what it stores (roots, names, posting
lists), not for the content of the files.

To build it again, put `index-format-1.go.txt` as `main.go` into an empty
module that requires `github.com/google/codesearch v1.2.0`, and run it with
the output path as the argument.
