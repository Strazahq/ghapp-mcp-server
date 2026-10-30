.PHONY: hooks check fmt vet test doc security

# Bind the hooks once per clone. They live in the tree so they travel.
hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/*
	@echo "hooks bound: pre-commit and commit-msg run from .githooks"

# What CI runs on every pull request, by hand.
check: fmt vet doc test

# gofmt -l exits 0 even when it lists files, so test its output.
fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "fmt: run gofmt -w on the files above"; exit 1; }

vet:
	go vet ./...

doc:
	go run ./tools/doccheck ./...

# The unit tests, with the race detector. The git tests run real git.
test:
	go test -race ./...

# Secrets in the history, and known vulnerabilities in the code and modules.
security:
	gitleaks git --no-banner --redact .
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
