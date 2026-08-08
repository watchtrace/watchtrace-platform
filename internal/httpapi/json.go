package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

const maxJSONBodyBytes int64 = 1 << 20

// DecodeJSON validates and decodes one JSON object. It rejects unsupported
// media types, oversized bodies, unknown fields, trailing JSON values, and
// failed Gin binding validations with the same safe error response.
func DecodeJSON(c *gin.Context, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		RespondError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		RespondError(c, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		RespondError(c, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value")
		return false
	}
	if err := binding.Validator.ValidateStruct(destination); err != nil {
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "request body failed validation")
		return false
	}

	return true
}
