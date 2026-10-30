# Contributing

Open an issue before a pull request that adds a tool or a setting. Their
names are public interface, so they are settled first.

## Setup

You need Go, git and openssl. go.mod pins the Go toolchain, and the go
command fetches it when the installed Go is older. `make security` also
needs gitleaks, from its
[releases](https://github.com/gitleaks/gitleaks/releases).

```sh
make hooks
```

`make hooks` binds the git hooks in .githooks. The commit hook refuses Go
files that gofmt would change, and the commit message hook refuses an AI
attribution trailer. A pull request runs `make check`, govulncheck and the
demo in CI.

## Tests

`make check` runs gofmt, go vet, the doc comment check and the tests with
the race detector. The git tests run real git against a fake GitHub, so
git must be on the PATH. `./demo/run.sh` runs the whole flow offline
against stand-ins for GitHub, Straza and Straza's holder, and prints a
transcript. Tests are table-driven, and a test of a refusal quotes its
sentence word for word.

## Commits and pull requests

- The subject is at most 72 characters, says in the imperative what
  changed, and has no em dash. The body says why.
- A commit carries no AI attribution trailer. The commit message hook
  refuses one.
- A pull request makes one change, and every changed line traces to it.
- A change that users see gets a line in CHANGELOG.md under Unreleased.

## Writing

Comments, messages and docs use full sentences and plain words, one idea
per sentence, and no em dashes. A doc comment starts with the name it
documents; `make doc` checks that. Every message to an operator says what
failed, why, and what to do next. A refusal to an agent is one sentence,
with a code in `_meta`.
