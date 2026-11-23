package httpresponse

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failedResponseWriter struct {
	header   http.Header
	statuses []int
	writes   int
	err      error
}

func (w *failedResponseWriter) Header() http.Header    { return w.header }
func (w *failedResponseWriter) WriteHeader(status int) { w.statuses = append(w.statuses, status) }
func (w *failedResponseWriter) Write([]byte) (int, error) {
	w.writes++
	return 1, w.err
}

func TestResponseHelpersReturnWriteFailuresWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(http.ResponseWriter) error
	}{
		{"JSON", func(w http.ResponseWriter) error {
			return WriteJSON(w, 200, "application/json", map[string]string{"result": "ok"})
		}},
		{"problem", func(w http.ResponseWriter) error { return WriteProblem(w, 400, "request", nil) }},
		{"problem details", func(w http.ResponseWriter) error {
			return WriteProblemDetails(w, models.ProblemDetails{Status: 400, RequestID: "request"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, failure := range []error{io.ErrClosedPipe, nil} {
				writer := &failedResponseWriter{header: make(http.Header), err: failure}
				err := test.write(writer)
				if failure == nil {
					assert.ErrorIs(t, err, io.ErrShortWrite)
				} else {
					assert.ErrorIs(t, err, failure)
				}
				require.Len(t, writer.statuses, 1)
				assert.NotEqual(t, http.StatusInternalServerError, writer.statuses[0])
				assert.Equal(t, 1, writer.writes)
			}
		})
	}
}

func TestEncodingFailureReturnsErrorAndSafeProblem(t *testing.T) {
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-Id", "request")
	response.Header().Set("Content-Length", "1")
	response.Header().Set("Content-Encoding", "gzip")
	response.Header().Set("Location", "/success-only")
	err := WriteJSON(response, http.StatusOK, "application/json", make(chan int))
	var encodingError *json.UnsupportedTypeError
	require.ErrorAs(t, err, &encodingError)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	assert.Empty(t, response.Header().Get("Content-Length"))
	assert.Empty(t, response.Header().Get("Content-Encoding"))
	assert.Empty(t, response.Header().Get("Location"))
	var problem models.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, 500, problem.Status)
	assert.Equal(t, "request", problem.RequestID)
	assert.Empty(t, problem.Detail)
	assert.NotContains(t, response.Body.String(), "chan")

	broken := &failedResponseWriter{header: make(http.Header), err: io.ErrClosedPipe}
	err = WriteJSON(broken, http.StatusOK, "application/json", make(chan int))
	assert.ErrorAs(t, err, &encodingError)
	assert.ErrorIs(t, err, io.ErrClosedPipe)
	assert.Equal(t, []int{http.StatusInternalServerError}, broken.statuses)
	assert.Equal(t, 1, broken.writes)
}
