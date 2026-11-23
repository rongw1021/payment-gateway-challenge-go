package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/config"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type observedChanges struct {
	*changedatacapture.InMemory[models.BankTransactionChangeEvent]
	consumed chan models.BankTransactionChangeEvent
}

func (c *observedChanges) Consume(ctx context.Context) (models.BankTransactionChangeEvent, error) {
	event, err := c.InMemory.Consume(ctx)
	if err == nil {
		c.consumed <- event
	}
	return event, err
}

type failingChanges struct{ err error }

type failingIntentChanges struct{ err error }

func (f failingIntentChanges) Produce(models.PaymentIntentChangeEvent) error {
	return errors.New("unexpected publication")
}
func (f failingIntentChanges) Consume(context.Context) (models.PaymentIntentChangeEvent, error) {
	return models.PaymentIntentChangeEvent{}, f.err
}

type observedIntentChanges struct {
	*changedatacapture.InMemory[models.PaymentIntentChangeEvent]
	consumed chan models.PaymentIntentChangeEvent
}

func (c *observedIntentChanges) Consume(ctx context.Context) (models.PaymentIntentChangeEvent, error) {
	event, err := c.InMemory.Consume(ctx)
	if err == nil {
		c.consumed <- event
	}
	return event, err
}

func (f failingChanges) Produce(models.BankTransactionChangeEvent) error {
	return errors.New("unexpected publication")
}
func (f failingChanges) Consume(context.Context) (models.BankTransactionChangeEvent, error) {
	return models.BankTransactionChangeEvent{}, f.err
}

type testBank func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error)

func (b testBank) ProcessBankTransaction(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
	return b(ctx, request)
}

func testCronOptions() backgroundservices.CronJobOptions {
	return backgroundservices.CronJobOptions{
		SweepInterval: time.Hour, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 100,
	}
}

func newDeadlineTestAPI(t *testing.T, bank testBank) (*Api, *atomic.Int32) {
	t.Helper()
	var bankCalls atomic.Int32
	if bank == nil {
		bank = func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
			code := "bank-code"
			return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: &code}, nil
		}
	}
	hash := sha256.Sum256([]byte("deadlines"))
	gateway, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{middleware.CardTokensCreate, middleware.PaymentIntentsCreate, middleware.PaymentIntentsConfirm},
	}}, testBank(func(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		bankCalls.Add(1)
		return bank(ctx, request)
	}), changedatacapture.NewStreams(), testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	return gateway, &bankCalls
}

func serveDeadlineTestAPI(t *testing.T, gateway *Api, listener net.Listener, options config.ServerConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Serve(ctx, listener, options) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(6 * time.Second):
			t.Error("deadline-test server did not shut down")
		}
	})
}

func TestHTTPReadAndIdleDeadlines(t *testing.T) {
	for _, stage := range []string{"headers", "card body", "intent body", "confirmation body", "idle"} {
		t.Run(stage, func(t *testing.T) {
			gateway, bankCalls := newDeadlineTestAPI(t, nil)
			options := config.ServerConfig{
				ReadHeaderTimeout: 250 * time.Millisecond, ReadTimeout: 1200 * time.Millisecond,
				WriteTimeout: 3 * time.Second, IdleTimeout: 250 * time.Millisecond,
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			serveDeadlineTestAPI(t, gateway, listener, options)
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			reader := bufio.NewReader(conn)

			if stage == "headers" || stage == "idle" {
				request := "GET /ping HTTP/1.1\r\nHost: localhost\r\n"
				if stage == "idle" {
					request += "\r\n"
				}
				_, err = io.WriteString(conn, request)
				require.NoError(t, err)
				if stage == "idle" {
					response, err := http.ReadResponse(reader, nil)
					require.NoError(t, err)
					assert.Equal(t, http.StatusOK, response.StatusCode)
					_, err = io.Copy(io.Discard, response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					assert.False(t, response.Close, "healthy connection must be reusable before its idle deadline")
				}
				started := time.Now()
				_, err = reader.ReadByte()
				require.ErrorIs(t, err, io.EOF, "server must close the connection, not wait for the client's five-second deadline")
				assert.Less(t, time.Since(started), 800*time.Millisecond, "header/idle deadline must apply before the longer read deadline")
				assert.Zero(t, bankCalls.Load())
				return
			}

			path, body, expected := "/api/CardTokens",
				`{"card_number":"0222405343240007","expiry_month":4,"expiry_year":9999,"cvv":"012"}`, http.StatusOK
			var intentID string
			if stage == "intent body" {
				path, body, expected = "/api/PaymentIntents", `{"currency":"GBP","amount":100}`, http.StatusCreated
			} else if stage == "confirmation body" {
				amount := int32(100)
				intent, _, err := gateway.paymentIntents.CreatePaymentIntent(context.Background(), "merchant", "create",
					models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
				require.NoError(t, err)
				token, err := gateway.cardTokens.Issue(context.Background(), "merchant",
					models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 9999, Cvv: "012"})
				require.NoError(t, err)
				intentID = intent.PaymentIntentID
				path = "/api/PaymentIntents/" + intentID + "/confirm"
				body = fmt.Sprintf(`{"card_token":%q}`, token.Token)
			}
			const key = "01234567-89ab-4cde-8123-456789abcdef"
			headers := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer sk_deadlines\r\nContent-Type: application/json\r\nX-Idempotency-Key: %s\r\nContent-Length: %d\r\n\r\n", path, key, len(body))
			_, err = io.WriteString(conn, headers+body[:len(body)-1])
			require.NoError(t, err)
			started := time.Now()
			response, err := http.ReadResponse(reader, nil)
			require.NoError(t, err, "a slow body should receive an HTTP timeout response")
			elapsed := time.Since(started)
			assert.GreaterOrEqual(t, elapsed, options.ReadTimeout/2, "body reading must not be limited to the shorter header deadline")
			assert.Less(t, elapsed, 2500*time.Millisecond)
			assert.Equal(t, http.StatusRequestTimeout, response.StatusCode)
			assert.Equal(t, "application/problem+json", response.Header.Get("Content-Type"))
			assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
			var problem models.ProblemDetails
			require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusRequestTimeout, problem.Status)
			assert.NotEmpty(t, problem.RequestID)
			assert.Equal(t, response.Header.Get("X-Request-Id"), problem.RequestID)
			assert.Zero(t, bankCalls.Load())
			if intentID != "" {
				intent, err := gateway.paymentIntents.GetPaymentIntent(context.Background(), "merchant", intentID)
				require.NoError(t, err)
				assert.Equal(t, models.PaymentIntentCreated, intent.Status)
				assert.Empty(t, intent.BankTransactions)
			}
			require.NoError(t, conn.Close())

			// A timed-out body must not consume the token or reserve the idempotency key.
			request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+path, strings.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk_deadlines")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Idempotency-Key", key)
			client := &http.Client{Timeout: 5 * time.Second}
			t.Cleanup(client.CloseIdleConnections)
			retry, err := client.Do(request)
			require.NoError(t, err)
			defer retry.Body.Close()
			assert.Equal(t, expected, retry.StatusCode)
			if intentID != "" {
				var intent models.PaymentIntentResponse
				require.NoError(t, json.NewDecoder(retry.Body).Decode(&intent))
				assert.Equal(t, models.PaymentIntentAuthorized, intent.Status)
				assert.Len(t, intent.BankTransactions, 1)
				assert.EqualValues(t, 1, bankCalls.Load())
			}
		})
	}
}

type deadlineTestConn struct {
	net.Conn
	writeErrors chan error
}

func (conn *deadlineTestConn) Write(body []byte) (int, error) {
	n, err := conn.Conn.Write(body)
	if err != nil {
		select {
		case conn.writeErrors <- err:
		default:
		}
	}
	return n, err
}

type deadlineTestListener struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (listener *deadlineTestListener) Accept() (net.Conn, error) {
	if listener.conn != nil {
		conn := listener.conn
		listener.conn = nil
		return conn, nil
	}
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *deadlineTestListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (*deadlineTestListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestHTTPWriteDeadlineForStalledReader(t *testing.T) {
	gateway, bankCalls := newDeadlineTestAPI(t, nil)
	server, client := net.Pipe()
	writeErrors := make(chan error, 1)
	listener := &deadlineTestListener{
		conn: &deadlineTestConn{Conn: server, writeErrors: writeErrors}, closed: make(chan struct{}),
	}
	options := config.ServerConfig{
		ReadHeaderTimeout: 100 * time.Millisecond, ReadTimeout: 200 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond, IdleTimeout: time.Second,
	}
	serveDeadlineTestAPI(t, gateway, listener, options)
	t.Cleanup(func() { client.Close(); server.Close() })
	require.NoError(t, client.SetWriteDeadline(time.Now().Add(5*time.Second)))
	_, err := io.WriteString(client, "GET /ping HTTP/1.1\r\nHost: localhost\r\n\r\n")
	require.NoError(t, err)
	// net.Pipe has no write buffer, so a client that never reads reliably blocks the response.
	select {
	case err := <-writeErrors:
		var timeout net.Error
		require.ErrorAs(t, err, &timeout)
		assert.True(t, timeout.Timeout())
	case <-time.After(2 * time.Second):
		t.Fatal("response write was not bounded by the server deadline")
	}
	assert.Zero(t, bankCalls.Load())
}

func TestHTTPReadDeadlineDoesNotCancelBankCall(t *testing.T) {
	options := config.ServerConfig{
		ReadHeaderTimeout: 250 * time.Millisecond, ReadTimeout: 500 * time.Millisecond,
		WriteTimeout: 3 * time.Second, IdleTimeout: time.Second,
	}
	gateway, bankCalls := newDeadlineTestAPI(t, testBank(func(ctx context.Context, _ banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		timer := time.NewTimer(2 * options.ReadTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			code := "bank-code"
			return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: &code}, nil
		case <-ctx.Done():
			return banksimulator.BankAuthorizationResponse{}, ctx.Err()
		}
	}))
	amount := int32(100)
	intent, _, err := gateway.paymentIntents.CreatePaymentIntent(context.Background(), "merchant", "create",
		models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	token, err := gateway.cardTokens.Issue(context.Background(), "merchant",
		models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 9999, Cvv: "012"})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDeadlineTestAPI(t, gateway, listener, options)
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm",
		strings.NewReader(fmt.Sprintf(`{"card_token":%q}`, token.Token)))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer sk_deadlines")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Idempotency-Key", "01234567-89ab-4cde-8123-456789abcdef")
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var confirmed models.PaymentIntentResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&confirmed))
	assert.Equal(t, models.PaymentIntentAuthorized, confirmed.Status)
	require.Len(t, confirmed.BankTransactions, 1)
	assert.EqualValues(t, 1, bankCalls.Load())
}

func TestAPIRequestLoggingIncludesAuthenticationAndBindingFailures(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(logging.NewConsole(&output))
	defer slog.SetDefault(previous)
	hash := sha256.Sum256([]byte("logging-test"))
	bank := testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("rejected requests must not call the bank")
		return banksimulator.BankAuthorizationResponse{}, errors.New("unexpected bank call")
	})
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{middleware.CardTokensCreate, middleware.PaymentIntentsCreate},
	}}, bank, changedatacapture.NewStreams(), testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	for _, test := range []struct {
		name, path, authorization, idempotencyKey string
		status                                    int
	}{
		{"authentication", "/api/CardTokens", "", "", http.StatusUnauthorized},
		{"idempotency", "/api/PaymentIntents", "Bearer sk_logging-test", "", http.StatusBadRequest},
		{"binding", "/api/PaymentIntents", "Bearer sk_logging-test", "01234567-89ab-4cde-8123-456789abcdef", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			output.Reset()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"private":"not-a-valid-request"}`))
			request.Header.Set("Authorization", test.authorization)
			request.Header.Set("X-Idempotency-Key", test.idempotencyKey)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			api.router.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code)
			var record map[string]interface{}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			assert.Equal(t, "INFO", record["level"])
			assert.Equal(t, http.MethodPost, record["method"])
			assert.Equal(t, test.path, record["route"])
			assert.EqualValues(t, test.status, record["status_code"])
			assert.NotEmpty(t, record["request_id"])
			assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
			assert.NotContains(t, output.String(), "not-a-valid-request")
			assert.NotContains(t, output.String(), "sk_logging-test")
		})
	}
}

func TestAPIGlobalRateLimitRejectsBeforeAuthenticationAndRecords429(t *testing.T) {
	var output bytes.Buffer
	previousLog := slog.Default()
	slog.SetDefault(logging.NewConsole(&output))
	defer slog.SetDefault(previousLog)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousMeter := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer func() {
		require.NoError(t, provider.Shutdown(context.Background()))
		otel.SetMeterProvider(previousMeter)
	}()
	hash := sha256.Sum256([]byte("rate-limit"))
	credentials := []middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{},
	}}
	bank := testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("rate-limited requests must not call the bank")
		return banksimulator.BankAuthorizationResponse{}, nil
	})
	invalid, err := New(credentials, bank, changedatacapture.NewStreams(), testCronOptions(), 0, 10, slog.Default())
	require.ErrorContains(t, err, "rate_limit.global_requests_per_second")
	assert.Nil(t, invalid)
	api, err := New(credentials, bank, changedatacapture.NewStreams(), testCronOptions(), 1, 10, slog.Default())
	require.NoError(t, err)
	first := httptest.NewRecorder()
	api.router.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil))
	require.Equal(t, http.StatusUnauthorized, first.Code)
	output.Reset()
	response := httptest.NewRecorder()
	api.router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/PaymentIntents", nil))
	require.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.Empty(t, response.Header().Get("WWW-Authenticate"), "throttling must happen before authentication")
	assert.Equal(t, "1", response.Header().Get("Retry-After"))
	var problem models.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
	assert.NotEmpty(t, problem.RequestID)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	assert.EqualValues(t, 429, record["status_code"])
	assert.Equal(t, problem.RequestID, record["request_id"])
	assert.Equal(t, "INFO", record["level"])
	ping := httptest.NewRecorder()
	api.router.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assert.Equal(t, http.StatusOK, ping.Code)
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	var throttled, successes int64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.count" && metric.Name != "http.server.request.success.count" {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, point := range sum.DataPoints {
				status, _ := point.Attributes.Value("http.response.status_code")
				if metric.Name == "http.server.request.count" && status.AsInt64() == 429 {
					throttled += point.Value
				}
				if metric.Name == "http.server.request.success.count" {
					successes += point.Value
				}
			}
		}
	}
	assert.EqualValues(t, 1, throttled)
	assert.EqualValues(t, 2, successes, "the configured non-5xx HTTP success definition still includes 401 and 429")
}

func TestAPIMerchantRateLimitRunsAfterScopeBeforeValidation(t *testing.T) {
	var output bytes.Buffer
	var credentials []middleware.APIKeyCredential
	scopes := []string{middleware.CardTokensCreate, middleware.PaymentIntentsCreate, middleware.PaymentIntentsConfirm, middleware.PaymentIntentsRead}
	for _, key := range []struct {
		payload, merchant string
		scopes            []string
	}{
		{"a1", "a", scopes}, {"a2", "a", scopes},
		{"read", "a", []string{middleware.PaymentIntentsRead}}, {"b", "b", scopes},
	} {
		hash := sha256.Sum256([]byte(key.payload))
		credentials = append(credentials, middleware.APIKeyCredential{
			MerchantID: key.merchant, KeyHash: hex.EncodeToString(hash[:]), Scopes: key.scopes,
		})
	}
	bank := testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("rate-limited requests must not call the bank")
		return banksimulator.BankAuthorizationResponse{}, errors.New("unexpected bank call")
	})
	invalid, err := New(credentials, bank, changedatacapture.NewStreams(), testCronOptions(), 100, 0, logging.NewConsole(&output))
	require.ErrorContains(t, err, "rate_limit.per_merchant_requests_per_second")
	assert.Nil(t, invalid)
	api, err := New(credentials, bank, changedatacapture.NewStreams(), testCronOptions(), 100, 1, logging.NewConsole(&output))
	require.NoError(t, err)
	path := "/api/PaymentIntents/01234567-89ab-4cde-8123-456789abcdef"
	send := func(method, path, key string, status int) {
		t.Helper()
		output.Reset()
		request := httptest.NewRequest(method, path, strings.NewReader("invalid-json"))
		request.Header.Set("Authorization", "Bearer sk_"+key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		api.router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code)
		if status == http.StatusTooManyRequests {
			assert.Equal(t, "1", response.Header().Get("Retry-After"))
			var record map[string]interface{}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			assert.EqualValues(t, 429, record["status_code"])
			assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
		}
	}
	send(http.MethodPost, "/api/CardTokens", "unknown", 401)
	send(http.MethodPost, "/api/CardTokens", "read", 403)
	send(http.MethodGet, path, "a1", 404)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/CardTokens"},
		{http.MethodPost, "/api/PaymentIntents"},
		{http.MethodPost, path + "/confirm"},
		{http.MethodGet, path},
	} {
		send(route.method, route.path, "a2", 429)
	}
	send(http.MethodPost, "/api/CardTokens", "unknown", 401)
	send(http.MethodPost, "/api/CardTokens", "read", 403)
	send(http.MethodGet, path, "b", 404)
}

func TestAPIBackgroundServicesRunAndStopWithServer(t *testing.T) {
	capture := &observedChanges{InMemory: changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), consumed: make(chan models.BankTransactionChangeEvent, 8)}
	intentCapture := &observedIntentChanges{
		InMemory: changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](),
		consumed: make(chan models.PaymentIntentChangeEvent, 8),
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var bankCalls atomic.Int32
	bank := testBank(func(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		bankCalls.Add(1)
		close(entered)
		select {
		case <-release:
			code := "bank-code"
			return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: &code}, nil
		case <-ctx.Done():
			return banksimulator.BankAuthorizationResponse{}, ctx.Err()
		}
	})
	hash := sha256.Sum256([]byte("workers"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{middleware.PaymentIntentsConfirm},
	}}, bank, changedatacapture.Streams{BankTransactions: capture, PaymentIntents: intentCapture}, backgroundservices.CronJobOptions{
		SweepInterval: 5 * time.Millisecond, BankTransactionStatusUpdateTimeout: 30 * time.Millisecond, BatchSize: 1,
	}, 100, 10, slog.Default())
	require.NoError(t, err)
	amount := int32(100)
	intent, _, err := api.paymentIntents.CreatePaymentIntent(context.Background(), "merchant", "create",
		models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	token, err := api.cardTokens.Issue(context.Background(), "merchant",
		models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 9999, Cvv: "012"})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- api.Serve(ctx, listener, config.DefaultServerConfig())
		close(serverDone)
	}()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		cancel()
		select {
		case err := <-serverDone:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("API and background services did not stop")
		}
	})
	baseURL := "http://" + listener.Addr().String()
	ping, err := client.Get(baseURL + "/ping")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, ping.StatusCode)
	_, err = io.Copy(io.Discard, ping.Body)
	require.NoError(t, err)
	require.NoError(t, ping.Body.Close())
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm",
		strings.NewReader(`{"card_token":"`+token.Token+`"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer sk_workers")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Idempotency-Key", "01234567-89ab-4cde-8123-456789abcdef")
	type confirmationResult struct {
		response *http.Response
		err      error
	}
	confirmation := make(chan confirmationResult, 1)
	go func() {
		response, err := client.Do(request)
		confirmation <- confirmationResult{response, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bank call did not start")
	}
	require.Eventually(t, func() bool {
		current, err := api.paymentIntents.GetPaymentIntent(ctx, "merchant", intent.PaymentIntentID)
		return err == nil && current.Status == models.PaymentIntentPendingVerification
	}, 5*time.Second, time.Millisecond)
	pending, err := api.paymentIntents.GetPaymentIntent(ctx, "merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	require.Len(t, pending.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionStatusUpdateTimeout, pending.BankTransactions[0].Status)
	assert.False(t, pending.BankTransactions[0].UpdatedAt.IsZero())
	for _, eventType := range []models.BankTransactionEventType{models.BankTransactionCreated, models.BankTransactionUpdated} {
		select {
		case event := <-capture.consumed:
			assert.Equal(t, eventType, event.EventType)
			assert.Equal(t, intent.PaymentIntentID, event.PaymentIntentID)
		case <-time.After(5 * time.Second):
			t.Fatal("reconciliation did not consume the transaction event")
		}
	}
	for _, eventType := range []models.PaymentIntentEventType{
		models.PaymentIntentCreatedEvent, models.PaymentIntentUpdatedEvent, models.PaymentIntentUpdatedEvent,
	} {
		select {
		case event := <-intentCapture.consumed:
			assert.Equal(t, eventType, event.EventType)
			assert.Equal(t, intent.PaymentIntentID, event.PaymentIntentID)
		case <-time.After(5 * time.Second):
			t.Fatal("audit and webhook worker did not consume the intent event")
		}
	}
	close(release)
	var result confirmationResult
	select {
	case result = <-confirmation:
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation did not finish after releasing the bank")
	}
	require.NoError(t, result.err)
	defer result.response.Body.Close()
	require.Equal(t, http.StatusOK, result.response.StatusCode)
	var completed models.PaymentIntentResponse
	require.NoError(t, json.NewDecoder(result.response.Body).Decode(&completed))
	_, err = io.Copy(io.Discard, result.response.Body)
	require.NoError(t, err)
	require.NoError(t, result.response.Body.Close())
	assert.Equal(t, models.PaymentIntentAuthorized, completed.Status)
	assert.Equal(t, pending.BankTransactions[0].BankTransactionID, completed.BankTransactions[0].BankTransactionID)
	assert.EqualValues(t, 1, bankCalls.Load())
	select {
	case event := <-intentCapture.consumed:
		assert.Equal(t, models.PaymentIntentUpdatedEvent, event.EventType)
		assert.Equal(t, intent.PaymentIntentID, event.PaymentIntentID)
	case <-time.After(5 * time.Second):
		t.Fatal("audit and webhook worker did not consume the late completion event")
	}
	// Close spare pooled connections before measuring server/worker shutdown.
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-time.After(6 * time.Second):
		t.Fatal("API did not stop after cancellation")
	}
}

func TestAPIStopsWhenBackgroundConsumerFails(t *testing.T) {
	failure := errors.New("event consumer unavailable")
	hash := sha256.Sum256([]byte("workers"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("unexpected bank call")
		return banksimulator.BankAuthorizationResponse{}, nil
	}), changedatacapture.Streams{
		BankTransactions: failingChanges{failure},
		PaymentIntents:   changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](),
	}, testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.ErrorIs(t, api.Serve(ctx, listener, config.DefaultServerConfig()), failure)
}

func TestAPIStopsWhenIntentEventConsumerFails(t *testing.T) {
	failure := errors.New("intent event consumer unavailable")
	hash := sha256.Sum256([]byte("workers"))
	streams := changedatacapture.NewStreams()
	streams.PaymentIntents = failingIntentChanges{failure}
	gateway, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("unexpected bank call")
		return banksimulator.BankAuthorizationResponse{}, nil
	}), streams, testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.ErrorIs(t, gateway.Serve(ctx, listener, config.DefaultServerConfig()), failure)
}

func TestAPIRecoversBankPanicWithoutResubmittingPayment(t *testing.T) {
	var output bytes.Buffer
	logger := logging.NewConsole(&output)
	hash := sha256.Sum256([]byte("panic-test"))
	calls := 0
	gateway, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{middleware.PaymentIntentsConfirm},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		calls++
		panic("private bank panic")
	}), changedatacapture.NewStreams(), testCronOptions(), 100, 10, logger)
	require.NoError(t, err)
	amount := int32(100)
	intent, _, err := gateway.paymentIntents.CreatePaymentIntent(context.Background(), "merchant", "create",
		models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	token, err := gateway.cardTokens.Issue(context.Background(), "merchant",
		models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 9999, Cvv: "012"})
	require.NoError(t, err)
	confirm := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm",
			strings.NewReader(fmt.Sprintf(`{"card_token":%q}`, token.Token)))
		request.Header.Set("Authorization", "Bearer sk_panic-test")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Idempotency-Key", "01234567-89ab-4cde-8123-456789abcdef")
		response := httptest.NewRecorder()
		require.NotPanics(t, func() { gateway.router.ServeHTTP(response, request) })
		return response
	}
	response := confirm()
	require.Equal(t, http.StatusInternalServerError, response.Code)
	var problem models.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
	assert.NotContains(t, response.Body.String(), "private bank panic")
	assert.NotContains(t, output.String(), "private bank panic")
	assert.Contains(t, output.String(), `"status_code":500`)
	current, err := gateway.paymentIntents.GetPaymentIntent(context.Background(), "merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentProcessing, current.Status)
	require.Len(t, current.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionScheduled, current.BankTransactions[0].Status)
	assert.Equal(t, http.StatusConflict, confirm().Code, "replay must not resubmit the already reserved attempt")
	assert.Equal(t, 1, calls)
	ping := httptest.NewRecorder()
	gateway.router.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assert.Equal(t, http.StatusOK, ping.Code)
}

func TestAPIRejectsInvalidCronConfiguration(t *testing.T) {
	hash := sha256.Sum256([]byte("workers"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		return banksimulator.BankAuthorizationResponse{}, nil
	}), changedatacapture.NewStreams(), backgroundservices.CronJobOptions{}, 100, 10, slog.Default())
	require.ErrorContains(t, err, "SweepInterval")
	assert.Nil(t, api)
}

func TestCardTokenRouteAndSwagger(t *testing.T) {
	hash := sha256.Sum256([]byte("test-cards"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{middleware.CardTokensCreate},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("card token issuance must not call the bank")
		return banksimulator.BankAuthorizationResponse{}, nil
	}), changedatacapture.NewStreams(), testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/CardTokens",
		strings.NewReader(`{"card_number":"0222405343240007","expiry_month":4,"expiry_year":9999,"cvv":"012"}`))
	req.Header.Set("Authorization", "Bearer sk_test-cards")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	w = httptest.NewRecorder()
	api.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w = httptest.NewRecorder()
	api.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"message":"pong"}`, w.Body.String())

	w = httptest.NewRecorder()
	api.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/payments/missing", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Header().Get("WWW-Authenticate"))

	w = httptest.NewRecorder()
	api.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var document struct {
		Host  string `json:"host"`
		Paths map[string]struct {
			Post struct {
				Description string                     `json:"description"`
				Responses   map[string]json.RawMessage `json:"responses"`
				Security    []map[string][]string      `json:"security"`
			} `json:"post"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &document))
	assert.Empty(t, document.Host, "Swagger should use the current server's host and port")
	require.Contains(t, document.Paths, "/api/CardTokens")
	operation := document.Paths["/api/CardTokens"].Post
	assert.Contains(t, operation.Description, "15 minutes")
	for _, code := range []string{"200", "400", "401", "403", "429"} {
		assert.Contains(t, operation.Responses, code)
	}
	require.Len(t, operation.Security, 1)
	assert.Contains(t, operation.Security[0], "ApiKey")
	assert.Contains(t, document.Paths, "/ping")
	assert.Contains(t, document.Paths, "/api/PaymentIntents")
	assert.Contains(t, document.Paths, "/api/PaymentIntents/{paymentIntentId}")
	confirmation := document.Paths["/api/PaymentIntents/{paymentIntentId}/confirm"].Post
	for _, code := range []string{"200", "202", "400", "401", "403", "404", "409", "429"} {
		assert.Contains(t, confirmation.Responses, code)
	}
	require.Len(t, confirmation.Security, 1)
	assert.Contains(t, confirmation.Security[0], "ApiKey")

	w = httptest.NewRecorder()
	api.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, strings.ReplaceAll(w.Body.String(), `\/`, "/"), `"/swagger/doc.json"`)
	assert.NotContains(t, w.Body.String(), "localhost:8090")
}

func TestCardTokenAPIFailsWithoutCredentials(t *testing.T) {
	api, err := New(nil, nil, changedatacapture.Streams{}, testCronOptions(), 100, 10, slog.Default())
	require.Error(t, err)
	assert.Nil(t, api)
}

func TestPaymentIntentAPIWorkflowUsesBankClientAndEventPublisher(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousTraces, previousMetrics := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(metrics)
	defer func() {
		require.NoError(t, traces.Shutdown(context.Background()))
		require.NoError(t, metrics.Shutdown(context.Background()))
		otel.SetTracerProvider(previousTraces)
		otel.SetMeterProvider(previousMetrics)
	}()
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/payments", r.URL.Path)
		var request banksimulator.BankTransactionRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "0222405343240007", request.CardNumber)
		assert.Equal(t, "012", request.Cvv)
		assert.Equal(t, int32(100), request.Amount)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorized":true,"authorization_code":"bank-code"}`))
	}))
	defer bank.Close()
	client, err := banksimulator.NewClient(bank.URL+"/", time.Second)
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	capture := changedatacapture.NewInMemory[models.BankTransactionChangeEvent]()
	hash := sha256.Sum256([]byte("workflow"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{
			middleware.CardTokensCreate, middleware.PaymentIntentsCreate,
			middleware.PaymentIntentsConfirm, middleware.PaymentIntentsRead,
		},
	}}, client, changedatacapture.Streams{
		BankTransactions: capture, PaymentIntents: changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](),
	}, testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer sk_workflow")
		request.Header.Set("Content-Type", "application/json")
		if method == http.MethodPost && strings.HasPrefix(path, "/api/PaymentIntents") {
			request.Header.Set("X-Idempotency-Key", "01234567-89ab-4cde-8123-456789abcdef")
		}
		response := httptest.NewRecorder()
		api.router.ServeHTTP(response, request)
		return response
	}
	created := send(http.MethodPost, "/api/PaymentIntents", `{"currency":"GBP","amount":100}`)
	require.Equal(t, http.StatusCreated, created.Code)
	var intent models.PaymentIntentResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &intent))
	card := send(http.MethodPost, "/api/CardTokens", `{"card_number":"0222405343240007","expiry_month":4,"expiry_year":9999,"cvv":"012"}`)
	require.Equal(t, http.StatusOK, card.Code)
	var token models.CardTokenResponse
	require.NoError(t, json.Unmarshal(card.Body.Bytes(), &token))
	confirmed := send(http.MethodPost, created.Header().Get("Location")+"/confirm", `{"card_token":"`+token.Token+`"}`)
	require.Equal(t, http.StatusOK, confirmed.Code)
	require.NoError(t, json.Unmarshal(confirmed.Body.Bytes(), &intent))
	assert.Equal(t, models.PaymentIntentAuthorized, intent.Status)
	require.Len(t, intent.BankTransactions, 1)
	assert.Equal(t, "0007", intent.BankTransactions[0].CardNumberLastFour)
	require.NotNil(t, intent.RequestID)
	assert.Equal(t, confirmed.Header().Get("X-Request-Id"), *intent.RequestID)
	for _, id := range []string{
		token.Token, token.RequestID, created.Header().Get("X-Request-Id"),
		intent.PaymentIntentID, intent.BankTransactions[0].BankTransactionID, *intent.RequestID,
	} {
		assert.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", id)
	}
	lookup := send(http.MethodGet, created.Header().Get("Location"), "")
	require.Equal(t, http.StatusOK, lookup.Code)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, kind := range []models.BankTransactionEventType{models.BankTransactionCreated, models.BankTransactionUpdated} {
		event, err := capture.Consume(ctx)
		require.NoError(t, err)
		assert.Equal(t, kind, event.EventType)
		assert.Equal(t, *intent.RequestID, event.RequestID)
		assert.Equal(t, intent.BankTransactions[0].BankTransactionID, event.BankTransactionID)
	}
	assert.Equal(t, http.StatusNotFound, send(http.MethodPost, "/api/Payments", `{}`).Code)
	assert.Equal(t, http.StatusNotFound, send(http.MethodGet, "/api/payments/"+intent.PaymentIntentID, "").Code)
	recorded := spans.Ended()
	require.Len(t, recorded, 1)
	assert.Equal(t, "POST /api/CardTokens", recorded[0].Name(), "payment handlers and bank calls must not create traces")
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	var count int64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "http.server.request.count" {
				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				for _, point := range sum.DataPoints {
					count += point.Value
				}
			}
		}
	}
	assert.EqualValues(t, 4, count)
}

func TestPaymentIntentRoutesValidateKeysAfterAuthBeforeBinding(t *testing.T) {
	var credentials []middleware.APIKeyCredential
	for payload, scopes := range map[string][]string{
		"writer": {middleware.PaymentIntentsCreate, middleware.PaymentIntentsConfirm},
		"reader": {middleware.PaymentIntentsRead},
	} {
		hash := sha256.Sum256([]byte(payload))
		credentials = append(credentials, middleware.APIKeyCredential{
			MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: scopes,
		})
	}
	api, err := New(credentials, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		t.Error("rejected requests must not call the bank")
		return banksimulator.BankAuthorizationResponse{}, nil
	}), changedatacapture.NewStreams(), testCronOptions(), 100, 10, slog.Default())
	require.NoError(t, err)
	for _, path := range []string{
		"/api/PaymentIntents",
		"/api/PaymentIntents/11111111-1111-4111-8111-111111111111/confirm",
	} {
		for _, test := range []struct {
			authorization string
			key           string
			status        int
		}{
			{"", "", http.StatusUnauthorized},
			{"Bearer sk_reader", "", http.StatusForbidden},
			{"Bearer sk_writer", "", http.StatusBadRequest},
			{"Bearer sk_writer", "order", http.StatusBadRequest},
			{"Bearer sk_writer", "01234567-89AB-4CDE-8123-456789ABCDEF", http.StatusBadRequest},
			{"Bearer sk_writer", "0123456789ab4cde8123456789abcdef", http.StatusBadRequest},
		} {
			t.Run(path+"/"+test.authorization+"/"+test.key, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{invalid-json"))
				request.Header.Set("Authorization", test.authorization)
				request.Header.Set("Content-Type", "application/json")
				if test.key != "" {
					request.Header.Set("X-Idempotency-Key", test.key)
				}
				response := httptest.NewRecorder()
				api.router.ServeHTTP(response, request)
				require.Equal(t, test.status, response.Code)
				if test.status == http.StatusBadRequest {
					var problem models.ProblemDetails
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
					assert.Contains(t, problem.Errors, "idempotency_key")
					assert.NotContains(t, problem.Errors, "body")
				}
			})
		}
	}
}
