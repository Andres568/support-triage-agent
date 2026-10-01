# Cloud model registry ids (internal/providers/registry.go) -> the provider
# slug that names the API key secret (<slug>-api-key) and its env variable
# (<SLUG>_API_KEY). Local (ollama) and fake models are not deployable.
# internal/providers/terraform_test.go fails CI when this drifts from the
# registry.
locals {
  model_providers = {
    "claude-opus-5-5"           = "anthropic"
    "claude-sonnet-5-5"         = "anthropic"
    "claude-haiku-4-5-20251001" = "anthropic"
    "gpt-6.1-sol"               = "openai"
    "gpt-6-luna"                = "openai"
    "grok-4.7"                  = "xai"
    "grok-4.3"                  = "xai"
  }
}
