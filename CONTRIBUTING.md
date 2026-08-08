# Contributing to gh-vault

Contributions are welcome.

## Reporting Issues

Open an issue on GitHub. Include:

- What you expected to happen
- What actually happened
- Steps to reproduce
- Your environment (OS, Docker version, Go version)

## Suggesting Features

Open an issue with the `enhancement` label. Describe the problem you're trying to solve, not just the solution.

## Pull Requests

1. Fork the repo and create a branch from `main`
2. Make your changes
3. Run `go test ./...` and `go vet ./...`
4. Open a PR with a clear description of what changed and why

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Wrap errors with `fmt.Errorf("<pkg>: <context>: %w", err)`. Use the short package name as prefix (`web:`, `store:`, `backup:`, etc.).
- Use `slog` for structured logging
- Keep packages under `internal/` (not importable externally)
