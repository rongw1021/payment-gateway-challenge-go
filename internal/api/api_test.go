package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
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
	*changedatacapture.InMemory
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
	}}, bank, changedatacapture.NewInMemory(), testCronOptions(), 100)
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
	invalid, err := New(credentials, bank, changedatacapture.NewInMemory(), testCronOptions(), 0)
	require.ErrorContains(t, err, "rate_limit.requests_per_second")
	assert.Nil(t, invalid)
	api, err := New(credentials, bank, changedatacapture.NewInMemory(), testCronOptions(), 1)
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

func TestAPIBackgroundServicesRunAndStopWithServer(t *testing.T) {
	capture := &observedChanges{InMemory: changedatacapture.NewInMemory(), consumed: make(chan models.BankTransactionChangeEvent, 8)}
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
	}}, bank, capture, backgroundservices.CronJobOptions{
		SweepInterval: 5 * time.Millisecond, BankTransactionStatusUpdateTimeout: 30 * time.Millisecond, BatchSize: 1,
	}, 100)
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
		serverDone <- api.Serve(ctx, listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serverDone:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("API and background services did not stop")
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + listener.Addr().String()
	ping, err := client.Get(baseURL + "/ping")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, ping.StatusCode)
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
		current, err := api.paymentIntents.GetPaymentIntent("merchant", intent.PaymentIntentID)
		return err == nil && current.Status == models.PaymentIntentPendingVerification
	}, 5*time.Second, time.Millisecond)
	pending, err := api.paymentIntents.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	require.Len(t, pending.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionStatusUpdateTimeout, pending.BankTransactions[0].Status)
	assert.NotNil(t, pending.BankTransactions[0].CompletedAt)
	for _, eventType := range []models.BankTransactionEventType{models.BankTransactionCreated, models.BankTransactionUpdated} {
		select {
		case event := <-capture.consumed:
			assert.Equal(t, eventType, event.EventType)
			assert.Equal(t, intent.PaymentIntentID, event.PaymentIntentID)
		case <-time.After(5 * time.Second):
			t.Fatal("reconciliation did not consume the transaction event")
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
	assert.Equal(t, models.PaymentIntentAuthorized, completed.Status)
	assert.Equal(t, pending.BankTransactions[0].BankTransactionID, completed.BankTransactions[0].BankTransactionID)
	assert.EqualValues(t, 1, bankCalls.Load())
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
	}), failingChanges{failure}, testCronOptions(), 100)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.ErrorIs(t, api.Serve(ctx, listener), failure)
}

func TestAPIRejectsInvalidCronConfiguration(t *testing.T) {
	hash := sha256.Sum256([]byte("workers"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: []string{},
	}}, testBank(func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		return banksimulator.BankAuthorizationResponse{}, nil
	}), changedatacapture.NewInMemory(), backgroundservices.CronJobOptions{}, 100)
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
	}), changedatacapture.NewInMemory(), testCronOptions(), 100)
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
	api, err := New(nil, nil, nil, testCronOptions(), 100)
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
	capture := changedatacapture.NewInMemory()
	hash := sha256.Sum256([]byte("workflow"))
	api, err := New([]middleware.APIKeyCredential{{
		MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]),
		Scopes: []string{
			middleware.CardTokensCreate, middleware.PaymentIntentsCreate,
			middleware.PaymentIntentsConfirm, middleware.PaymentIntentsRead,
		},
	}}, client, capture, testCronOptions(), 100)
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
	}), changedatacapture.NewInMemory(), testCronOptions(), 100)
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
