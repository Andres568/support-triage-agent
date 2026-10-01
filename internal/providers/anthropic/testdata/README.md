# anthropic fixtures

- Every `*.response.json` and `*.request.golden.json` here is **hand-authored**
  from the Anthropic Messages API docs as of 2026-09-29. They pin our
  understanding of the wire format, not the provider's actual behavior; no
  exchange was recorded against the real API (that needs a paid key).
- `go test ./internal/providers/anthropic -update` rewrites the request goldens
  from the current code. Review the diff: a golden change is a wire change.
