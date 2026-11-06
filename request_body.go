package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/renezander030/draftcat/internal/strictjson"
)

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "unreadable body", http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
}

// readStrictJSON reads a gate endpoint's JSON body of at most limit bytes into
// v. A body it cannot use is answered here, 413 when too large and 400 when
// unreadable, malformed or ambiguous, and it returns false.
func readStrictJSON(w http.ResponseWriter, r *http.Request, limit int64, v interface{}) bool {
	body, ok := readRequestBody(w, r, limit)
	if !ok {
		return false
	}
	if err := decodeStrictJSON(body, v); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return false
	}
	return true
}

// decodeStrictJSON preserves number tokens and rejects ambiguous envelopes.
func decodeStrictJSON(body []byte, target interface{}) error {
	return strictjson.Decode(body, target)
}
