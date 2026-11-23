package httpresponse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

func WriteProblem(w http.ResponseWriter, status int, requestID string, fields map[string][]string) error {
	return WriteProblemDetails(w, models.ProblemDetails{
		Status: status, RequestID: requestID, Errors: fields,
	})
}

func WriteProblemDetails(w http.ResponseWriter, problem models.ProblemDetails) error {
	if problem.Title == "" {
		problem.Title = http.StatusText(problem.Status)
		if problem.Errors != nil {
			problem.Title = "One or more validation errors occurred."
		}
	}
	return WriteJSON(w, problem.Status, "application/problem+json", problem)
}

func ObservedStatus(status int, completed bool) int {
	if status != 0 {
		return status
	}
	if !completed {
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

// WriteJSON writes one response. An encoding failure writes a safe 500 before any
// headers are sent. Callers should log returned errors, not attempt another response.
func WriteJSON(w http.ResponseWriter, status int, contentType string, value interface{}) error {
	body, err := json.Marshal(value)
	if err != nil {
		for _, header := range []string{"Content-Length", "Content-Encoding", "Location"} {
			w.Header().Del(header)
		}
		// The fallback contains only JSON-safe fields; its encoding cannot fail.
		return errors.Join(fmt.Errorf("encode HTTP response: %w", err),
			WriteProblem(w, http.StatusInternalServerError, w.Header().Get("X-Request-Id"), nil))
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err != nil {
		return fmt.Errorf("write HTTP response: %w", err)
	}
	if n != len(body) {
		return fmt.Errorf("write HTTP response: %w", io.ErrShortWrite)
	}
	return nil
}
