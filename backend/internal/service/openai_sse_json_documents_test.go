package service

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitOpenAIConcatenatedJSONDocuments(t *testing.T) {
	first := `{"type":"response.in_progress","sequence_number":1}`
	second := `{"type":"response.completed","sequence_number":2}`

	documents, repaired := splitOpenAIConcatenatedJSONDocuments([]byte(first + second))

	require.True(t, repaired)
	require.Len(t, documents, 2)
	require.JSONEq(t, first, string(documents[0]))
	require.JSONEq(t, second, string(documents[1]))
}

func TestSplitOpenAIConcatenatedJSONDocumentsRejectsMalformedTail(t *testing.T) {
	documents, repaired := splitOpenAIConcatenatedJSONDocuments(
		[]byte(`{"type":"response.in_progress"}unexpected-tail`),
	)

	require.False(t, repaired)
	require.Nil(t, documents)
}

func TestOpenAISSEJSONDocumentScannerExpandsConcatenatedDataLine(t *testing.T) {
	first := `{"type":"response.in_progress","sequence_number":1}`
	second := `{"type":"response.completed","sequence_number":2}`
	scanner := bufio.NewScanner(strings.NewReader("event: response.in_progress\ndata: " + first + second + "\n\n"))
	documents := newOpenAISSEJSONDocumentScanner(scanner)

	var lines []string
	for documents.Scan() {
		lines = append(lines, documents.Text())
	}

	require.NoError(t, documents.Err())
	require.Equal(t, []string{
		"event: response.in_progress",
		"data: " + first,
		"",
		"event: response.completed",
		"data: " + second,
		"",
		"",
	}, lines)
	for _, line := range lines {
		data, ok := extractOpenAISSEDataLine(line)
		if ok {
			require.True(t, json.Valid([]byte(data)))
		}
	}
}

func TestSplitOpenAIConcatenatedJSONDocumentsRejectsDocumentWithoutType(t *testing.T) {
	documents, repaired := splitOpenAIConcatenatedJSONDocuments(
		[]byte(`{"type":"response.in_progress"}{"sequence_number":2}`),
	)

	require.False(t, repaired)
	require.Nil(t, documents)
}
