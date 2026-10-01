# openairesp fixtures

- `*.response.json` and `*.request.golden.json` without the `ollama_` prefix are
  **hand-authored** from the OpenAI Responses API docs as of 2026-09-29. They pin
  our understanding of the wire format, not a provider's actual behavior.
- `ollama_qwen3_8b.*` is **recorded** from a real local Ollama (qwen3:8b,
  `reasoning.effort: none`) with:

      go test ./internal/providers/openairesp -run TestOllamaLive -record

  (`OLLAMA_BASE_URL` overrides `http://127.0.0.1:11434/v1`). The offline test
  `TestOllamaRecorded` replays it.
- `go test ./internal/providers/openairesp -update` rewrites the request goldens
  from the current code. Review the diff: a golden change is a wire change.
