package banksimulator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var bankRequest = BankTransactionRequest{
	CardNumber: "0002405343248877", ExpiryDate: "04/2035", Currency: "GBP", Amount: 100, Cvv: "012",
}

func TestClientSendsBankContractAndAcceptsExplicitDecisions(t *testing.T) {
	for _, test := range []struct {
		name, body string
		authorized bool
		code       *string
	}{
		{"authorized", `{"authorized":true,"authorization_code":"bank-auth-code"}`, true, stringPointer("bank-auth-code")},
		{"declined empty code", `{"authorized":false,"authorization_code":""}`, false, stringPointer("")},
		{"declined missing code", `{"authorized":false}`, false, nil},
		{"declined null code", `{"authorized":false,"authorization_code":null}`, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/bank/payments", r.URL.Path)
				assert.Equal(t, "application/json", r.Header.Get("Accept"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.JSONEq(t, `{"card_number":"0002405343248877","expiry_date":"04/2035","currency":"GBP","amount":100,"cvv":"012"}`, string(body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/bank/", 5*time.Second)
			require.NoError(t, err)

			decision, err := client.ProcessBankTransaction(context.Background(), bankRequest)

			require.NoError(t, err)
			assert.Equal(t, test.authorized, decision.Authorized)
			assert.Equal(t, test.code, decision.AuthorizationCode)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestClientRejectsUnexpectedStatusesWithoutRetryingOrExposingBody(t *testing.T) {
	for _, status := range []int{201, 204, 307, 400, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirect-target" {
					_, _ = io.WriteString(w, `{"authorized":false}`)
					return
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "sensitive-bank-response")
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/", time.Second)
			require.NoError(t, err)

			_, err = client.ProcessBankTransaction(context.Background(), bankRequest)

			var failure *Failure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, HTTPFailure, failure.Kind)
			assert.Equal(t, status, failure.StatusCode)
			assert.NotContains(t, err.Error(), "sensitive-bank-response")
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestClientRejectsInvalidDecisions(t *testing.T) {
	for _, body := range []string{
		"not-json", "", `"plain string"`, "null", "[]", "{}",
		`{"errMsg":"sensitive-bank-response"}`, `{"authorized":null}`, `{"authorized":"false"}`,
		`{"authorized":1}`, `{"authorized":true}`, `{"authorized":true,"authorization_code":null}`,
		`{"authorized":true,"authorization_code":" "}`, `{"authorized":false,"authorization_code":123}`,
		`{"authorized":false} {}`, `{"authorized":false} trailing`,
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/", time.Second)
			require.NoError(t, err)

			_, err = client.ProcessBankTransaction(context.Background(), bankRequest)

			var failure *Failure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, InvalidResponseFailure, failure.Kind)
			assert.NotContains(t, err.Error(), "sensitive-bank-response")
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestClientResponseLimitIncludesTrailingWhitespace(t *testing.T) {
	for _, knownLength := range []bool{false, true} {
		for _, size := range []int{64 * 1024, 64*1024 + 1} {
			t.Run(fmt.Sprintf("length=%t/bytes=%d", knownLength, size), func(t *testing.T) {
				body := `{"authorized":false}`
				body += strings.Repeat(" ", size-len(body))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if knownLength {
						w.Header().Set("Content-Length", fmt.Sprint(size))
					} else {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					_, _ = io.WriteString(w, body)
				}))
				defer server.Close()
				client, err := NewClient(server.URL+"/", time.Second)
				require.NoError(t, err)

				decision, err := client.ProcessBankTransaction(context.Background(), bankRequest)

				if size == 64*1024 {
					require.NoError(t, err)
					assert.False(t, decision.Authorized)
				} else {
					var failure *Failure
					require.ErrorAs(t, err, &failure)
					assert.Equal(t, InvalidResponseFailure, failure.Kind)
				}
			})
		}
	}
}

func TestClientTimeoutCoversHeadersAndBody(t *testing.T) {
	for _, delayBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%t", delayBody), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read bank request: %v", err)
					return
				}
				if delayBody {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/", 100*time.Millisecond)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()

			_, err = client.ProcessBankTransaction(ctx, bankRequest)

			elapsed := time.Since(start)
			var failure *Failure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, TimeoutFailure, failure.Kind)
			assert.NoError(t, ctx.Err())
			assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
			assert.Less(t, elapsed, 3*time.Second)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestClientCallerCancellationIsNotBankTimeout(t *testing.T) {
	for _, delayBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%t", delayBody), func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read bank request: %v", err)
					return
				}
				if delayBody {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/", 5*time.Second)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.ProcessBankTransaction(ctx, bankRequest)
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-result:
				assert.ErrorIs(t, err, context.Canceled)
				assert.NotErrorIs(t, err, context.DeadlineExceeded)
				var failure *Failure
				assert.False(t, errors.As(err, &failure))
			case <-time.After(5 * time.Second):
				t.Fatal("request did not stop after cancellation")
			}
		})
	}
}

type bankRoundTripper func(*http.Request) (*http.Response, error)

func (send bankRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return send(request)
}

type failedResponseBody struct {
	err    error
	closed bool
}

func (b *failedResponseBody) Read([]byte) (int, error) { return 0, b.err }
func (b *failedResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestClientPropagatesTransportAndBodyFailures(t *testing.T) {
	failure := errors.New("simulated network failure")
	for _, duringBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%t", duringBody), func(t *testing.T) {
			client, err := NewClient("https://bank.example/", time.Second)
			require.NoError(t, err)
			calls := 0
			body := &failedResponseBody{err: failure}
			client.httpClient.Transport = bankRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				if duringBody {
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				}
				return nil, failure
			})

			_, err = client.ProcessBankTransaction(context.Background(), bankRequest)

			assert.ErrorIs(t, err, failure)
			var bankFailure *Failure
			require.ErrorAs(t, err, &bankFailure)
			assert.Equal(t, NetworkFailure, bankFailure.Kind)
			assert.Equal(t, 1, calls)
			if duringBody {
				assert.True(t, body.closed)
			}
		})
	}
}

func TestClientDoesNotStoreCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "bank-session", Value: "not-forwarded"})
		_, _ = io.WriteString(w, `{"authorized":false}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/", time.Second)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err := client.ProcessBankTransaction(context.Background(), bankRequest)
		require.NoError(t, err)
	}
}

func TestClientConfigurationValidation(t *testing.T) {
	for _, address := range []string{
		"", "relative/", "ftp://bank.example/", "https://bank.example/path", "http:///",
		"https://user:password@bank.example/", "https://bank.example/?x=1", "https://bank.example/?",
		"https://bank.example/#fragment", "https://bank.example/#",
	} {
		t.Run(address, func(t *testing.T) {
			client, err := NewClient(address, time.Second)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.NotContains(t, err.Error(), "password")
		})
	}
	for _, timeout := range []time.Duration{-time.Millisecond, 0, time.Millisecond - 1, (1 << 31) * time.Millisecond} {
		client, err := NewClient("https://bank.example/", timeout)
		require.Error(t, err)
		assert.Nil(t, client)
	}
	for _, timeout := range []time.Duration{time.Millisecond, (1<<31 - 1) * time.Millisecond} {
		_, err := NewClient("https://bank.example/", timeout)
		require.NoError(t, err)
	}
}

func stringPointer(value string) *string { return &value }
