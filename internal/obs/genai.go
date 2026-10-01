// Package obs is the workers' observability: Cloud-Logging-compatible JSON
// logs, OpenTelemetry traces with the GenAI conventions, and per-item run
// rows in Postgres.
//
// Prompts, completions, ticket text and email addresses are never span or
// log attributes: traces leave the database's access controls.
package obs

import "go.opentelemetry.io/otel/attribute"

// OpenTelemetry GenAI semantic conventions (status: Development). The Go
// semconv package dropped these keys, so they are defined here.
const (
	GenAIOperationName = attribute.Key("gen_ai.operation.name")
	GenAIProviderName  = attribute.Key("gen_ai.provider.name")
	GenAIRequestModel  = attribute.Key("gen_ai.request.model")
	GenAIResponseModel = attribute.Key("gen_ai.response.model")
	GenAIResponseID    = attribute.Key("gen_ai.response.id")
	GenAIFinishReasons = attribute.Key("gen_ai.response.finish_reasons") // string slice
	GenAIUsageInput    = attribute.Key("gen_ai.usage.input_tokens")      // total incl. cache
	GenAIUsageOutput   = attribute.Key("gen_ai.usage.output_tokens")
	GenAIToolName      = attribute.Key("gen_ai.tool.name")
	GenAIToolCallID    = attribute.Key("gen_ai.tool.call.id")
	ErrorType          = attribute.Key("error.type")
	AppCostMicros      = attribute.Key("app.cost_usd_micros")
	AppSubject         = attribute.Key("app.subject")
	AppAttempt         = attribute.Key("app.attempt")
	AppOutcome         = attribute.Key("app.outcome")
	AppModel           = attribute.Key("app.model") // registry id of the run's model
)

// Operation names (gen_ai.operation.name).
const (
	OpChat        = "chat"
	OpExecuteTool = "execute_tool"
)
