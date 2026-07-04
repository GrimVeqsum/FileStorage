package response

import (
	"encoding/json"
	"net/http"
)

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if data == nil {
		return
	}

	json.NewEncoder(w).Encode(data)
}

func Error(w http.ResponseWriter, status int, code string, message string) {
	JSON(w, status, ErrorResponse{
		Error:   code,
		Message: message,
	})
}
