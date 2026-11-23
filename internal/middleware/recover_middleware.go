package middleware

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
)

// Recover returns a generic problem response only if no response has been committed.
// Panics in other goroutines are outside this middleware's recovery boundary.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			response := &recoveryResponseWriter{ResponseWriter: w}
			defer func() {
				value := recover()
				if value == nil {
					return
				}
				if value == http.ErrAbortHandler {
					panic(value)
				}
				logger.ErrorContext(r.Context(), "HTTP handler panic recovered",
					"panic_type", fmt.Sprintf("%T", value),
					"method", r.Method, "route", requestRoute(r, requestFamily(r.URL.Path)),
					"response_started", response.started, "stack", panicStack())
				if response.started {
					// A second response would corrupt the body or misreport the committed status.
					if response.hijacked != nil {
						if err := response.hijacked.Close(); err != nil {
							logger.ErrorContext(r.Context(), "failed to close hijacked connection", "error", err)
						}
					}
					panic(http.ErrAbortHandler)
				}
				for _, header := range []string{"Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Location"} {
					w.Header().Del(header)
				}
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Pragma", "no-cache")
				if err := httpresponse.WriteProblem(w, http.StatusInternalServerError, GetRequestID(r.Context()), nil); err != nil {
					logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
				}
			}()
			next.ServeHTTP(response, r)
		})
	}
}

// Record only function names and source locations, never panic values or stack arguments.
func panicStack() string {
	var pcs [32]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var stack strings.Builder
	for {
		frame, more := frames.Next()
		fmt.Fprintf(&stack, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		if !more {
			break
		}
	}
	return stack.String()
}

type recoveryResponseWriter struct {
	http.ResponseWriter
	started  bool
	hijacked net.Conn
}

func (w *recoveryResponseWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.started = true
	}
}

func (w *recoveryResponseWriter) Write(body []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(body)
}

func (w *recoveryResponseWriter) FlushError() error {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *recoveryResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *recoveryResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.started = true
		w.hijacked = conn
	}
	return conn, buffer, err
}

func (w *recoveryResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
