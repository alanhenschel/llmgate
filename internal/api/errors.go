package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the OpenAI error envelope.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: typ, Code: code}})
}

func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
}
