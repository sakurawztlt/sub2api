package securityaudit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestCloneKeepsLegacyAndPromptBodiesIndependent(t *testing.T) {
	request := Request{
		Protocol:   "openai_responses",
		Body:       []byte(`{"instructions":"original-instructions","input":"full-transcript"}`),
		LegacyBody: []byte(`{"input":"legacy-normalized-only"}`),
	}
	cloned := request.Clone()
	snapshot, err := ExtractPromptSnapshot(cloned)
	require.NoError(t, err)
	require.Contains(t, snapshot.ScanText, "original-instructions")
	require.Contains(t, snapshot.ScanText, "full-transcript")
	require.NotContains(t, snapshot.ScanText, "legacy-normalized-only")
	cloned.Body[0] = '!'
	cloned.LegacyBody[0] = '!'
	require.Equal(t, byte('{'), request.Body[0])
	require.Equal(t, byte('{'), request.LegacyBody[0])
}
