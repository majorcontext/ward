package server

import (
	"encoding/json"
	"net/http"

	"github.com/majorcontext/ward/wardapi"
)

type code = wardapi.Code

const (
	codeUnauthorized     = wardapi.CodeUnauthorized
	codeForbidden        = wardapi.CodeForbidden
	codeNotFound         = wardapi.CodeNotFound
	codeInvalidBody      = wardapi.CodeInvalidBody
	codeMethodNotAllowed = wardapi.CodeMethodNotAllowed
	codeInternal         = wardapi.CodeInternal
	codeLandConflict     = wardapi.CodeLandConflict
	codeLandInvalid      = wardapi.CodeLandInvalid
	codeRefNotAllowed    = wardapi.CodeRefNotAllowed
	codeLandIDInUse      = wardapi.CodeLandIDInUse
	codeLandTimeout      = wardapi.CodeLandTimeout
	codeLeaseLost        = wardapi.CodeLeaseLost
)

var statuses = map[code]int{
	codeUnauthorized:     http.StatusUnauthorized,
	codeForbidden:        http.StatusForbidden,
	codeNotFound:         http.StatusNotFound,
	codeInvalidBody:      http.StatusBadRequest,
	codeMethodNotAllowed: http.StatusMethodNotAllowed,
	codeInternal:         http.StatusInternalServerError,
	codeLandConflict:     http.StatusConflict,
	codeLandInvalid:      http.StatusBadRequest,
	codeRefNotAllowed:    http.StatusForbidden,
	codeLandIDInUse:      http.StatusConflict,
	codeLandTimeout:      http.StatusGatewayTimeout,
	codeLeaseLost:        http.StatusGone,
}

// writeError is the only writer of an error body.
func writeError(w http.ResponseWriter, c code, message string) { writeErrorDetails(w, c, message, nil) }

func writeErrorDetails(w http.ResponseWriter, c code, message string, details map[string]any) {
	writeJSON(w, statuses[c], wardapi.ErrorBody{Error: wardapi.Error{Code: c, Message: message, Details: details}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
