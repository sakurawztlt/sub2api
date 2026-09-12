//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBatchImageModerationIncludesEveryPrompt(t *testing.T) {
	body, err := batchImageModerationBody(&service.BatchImageSubmitRequest{
		Items: []service.BatchImageSubmitItem{
			{CustomID: "first", Prompt: " first prompt "},
			{CustomID: "empty", Prompt: " "},
			{CustomID: "last", Prompt: "second prompt"},
		},
	})
	require.NoError(t, err)
	input := service.ExtractContentModerationInput(service.ContentModerationProtocolOpenAIImages, body)
	require.Contains(t, input.Text, "first prompt")
	require.Contains(t, input.Text, "second prompt")
}

func TestLiveModerationReadsSessionInstructions(t *testing.T) {
	body := liveModerationBody([]byte(`{"model":"gpt-live-test","instructions":"session instructions"}`))
	input := service.ExtractContentModerationInput(service.ContentModerationProtocolOpenAIResponses, body)
	require.Equal(t, "session instructions", input.Text)
}
