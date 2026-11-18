package server

import (
	"encoding/json"
	"net/http"
)

type code string

const (
	codeUnauthorized     code = "unauthorized"
	codeForbidden        code = "forbidden"
	codeNotFound         code = "not_found"
	codeInvalidBody      code = "invalid_body"
	codeMethodNotAllowed code = "method_not_allowed"
	codeInternal         code = "internal_error"
)

var statuses = map[code]int{
	codeUnauthorized:     http.StatusUnauthorized,
	codeForbidden:        http.StatusForbidden,
	codeNotFound:         http.StatusNotFound,
	codeInvalidBody:      http.StatusBadRequest,
	codeMethodNotAllowed: http.StatusMethodNotAllowed,
	codeInternal:         http.StatusInternalServerError,
}

type errorBody struct {
	Error struct {
		Code    code           `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// writeError is the only writer of an error body.
func writeError(w http.ResponseWriter, c code, message string) {
	var b errorBody
	b.Error.Code, b.Error.Message = c, message
	writeJSON(w, statuses[c], b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
