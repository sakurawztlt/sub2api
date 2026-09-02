package repository

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newSessionIDUsageLog(sessionID *string) *service.UsageLog {
	return &service.UsageLog{
		UserID:       1,
		APIKeyID:     2,
		AccountID:    3,
		RequestID:    "req-session-id",
		Model:        "claude-3",
		InputTokens:  10,
		OutputTokens: 5,
		TotalCost:    1,
		ActualCost:   1,
		SessionID:    sessionID,
		CreatedAt:    time.Now().UTC(),
	}
}

func TestPrepareUsageLogInsertSessionIDArgWiring(t *testing.T) {
	sessionID := "sess-persisted-123"
	prepared := prepareUsageLogInsert(newSessionIDUsageLog(&sessionID))
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	// NativeCompactionV2 and CreatedAt follow SessionID in the insert argument list.
	sessionArg, ok := prepared.args[len(prepared.args)-3].(sql.NullString)
	require.True(t, ok)
	require.Equal(t, sql.NullString{String: sessionID, Valid: true}, sessionArg)
	require.Equal(t, "text", usageLogInsertArgTypes[len(usageLogInsertArgTypes)-3])
}

func TestPrepareUsageLogInsertSessionIDNullWhenAbsent(t *testing.T) {
	prepared := prepareUsageLogInsert(newSessionIDUsageLog(nil))
	require.Equal(t, sql.NullString{}, prepared.args[len(prepared.args)-3])

	empty := ""
	prepared = prepareUsageLogInsert(newSessionIDUsageLog(&empty))
	require.Equal(t, sql.NullString{}, prepared.args[len(prepared.args)-3])
}

func TestPrepareUsageLogInsertRequestedReasoningEffortArgWiring(t *testing.T) {
	requested := "max"
	forwarded := "xhigh"
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:                   1,
		APIKeyID:                 2,
		AccountID:                3,
		RequestID:                "req-requested-effort",
		Model:                    "gpt-5.4",
		ReasoningEffort:          &forwarded,
		RequestedReasoningEffort: &requested,
		CreatedAt:                time.Now().UTC(),
	})

	require.Len(t, prepared.args, len(usageLogInsertArgTypes))
	require.Equal(t, "text", usageLogInsertArgTypes[47])
	require.Equal(t, "text", usageLogInsertArgTypes[48])

	forwardedArg, ok := prepared.args[47].(sql.NullString)
	require.True(t, ok)
	require.Equal(t, sql.NullString{String: forwarded, Valid: true}, forwardedArg)

	requestedArg, ok := prepared.args[48].(sql.NullString)
	require.True(t, ok)
	require.Equal(t, sql.NullString{String: requested, Valid: true}, requestedArg)
}

func TestUsageLogInsertQueriesIncludeSessionID(t *testing.T) {
	require.Contains(t, usageLogSelectColumns, "session_id")

	sessionID := "sess-in-query"
	log := newSessionIDUsageLog(&sessionID)
	prepared := prepareUsageLogInsert(log)
	key := usageLogBatchKey(log.RequestID, log.APIKeyID)

	batchQuery, batchArgs := buildUsageLogBatchInsertQuery(
		[]string{key},
		map[string]usageLogInsertPrepared{key: prepared},
	)
	require.GreaterOrEqual(t, strings.Count(batchQuery, "session_id"), 3)
	require.Len(t, batchArgs, len(prepared.args)+1)

	bestEffortQuery, bestEffortArgs := buildUsageLogBestEffortInsertQuery(
		[]usageLogInsertPrepared{prepared},
	)
	require.Contains(t, bestEffortQuery, "session_id")
	require.Len(t, bestEffortArgs, len(prepared.args))
}
