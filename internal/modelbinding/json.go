package modelbinding

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
)

// BindJSON decodes and validates a JSON request, writing an HTTP error on failure.
// The returned error reports a failed response write; false with nil means the
// invalid request was handled successfully. Neither case should be processed further.
func BindJSON[T any](w http.ResponseWriter, r *http.Request) (*T, bool, error) {
	requestID := middleware.GetRequestID(r.Context())
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return nil, false, httpresponse.WriteProblem(w, http.StatusUnsupportedMediaType, requestID, nil)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var request *T
	err = decoder.Decode(&request)
	if err == nil && request != nil {
		var extra json.RawMessage
		if trailing := decoder.Decode(&extra); trailing != io.EOF {
			err = trailing
			if err == nil {
				err = errors.New("multiple JSON values")
			}
		}
	}
	if err != nil || request == nil {
		var tooLarge *http.MaxBytesError
		var timeout net.Error
		if errors.As(err, &tooLarge) {
			return nil, false, httpresponse.WriteProblem(w, http.StatusRequestEntityTooLarge, requestID, nil)
		} else if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, false, httpresponse.WriteProblem(w, http.StatusRequestTimeout, requestID, nil)
		} else {
			return nil, false, httpresponse.WriteProblem(w, http.StatusBadRequest, requestID, map[string][]string{
				"body": {"A single JSON object is required."},
			})
		}
	}
	if validator, ok := any(request).(interface{ Validate() map[string][]string }); ok {
		if fields := validator.Validate(); len(fields) != 0 {
			return nil, false, httpresponse.WriteProblem(w, http.StatusBadRequest, requestID, fields)
		}
	}
	return request, true, nil
}
