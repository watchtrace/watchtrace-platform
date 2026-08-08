package httpapi

import "github.com/gin-gonic/gin"

// ErrorResponse is the common JSON envelope for API errors.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains a stable machine code, a safe user-facing message, and
// the request ID used to correlate the response with server logs.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// RespondError aborts the current request with a structured, safe API error.
// Callers must pass messages that contain no secrets or internal error details.
func RespondError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{
		Error: ErrorDetail{
			Code:      code,
			Message:   message,
			RequestID: RequestID(c),
		},
	})
}
