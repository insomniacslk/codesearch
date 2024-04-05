# codesearch

Command line code search tool that uses different backends, like GitHub, GitLab,
and soon local search via
[`google/codesearch`](https://github.com/google/codesearch).

## Configuration

Create a file called `config.yml` in your configdir:
* on Linux, `~/.config/cs/config.yml`
* on macOS, `~/Library/Application Support/cs/config.yml`

And use the content of the provided `config.yml.example` with your own
repository configuration and tokens.

## Features

* [x] basic search on github
* [x] basic search on gitlab
* [ ] basic search on local code with google/codesearch
* [x] handle rate limiting on github API
* [ ] handle rate limiting on gitlab API
* [ ] implement response caching via last-modified headers
* [*] fetch full file on larger context for github
* [ ] fetch full file on larger context for gitlab
* [ ] common syntax for all backends
* [*] file name search for gitlab
* [ ] file name search for github
