package service

// openAIResponsesUnsupportedFields are top-level Responses API fields that
// need to be stripped defensively before forwarding to OpenAI/Codex upstreams.
var openAIResponsesUnsupportedFields = []string{
	"prompt_cache_retention",
	"safety_identifier",
}
