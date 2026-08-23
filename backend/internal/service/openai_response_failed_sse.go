package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func buildOpenAIResponseFailedSSE(responseID, model string, source []byte, fallbackMessage string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		responseID = "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	errorType := strings.TrimSpace(gjson.GetBytes(source, "error.type").String())
	if errorType == "" {
		errorType = strings.TrimSpace(gjson.GetBytes(source, "response.error.type").String())
	}
	code := strings.TrimSpace(gjson.GetBytes(source, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(source, "response.error.code").String())
	}
	if code == "" {
		code = "upstream_error"
	}
	message := extractOpenAISSEErrorMessage(source)
	if message == "" {
		message = strings.TrimSpace(fallbackMessage)
	}
	if message == "" {
		message = "Upstream response failed"
	}
	errorBody := gin.H{"code": code, "message": message}
	if errorType != "" {
		errorBody["type"] = errorType
	}
	response := gin.H{
		"id":     responseID,
		"object": "response",
		"status": "failed",
		"output": []any{},
		"error":  errorBody,
	}
	if model = strings.TrimSpace(model); model != "" {
		response["model"] = model
	}
	payload, err := marshalOpenAIUpstreamJSON(gin.H{
		"type":     "response.failed",
		"response": response,
	})
	if err != nil {
		payload = []byte(`{"type":"response.failed","response":{"status":"failed","output":[],"error":{"code":"upstream_error","message":"Upstream response failed"}}}`)
	}
	return "event: response.failed\ndata: " + string(payload) + "\n\n"
}
