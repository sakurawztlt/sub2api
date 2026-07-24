package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeStreamReqMeta_DerivesCodeExecutionFallbackArgs(t *testing.T) {
	body := []byte(`{
		"container":"container_existing",
		"tools":[{"name":"code_execution","type":"code_execution_20250522"}],
		"messages":[{
			"role":"user",
			"content":[{"type":"text","text":"Write and execute a Python script that prints 'HELLO_CHECK'. Only use the code execution tool, nothing else."}]
		}]
	}`)

	meta := computeStreamReqMeta(nil, body, "cache-key", "", "", "", 12)

	assert.JSONEq(t, `{"code":"print(\"HELLO_CHECK\")"}`, meta.CodeExecutionFallbackArgs)
	assert.True(t, meta.CodeExecutionEnabled)
	assert.Equal(t, "container_existing", meta.CodeExecutionContainerID)
	assert.Equal(t, 1, meta.ToolsCount)
	assert.Equal(t, 1, meta.MessagesCount)
}
