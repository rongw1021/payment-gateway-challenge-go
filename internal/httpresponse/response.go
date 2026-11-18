package httpresponse

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

func WriteProblem(w http.ResponseWriter, status int, requestID string, fields map[string][]string) {
	WriteProblemDetails(w, models.ProblemDetails{
		Status: status, RequestID: requestID, Errors: fields,
	})
}

func WriteProblemDetails(w http.ResponseWriter, problem models.ProblemDetails) {
	if problem.Title == "" {
		problem.Title = http.StatusText(problem.Status)
		if problem.Errors != nil {
			problem.Title = "One or more validation errors occurred."
		}
	}
	WriteJSON(w, problem.Status, "application/problem+json", problem)
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

func WriteJSON(w http.ResponseWriter, status int, contentType string, value interface{}) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("Encode HTTP response: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("Write HTTP response: %v", err)
	}
}
