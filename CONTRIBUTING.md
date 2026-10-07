# Contributing to Lazarus

Thank you for your interest in contributing to **Lazarus**! We welcome contributions ranging from bug fixes and engine enhancements to documentation and test coverage improvements.

## Design Philosophy

Before making architectural changes, please keep these core tenets in mind:

1. **"Disposability is the prerequisite for verification"**: We never touch real or existing databases. Sandboxes must be completely isolated, throwaway containers or ephemeral copies.
2. **Minimal Dependencies**: We prefer pure Go implementations over external heavy CLI bindings where feasible (e.g., our AWS SigV4 S3 client requires no `aws-cli`).
3. **Deterministic Assertions**: Checks assert single numeric scalars to ensure unambiguous pass/fail boundaries.
4. **Zero Lingering State**: Failed or successful drills must cleanly destroy all intermediate resources unless explicitly instructed otherwise (`--keep-on-failure`).

## Development Workflow

### Prerequisites

- **Go 1.24+**
- **Docker** (required for end-to-end container restore tests: Postgres, MySQL, Redis)
- **sqlite3** CLI (for local SQLite tests)

### Common Commands

We provide a root [`Makefile`](Makefile) for development:

```bash
make help        # Show available development commands
make build       # Compile lazarus and lazarus-server binaries
make test        # Run all package unit tests
make test-race   # Run unit tests with Go race detector enabled
make lint        # Run go vet static analysis
make fmt         # Format source code with gofmt
```

### Quick Verification

To verify the test suite and formatting locally:

```bash
make fmt && make lint && make test-race
```

You can also run the 5-second zero-dependency demo:

```bash
./examples/quickstart.sh
```

## Pull Request Guidelines

1. **Branching**: Create a topic branch from `main` (e.g., `feat/my-new-feature` or `fix/s3-timeout`).
2. **Test Coverage**: Any new feature, bug fix, or edge case must include automated unit tests under the corresponding package.
3. **Code Quality**: Ensure `make test-race` and `make lint` pass without warnings.
4. **Commit Messages**: Follow the [Conventional Commits](https://www.conventionalcommits.org/) convention:
   - `feat:` for new capabilities
   - `fix:` for bug fixes
   - `docs:` for documentation updates
   - `refactor:` for code changes that neither fix bugs nor add features
   - `test:` for adding or adjusting tests
