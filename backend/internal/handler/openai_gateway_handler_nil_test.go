package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIGatewayHandlerMessagesNilReceiverReturnsSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	var handler *OpenAIGatewayHandler

	handler.Messages(c)

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, "error", gjson.GetBytes(recorder.Body.Bytes(), "type").String())
	require.Equal(t, "api_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
	require.Equal(t, "Internal server error", gjson.GetBytes(recorder.Body.Bytes(), "error.message").String())
}
