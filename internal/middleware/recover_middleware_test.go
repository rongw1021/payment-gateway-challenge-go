package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoverReturnsSafeProblemAndUpdatesRequestTelemetry(t *testing.T) {
	for _, value := range []any{"private-panic-value", errors.New("private-panic-value"), []string{"private-panic-value"}} {
		t.Run(fmt.Sprintf("%T", value), func(t *testing.T) {
			var output bytes.Buffer
			logger := logging.NewConsole(&output)
			meter, reader := metricReader(t)
			metrics, err := RequestMetrics(meter)
			require.NoError(t, err)
			router := chi.NewRouter()
			router.Use(metrics, RequestLogging(logger), Recover(logger))
			router.Post("/api/PaymentIntents/{paymentIntentId}/confirm", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Location", "/private-location")
				panic(value)
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents/private-id/confirm?cvv=private-query", strings.NewReader("private-body"))
			require.NotPanics(t, func() { router.ServeHTTP(response, request) })
			require.Equal(t, http.StatusInternalServerError, response.Code)
			assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Empty(t, response.Header().Get("Content-Length"))
			assert.Empty(t, response.Header().Get("Content-Encoding"))
			assert.Empty(t, response.Header().Get("Location"))
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, http.StatusInternalServerError, problem.Status)
			assert.Equal(t, "Internal Server Error", problem.Title)
			assert.NotEmpty(t, problem.RequestID)
			assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
			assert.Empty(t, problem.Detail)
			decoder := json.NewDecoder(strings.NewReader(output.String()))
			var panicLog, requestLog map[string]interface{}
			require.NoError(t, decoder.Decode(&panicLog))
			require.NoError(t, decoder.Decode(&requestLog))
			assert.Equal(t, "ERROR", panicLog["level"])
			assert.Equal(t, fmt.Sprintf("%T", value), panicLog["panic_type"])
			assert.Equal(t, false, panicLog["response_started"])
			assert.Equal(t, problem.RequestID, panicLog["request_id"])
			assert.Equal(t, "/api/PaymentIntents/{paymentIntentId}/confirm", panicLog["route"])
			assert.Contains(t, panicLog["stack"], "recover_middleware_test.go")
			assert.EqualValues(t, 500, requestLog["status_code"])
			assert.ErrorIs(t, decoder.Decode(&requestLog), io.EOF)
			for _, secret := range []string{"private-panic-value", "private-id", "private-query", "private-body", "private-location"} {
				assert.NotContains(t, output.String(), secret)
				assert.NotContains(t, response.Body.String(), secret)
			}
			data := collectHTTPMetrics(t, reader)
			assert.EqualValues(t, 1, sumCounter(t, data["http.server.request.count"]))
			assert.Zero(t, sumCounter(t, data["http.server.request.success.count"]))
		})
	}
}

func TestRecoverAbortsAfterResponseStarts(t *testing.T) {
	for _, stage := range []string{"headers", "body", "flush"} {
		t.Run(stage, func(t *testing.T) {
			var output bytes.Buffer
			handler := Recover(logging.NewConsole(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch stage {
				case "headers":
					w.WriteHeader(http.StatusAccepted)
				case "body":
					_, err := io.WriteString(w, "partial response")
					require.NoError(t, err)
				case "flush":
					require.NoError(t, http.NewResponseController(w).Flush())
				}
				panic("private-panic-value")
			}))
			response := httptest.NewRecorder()
			require.PanicsWithValue(t, http.ErrAbortHandler, func() {
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			})
			assert.NotContains(t, response.Body.String(), "Internal Server Error")
			assert.NotContains(t, response.Body.String(), "private-panic-value")
			if stage == "body" {
				assert.Equal(t, "partial response", response.Body.String())
			} else {
				assert.Empty(t, response.Body.String())
			}
			var record map[string]interface{}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			assert.Equal(t, true, record["response_started"])
			assert.NotContains(t, record, "request_id")
		})
	}
}

func TestRecoverPreservesIntentionalAbort(t *testing.T) {
	var output bytes.Buffer
	handler := Recover(logging.NewConsole(&output))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	response := httptest.NewRecorder()
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	})
	assert.Empty(t, output.String())
	assert.Empty(t, response.Body.String())
	assert.Empty(t, response.Header())
}

type hijackingRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *hijackingRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestRecoverClosesHijackedConnectionOnUnexpectedPanic(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	var output bytes.Buffer
	handler := Recover(logging.NewConsole(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, err := http.NewResponseController(w).Hijack()
		require.NoError(t, err)
		panic("unexpected failure")
	}))
	response := &hijackingRecorder{ResponseRecorder: httptest.NewRecorder(), conn: server}
	require.NoError(t, client.SetReadDeadline(time.Now().Add(time.Second)))
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	})
	_, err := client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
	assert.Empty(t, response.Body.String())
}

func TestRecoverDoesNotChangeSuccessfulResponse(t *testing.T) {
	var output bytes.Buffer
	handler := Recover(logging.NewConsole(&output))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "request", GetRequestID(r.Context()))
		w.Header().Set("X-Test", "retained")
		w.WriteHeader(http.StatusCreated)
		_, err := io.WriteString(w, "created")
		require.NoError(t, err)
	}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(WithRequestID(request.Context(), "request"))
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusCreated, response.Code)
	assert.Equal(t, "retained", response.Header().Get("X-Test"))
	assert.Equal(t, "created", response.Body.String())
	assert.Empty(t, output.String())
}
