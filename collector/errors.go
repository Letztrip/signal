package main

import (
	"encoding/json"
	"net/http"
)

type errEnvelope struct {
	Error errBody `json:"error"`
}

type errBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeErr(w http.ResponseWriter, status int, code, message, reqID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errEnvelope{Error: errBody{
		Code:      code,
		Message:   message,
		RequestID: reqID,
	}})
}
