//go:build unit

package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyCurrentConcurrencyDTO(t *testing.T) {
	key := APIKeyFromService(&service.APIKey{ID: 8, CurrentConcurrency: 6})
	require.Equal(t, 6, key.CurrentConcurrency)
	encoded, err := json.Marshal(key)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"current_concurrency":6`)
}
