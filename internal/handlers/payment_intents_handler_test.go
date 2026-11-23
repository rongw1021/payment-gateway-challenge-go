package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	missingIntentID  = "11111111-1111-4111-8111-111111111111"
	intentCreateKey  = "01234567-89ab-4cde-8123-456789abcdef"
	intentConfirmKey = "abcdef01-2345-4789-abcd-ef0123456789"
	intentRetryKey   = "11112222-3333-4444-8555-666677778888"
	intentOtherKey   = "99998888-7777-4666-8555-444433332222"
)

type handlerBank struct {
	calls   atomic.Int32
	process func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error)
}

func (b *handlerBank) ProcessBankTransaction(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
	b.calls.Add(1)
	return b.process(ctx, request)
}

type intentHandlerFixture struct {
	router  *chi.Mux
	repo    *repository.PaymentIntentsRepository
	cards   *services.CardTokenService
	bank    *handlerBank
	clock   atomic.Int64
	changes *changedatacapture.InMemory[models.BankTransactionChangeEvent]
}

func newIntentHandler(t *testing.T) *intentHandlerFixture {
	t.Helper()
	return newIntentHandlerWithLogger(t, slog.Default())
}

func newIntentHandlerWithLogger(t *testing.T, logger *slog.Logger) *intentHandlerFixture {
	t.Helper()
	f := &intentHandlerFixture{router: chi.NewRouter(), changes: changedatacapture.NewInMemory[models.BankTransactionChangeEvent]()}
	f.clock.Store(time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	f.cards = services.NewCardTokenService(repository.NewCardsRepository(now), now, logger)
	f.bank = &handlerBank{process: func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		code := "bank-code"
		return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: &code}, nil
	}}
	var err error
	f.repo, err = repository.NewPaymentIntentsRepository(f.changes, changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](), now)
	require.NoError(t, err)
	service, err := services.NewPaymentIntentsService(f.repo, f.bank, f.cards, now, logger)
	require.NoError(t, err)
	full := []string{
		middleware.CardTokensCreate, middleware.PaymentIntentsCreate,
		middleware.PaymentIntentsConfirm, middleware.PaymentIntentsRead,
	}
	var credentials []middleware.APIKeyCredential
	for key, scopes := range map[string][]string{
		"full": full, "other": full, "read": {middleware.PaymentIntentsRead},
		"create": {middleware.PaymentIntentsCreate}, "confirm": {middleware.PaymentIntentsConfirm},
		"cards": {middleware.CardTokensCreate},
	} {
		hash := sha256.Sum256([]byte(key))
		merchant := "merchant"
		if key == "other" {
			merchant = "other-merchant"
		}
		credentials = append(credentials, middleware.APIKeyCredential{
			MerchantID: merchant, KeyHash: hex.EncodeToString(hash[:]), Scopes: scopes,
		})
	}
	auth, err := middleware.NewAuthenticator(credentials)
	require.NoError(t, err)
	handler := NewPaymentIntentsHandler(service, logger)
	routes := f.router.With(middleware.NoStore, middleware.RequestID(logger))
	routes.With(auth.RequireScope(middleware.PaymentIntentsCreate, logger), middleware.RequireIdempotencyKey(logger)).Post("/api/PaymentIntents", handler.PostHandler())
	routes.With(auth.RequireScope(middleware.PaymentIntentsConfirm, logger), middleware.RequireIdempotencyKey(logger)).Post("/api/PaymentIntents/{paymentIntentId}/confirm", handler.ConfirmHandler())
	routes.With(auth.RequireScope(middleware.PaymentIntentsRead, logger)).Get("/api/PaymentIntents/{paymentIntentId}", handler.GetHandler())
	routes.With(auth.RequireScope(middleware.CardTokensCreate, logger)).Post("/api/CardTokens", NewCardTokensHandler(f.cards, logger).PostHandler())
	return f
}

func (f *intentHandlerFixture) send(method, path, apiKey, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer sk_"+apiKey)
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("X-Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func (f *intentHandlerFixture) create(t *testing.T, apiKey, key string) models.PaymentIntentResponse {
	t.Helper()
	response := f.send(http.MethodPost, "/api/PaymentIntents", apiKey, key, `{"currency":"GBP","amount":100}`)
	require.Equal(t, http.StatusCreated, response.Code)
	var intent models.PaymentIntentResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &intent))
	assert.Equal(t, "/api/PaymentIntents/"+intent.PaymentIntentID, response.Header().Get("Location"))
	require.NotNil(t, intent.RequestID)
	assert.Equal(t, response.Header().Get("X-Request-Id"), *intent.RequestID)
	return intent
}

func (f *intentHandlerFixture) token(t *testing.T, apiKey, number string) string {
	t.Helper()
	body := strings.Replace(cardTokenBody, "0222405343240007", number, 1)
	response := f.send(http.MethodPost, "/api/CardTokens", apiKey, "", body)
	require.Equal(t, http.StatusOK, response.Code)
	var token models.CardTokenResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &token))
	return token.Token
}

func (f *intentHandlerFixture) confirm(id, apiKey, key, token string) *httptest.ResponseRecorder {
	return f.send(http.MethodPost, "/api/PaymentIntents/"+id+"/confirm", apiKey, key, fmt.Sprintf(`{"card_token":%q}`, token))
}

func paymentProblem(t *testing.T, response *httptest.ResponseRecorder, status int) models.ProblemDetails {
	t.Helper()
	require.Equal(t, status, response.Code)
	assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	var problem models.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, status, problem.Status)
	assert.NotEmpty(t, problem.RequestID)
	assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
	return problem
}

func acceptedPaymentIntent(t *testing.T, response *httptest.ResponseRecorder) models.PaymentIntentResponse {
	t.Helper()
	require.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var intent models.PaymentIntentResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &intent))
	assert.Equal(t, models.PaymentIntentPendingVerification, intent.Status)
	assert.Equal(t, "/api/PaymentIntents/"+intent.PaymentIntentID, response.Header().Get("Location"))
	require.NotNil(t, intent.RequestID)
	assert.NotEmpty(t, *intent.RequestID)
	assert.Equal(t, response.Header().Get("X-Request-Id"), *intent.RequestID)
	return intent
}

func TestPaymentIntentsHandlerWorkflowAndReplayContract(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(fmt.Sprint(authorized), func(t *testing.T) {
			f := newIntentHandler(t)
			f.bank.process = func(_ context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
				assert.Equal(t, "0222405343240007", request.CardNumber)
				assert.Equal(t, "012", request.Cvv)
				assert.Equal(t, "04/2035", request.ExpiryDate)
				assert.Equal(t, "GBP", request.Currency)
				assert.Equal(t, int32(100), request.Amount)
				code := "bank-code"
				return banksimulator.BankAuthorizationResponse{Authorized: authorized, AuthorizationCode: &code}, nil
			}
			intent := f.create(t, "create", intentCreateKey)
			assert.Equal(t, models.PaymentIntentCreated, intent.Status)
			assert.Empty(t, intent.BankTransactions)
			assert.Zero(t, f.bank.calls.Load())
			token := f.token(t, "cards", "0222405343240007")
			response := f.send(http.MethodPost, "/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm", "confirm", intentCreateKey,
				fmt.Sprintf(`{"card_token":%q,"currency":"USD","amount":999}`, token))
			require.Equal(t, http.StatusOK, response.Code)
			var confirmed models.PaymentIntentResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &confirmed))
			expected := models.PaymentIntentDeclined
			if authorized {
				expected = models.PaymentIntentAuthorized
			}
			assert.Equal(t, expected, confirmed.Status)
			require.Len(t, confirmed.BankTransactions, 1)
			assert.Equal(t, "0007", confirmed.BankTransactions[0].CardNumberLastFour)
			assert.False(t, confirmed.BankTransactions[0].UpdatedAt.IsZero())
			var shape map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &shape))
			assert.Len(t, shape, 8)
			for _, name := range []string{"payment_intent_id", "status", "currency", "amount", "bank_transactions", "request_id", "created_at", "updated_at"} {
				assert.Contains(t, shape, name)
			}
			for _, secret := range []string{"0222405343240007", "cvv", token, "bank-code", "merchant", "idempotency", "fingerprint"} {
				assert.NotContains(t, response.Body.String(), secret)
			}
			f.clock.Add(int64(16 * time.Minute))
			replay := f.confirm(intent.PaymentIntentID, "confirm", intentCreateKey, token)
			require.Equal(t, http.StatusOK, replay.Code)
			var replayed models.PaymentIntentResponse
			require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &replayed))
			require.NotNil(t, confirmed.RequestID)
			require.NotNil(t, replayed.RequestID)
			assert.NotEqual(t, *confirmed.RequestID, *replayed.RequestID)
			assert.Equal(t, replay.Header().Get("X-Request-Id"), *replayed.RequestID)
			confirmed.RequestID, replayed.RequestID = nil, nil
			assert.Equal(t, confirmed, replayed)
			lookup := f.send(http.MethodGet, "/api/PaymentIntents/"+strings.ToUpper(intent.PaymentIntentID), "read", "", "")
			require.Equal(t, http.StatusOK, lookup.Code)
			assert.EqualValues(t, 1, f.bank.calls.Load())
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, kind := range []models.BankTransactionEventType{models.BankTransactionCreated, models.BankTransactionUpdated} {
				event, err := f.changes.Consume(ctx)
				require.NoError(t, err)
				assert.Equal(t, kind, event.EventType)
				assert.Equal(t, response.Header().Get("X-Request-Id"), event.RequestID)
			}
		})
	}
}

func TestPaymentIntentsHandlerConcurrentCreationAndConflicts(t *testing.T) {
	f := newIntentHandler(t)
	responses := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := 0; i < cap(responses); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- f.send(http.MethodPost, "/api/PaymentIntents", "full", intentCreateKey, `{"currency":"GBP","amount":100}`)
		}()
	}
	wg.Wait()
	close(responses)
	ids := make(map[string]bool)
	for response := range responses {
		require.Equal(t, http.StatusCreated, response.Code)
		var intent models.PaymentIntentResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &intent))
		ids[intent.PaymentIntentID] = true
	}
	assert.Len(t, ids, 1)
	assert.Zero(t, f.bank.calls.Load())
	for _, body := range []string{`{"currency":"USD","amount":100}`, `{"currency":"GBP","amount":101}`} {
		problem := paymentProblem(t, f.send(http.MethodPost, "/api/PaymentIntents", "full", intentCreateKey, body), http.StatusConflict)
		assert.Equal(t, "The idempotency key was already used with different intent details.", problem.Detail)
	}
	replayed := f.create(t, "full", intentCreateKey)
	assert.True(t, ids[replayed.PaymentIntentID])
}

func TestPaymentIntentsHandlerRequiresExactScope(t *testing.T) {
	f := newIntentHandler(t)
	for _, route := range []struct {
		method, path, body, permitted string
		success                       int
	}{
		{http.MethodPost, "/api/PaymentIntents", `{"currency":"GBP","amount":100}`, "create", 201},
		{http.MethodPost, "/api/PaymentIntents/" + missingIntentID + "/confirm", `{"card_token":"ffffffff-ffff-ffff-ffff-ffffffffffff"}`, "confirm", 404},
		{http.MethodGet, "/api/PaymentIntents/" + missingIntentID, "", "read", 404},
	} {
		for _, key := range []string{"", "unknown", "cards", "create", "confirm", "read"} {
			t.Run(route.method+route.path+"/"+key, func(t *testing.T) {
				response := f.send(route.method, route.path, key, intentCreateKey, route.body)
				switch {
				case key == "" || key == "unknown":
					paymentProblem(t, response, http.StatusUnauthorized)
					assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
				case key != route.permitted:
					paymentProblem(t, response, http.StatusForbidden)
				default:
					assert.Equal(t, route.success, response.Code)
				}
			})
		}
	}
	assert.Zero(t, f.bank.calls.Load())
}

func TestPaymentIntentsHandlerValidatesIdempotencyHeadersBeforeConsuming(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		for _, keys := range [][]string{
			nil, {""}, {" "}, {"legacy-key"}, {strings.ToUpper(intentCreateKey)},
			{intentCreateKey + "," + intentConfirmKey}, {intentCreateKey, intentConfirmKey},
		} {
			t.Run(fmt.Sprintf("confirm=%t/keys=%q", confirm, keys), func(t *testing.T) {
				f := newIntentHandler(t)
				intent := f.create(t, "full", intentCreateKey)
				token := f.token(t, "full", "0222405343240007")
				path, body := "/api/PaymentIntents", `{"currency":"GBP","amount":100}`
				if confirm {
					path, body = path+"/"+intent.PaymentIntentID+"/confirm", fmt.Sprintf(`{"card_token":%q}`, token)
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer sk_full")
				request.Header.Set("Content-Type", "application/json")
				for _, key := range keys {
					request.Header.Add("X-Idempotency-Key", key)
				}
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, request)
				problem := paymentProblem(t, response, http.StatusBadRequest)
				assert.Equal(t, []string{"Supply exactly one idempotency key in lowercase hyphenated GUID format."}, problem.Errors["idempotency_key"])
				assert.Zero(t, f.bank.calls.Load())
				assert.Equal(t, http.StatusOK, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token).Code)
			})
		}
	}
}

func TestPaymentIntentsHandlerRejectsInvalidBodiesWithoutReservingKeys(t *testing.T) {
	for _, body := range []string{
		"", "null", "[]", "{", "{}", `{"currency":"JPY","amount":100}`,
		`{"currency":"gbp","amount":100}`,
		`{"amount":100}`, `{"currency":"GBP"}`, `{"currency":"GBP","amount":null}`,
		`{"currency":"GBP","amount":1.5}`, `{"currency":"GBP","amount":"100"}`,
		`{"currency":"GBP","amount":0}`, `{"currency":"GBP","amount":-1}`,
		`{"currency":"GBP","amount":-2147483648}`,
		`{"currency":"GBP","amount":2147483648}`, `{"currency":"GBP","amount":100} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			f := newIntentHandler(t)
			paymentProblem(t, f.send(http.MethodPost, "/api/PaymentIntents", "full", intentCreateKey, body), http.StatusBadRequest)
			f.create(t, "full", intentCreateKey)
			assert.Zero(t, f.bank.calls.Load())
		})
	}
	for _, amount := range []int{1, 2147483647} {
		f := newIntentHandler(t)
		response := f.send(http.MethodPost, "/api/PaymentIntents", "full", intentCreateKey, fmt.Sprintf(`{"currency":"GBP","amount":%d}`, amount))
		assert.Equal(t, http.StatusCreated, response.Code)
	}
	for _, body := range []string{"{}", `{"card_token":null}`, `{"card_token":123}`, `{"card_token":"invalid"}`} {
		f := newIntentHandler(t)
		intent := f.create(t, "full", intentCreateKey)
		token := f.token(t, "full", "0222405343240007")
		response := f.send(http.MethodPost, "/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm", "full", intentConfirmKey, body)
		paymentProblem(t, response, http.StatusBadRequest)
		assert.Zero(t, f.bank.calls.Load())
		assert.Equal(t, http.StatusOK, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token).Code)
	}
}

func TestPaymentIntentsHandlerEnforcesJSONContentTypeAndBodyLimit(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		for _, test := range []struct {
			name, contentType string
			oversized         bool
			status            int
		}{
			{"missing content type", "", false, http.StatusUnsupportedMediaType},
			{"wrong content type", "text/plain", false, http.StatusUnsupportedMediaType},
			{"oversized body", "application/json", true, http.StatusRequestEntityTooLarge},
		} {
			t.Run(fmt.Sprintf("confirm=%t/%s", confirm, test.name), func(t *testing.T) {
				f := newIntentHandler(t)
				path, body := "/api/PaymentIntents", `{"currency":"GBP","amount":100}`
				if confirm {
					intent := f.create(t, "full", intentCreateKey)
					path += "/" + intent.PaymentIntentID + "/confirm"
					body = fmt.Sprintf(`{"card_token":%q}`, f.token(t, "full", "0222405343240007"))
				}
				if test.oversized {
					body += strings.Repeat(" ", 1<<20)
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer sk_full")
				request.Header.Set("X-Idempotency-Key", intentCreateKey)
				request.Header.Set("Content-Type", test.contentType)
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, request)
				paymentProblem(t, response, test.status)
				assert.Zero(t, f.bank.calls.Load())
			})
		}
	}
}

func TestPaymentIntentsHandlerDeclineRetryReturnsCurrentHistoryOnOldReplay(t *testing.T) {
	f := newIntentHandler(t)
	f.bank.process = func(_ context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		code := "bank-code"
		return banksimulator.BankAuthorizationResponse{Authorized: strings.HasSuffix(request.CardNumber, "7"), AuthorizationCode: &code}, nil
	}
	intent := f.create(t, "full", intentCreateKey)
	first := f.token(t, "full", "0222405343240002")
	second := f.token(t, "full", "0222405343240007")
	require.Equal(t, http.StatusOK, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, first).Code)
	paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, second), http.StatusConflict)
	paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentRetryKey, first), http.StatusBadRequest)
	f.clock.Add(int64(time.Minute))
	require.Equal(t, http.StatusOK, f.confirm(intent.PaymentIntentID, "full", intentRetryKey, second).Code)
	replay := f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, first)
	require.Equal(t, http.StatusOK, replay.Code)
	var current models.PaymentIntentResponse
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &current))
	assert.Equal(t, models.PaymentIntentAuthorized, current.Status)
	require.Len(t, current.BankTransactions, 2)
	assert.Equal(t, models.BankTransactionDeclined, current.BankTransactions[0].Status)
	assert.Equal(t, models.BankTransactionAuthorized, current.BankTransactions[1].Status)
	assert.EqualValues(t, 2, f.bank.calls.Load())
	paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentOtherKey, f.token(t, "full", "0222405343240007")), http.StatusConflict)
}

func TestPaymentIntentsHandlerMerchantIsolationAndInvalidTokens(t *testing.T) {
	f := newIntentHandler(t)
	owner := f.create(t, "full", intentCreateKey)
	other := f.create(t, "other", intentCreateKey)
	token := f.token(t, "full", "0222405343240007")
	foreignToken := f.token(t, "other", "0222405343240007")
	missing := paymentProblem(t, f.send(http.MethodGet, "/api/PaymentIntents/"+missingIntentID, "full", "", ""), http.StatusNotFound)
	for _, id := range []string{other.PaymentIntentID, "not-a-guid"} {
		problem := paymentProblem(t, f.send(http.MethodGet, "/api/PaymentIntents/"+id, "full", "", ""), http.StatusNotFound)
		assert.Equal(t, missing.Detail, problem.Detail)
		paymentProblem(t, f.confirm(id, "full", intentConfirmKey, token), http.StatusNotFound)
	}
	problem := paymentProblem(t, f.confirm(owner.PaymentIntentID, "full", intentConfirmKey, foreignToken), http.StatusBadRequest)
	assert.Equal(t, "The card token is invalid or unavailable.", problem.Detail)
	paymentProblem(t, f.confirm(owner.PaymentIntentID, "full", intentConfirmKey, "ffffffff-ffff-ffff-ffff-ffffffffffff"), http.StatusBadRequest)
	assert.Equal(t, http.StatusOK, f.confirm(owner.PaymentIntentID, "full", intentConfirmKey, token).Code)
	assert.Equal(t, http.StatusOK, f.confirm(other.PaymentIntentID, "other", intentConfirmKey, foreignToken).Code)
	assert.EqualValues(t, 2, f.bank.calls.Load())
}

func TestPaymentIntentsHandlerPendingOutcomesReturnAcceptedIntent(t *testing.T) {
	for _, test := range []struct {
		err    error
		status models.BankTransactionStatus
	}{
		{&banksimulator.Failure{Kind: banksimulator.HTTPFailure, StatusCode: 503}, models.BankTransactionServiceUnavailable},
		{&banksimulator.Failure{Kind: banksimulator.HTTPFailure, StatusCode: 500}, models.BankTransactionInvalidResponse},
		{&banksimulator.Failure{Kind: banksimulator.NetworkFailure}, models.BankTransactionNetworkError},
		{context.DeadlineExceeded, models.BankTransactionRequestTimeout},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			f := newIntentHandler(t)
			f.bank.process = func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
				return banksimulator.BankAuthorizationResponse{}, test.err
			}
			intent := f.create(t, "full", intentCreateKey)
			token := f.token(t, "full", "0222405343240007")
			pending := acceptedPaymentIntent(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token))
			assert.Equal(t, intent.PaymentIntentID, pending.PaymentIntentID)
			assert.Equal(t, intent.Amount, pending.Amount)
			assert.Equal(t, intent.Currency, pending.Currency)
			require.Len(t, pending.BankTransactions, 1)
			assert.NotEmpty(t, pending.BankTransactions[0].BankTransactionID)
			assert.Equal(t, test.status, pending.BankTransactions[0].Status)
			assert.Equal(t, "0007", pending.BankTransactions[0].CardNumberLastFour)
			assert.False(t, pending.BankTransactions[0].UpdatedAt.IsZero())
			replay := acceptedPaymentIntent(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token))
			assert.Equal(t, pending.PaymentIntentID, replay.PaymentIntentID)
			assert.Equal(t, pending.BankTransactions, replay.BankTransactions)
			assert.NotEqual(t, *pending.RequestID, *replay.RequestID)
			retrieved := f.send(http.MethodGet, "/api/PaymentIntents/"+intent.PaymentIntentID, "full", "", "")
			require.Equal(t, http.StatusOK, retrieved.Code)
			var current models.PaymentIntentResponse
			require.NoError(t, json.Unmarshal(retrieved.Body.Bytes(), &current))
			assert.Equal(t, pending.Status, current.Status)
			assert.Equal(t, pending.BankTransactions, current.BankTransactions)
			paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentOtherKey, f.token(t, "full", "0222405343240007")), http.StatusConflict)
			assert.EqualValues(t, 1, f.bank.calls.Load())
		})
	}
}

func TestPaymentIntentsHandlerCanceledConfirmationReplaysAsAccepted(t *testing.T) {
	f := newIntentHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.bank.process = func(ctx context.Context, _ banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		cancel()
		return banksimulator.BankAuthorizationResponse{}, ctx.Err()
	}
	intent := f.create(t, "full", intentCreateKey)
	token := f.token(t, "full", "0222405343240007")
	request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm",
		strings.NewReader(fmt.Sprintf(`{"card_token":%q}`, token))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer sk_full")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Idempotency-Key", intentConfirmKey)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	paymentProblem(t, response, http.StatusInternalServerError)

	replay := acceptedPaymentIntent(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token))
	assert.Equal(t, intent.PaymentIntentID, replay.PaymentIntentID)
	require.Len(t, replay.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionRequestCanceled, replay.BankTransactions[0].Status)
	assert.EqualValues(t, 1, f.bank.calls.Load())
}

func TestPaymentIntentsHandlerInFlightAndTimedOutReplays(t *testing.T) {
	f := newIntentHandler(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.bank.process = func(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		if strings.HasSuffix(request.CardNumber, "2") {
			return banksimulator.BankAuthorizationResponse{}, nil
		}
		close(entered)
		select {
		case <-release:
			return banksimulator.BankAuthorizationResponse{}, nil
		case <-ctx.Done():
			return banksimulator.BankAuthorizationResponse{}, ctx.Err()
		}
	}
	intent := f.create(t, "full", intentCreateKey)
	declinedToken := f.token(t, "full", "0222405343240002")
	require.Equal(t, http.StatusOK, f.confirm(intent.PaymentIntentID, "full", intentCreateKey, declinedToken).Code)
	// A clock adjustment must not make the older decline look like the active attempt.
	f.clock.Add(-int64(time.Second))
	token := f.token(t, "full", "0222405343240007")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents/"+intent.PaymentIntentID+"/confirm", strings.NewReader(fmt.Sprintf(`{"card_token":%q}`, token))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer sk_full")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Idempotency-Key", intentConfirmKey)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		done <- response
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("bank call did not start")
	}
	processing := paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token), http.StatusConflict)
	assert.Equal(t, models.BankTransactionScheduled, processing.BankTransactionStatus)
	assert.Equal(t, models.PaymentIntentProcessing, processing.PaymentIntentStatus)
	oldReplay := paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentCreateKey, declinedToken), http.StatusConflict)
	assert.Equal(t, processing.BankTransactionID, oldReplay.BankTransactionID)
	assert.Equal(t, models.BankTransactionScheduled, oldReplay.BankTransactionStatus)
	current, err := f.repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	require.Len(t, current.BankTransactions, 2)
	assert.Equal(t, models.BankTransactionScheduled, current.BankTransactions[0].Status)
	assert.Equal(t, models.BankTransactionDeclined, current.BankTransactions[1].Status)
	extraToken := f.token(t, "full", "0222405343240007")
	paymentProblem(t, f.confirm(intent.PaymentIntentID, "full", intentOtherKey, extraToken), http.StatusConflict)
	unused, err := f.cards.TryConsume(context.Background(), "merchant", extraToken)
	require.NoError(t, err)
	assert.NotNil(t, unused)
	f.clock.Add(int64(30 * time.Second))
	marked, err := f.repo.TryMarkTimedOutBankTransaction("merchant", processing.BankTransactionID, time.Unix(0, f.clock.Load()).UTC())
	require.NoError(t, err)
	assert.True(t, marked)
	pending := acceptedPaymentIntent(t, f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, token))
	require.Len(t, pending.BankTransactions, 2)
	assert.Equal(t, processing.BankTransactionID, pending.BankTransactions[0].BankTransactionID)
	assert.Equal(t, models.BankTransactionStatusUpdateTimeout, pending.BankTransactions[0].Status)
	assert.Equal(t, models.BankTransactionDeclined, pending.BankTransactions[1].Status)
	oldPendingReplay := acceptedPaymentIntent(t, f.confirm(intent.PaymentIntentID, "full", intentCreateKey, declinedToken))
	assert.Equal(t, pending.PaymentIntentID, oldPendingReplay.PaymentIntentID)
	assert.Equal(t, pending.BankTransactions, oldPendingReplay.BankTransactions)
	close(release)
	completed := <-done
	require.Equal(t, http.StatusOK, completed.Code)
	var completedIntent models.PaymentIntentResponse
	require.NoError(t, json.Unmarshal(completed.Body.Bytes(), &completedIntent))
	assert.Len(t, completedIntent.BankTransactions, 2)
	assert.EqualValues(t, 2, f.bank.calls.Load())
}

func TestPaymentIntentsHandlerHidesUnexpectedFailures(t *testing.T) {
	var output bytes.Buffer
	f := newIntentHandlerWithLogger(t, logging.NewConsole(&output))
	f.bank.process = func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		return banksimulator.BankAuthorizationResponse{}, errors.New("private-bank-details")
	}
	intent := f.create(t, "full", intentCreateKey)
	response := f.confirm(intent.PaymentIntentID, "full", intentConfirmKey, f.token(t, "full", "0222405343240007"))
	paymentProblem(t, response, http.StatusInternalServerError)
	assert.NotContains(t, response.Body.String(), "private-bank-details")
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	assert.Equal(t, "ERROR", record["level"])
	assert.Equal(t, "failed to process payment intent request", record["message"])
	assert.Contains(t, record["error"], "private-bank-details")
	assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
	assert.Equal(t, "merchant", record["merchant_id"])
	assert.Equal(t, intent.PaymentIntentID, record["payment_intent_id"])
	for _, field := range []string{"idempotency_key", "bank_transaction_id"} {
		assert.NotContains(t, record, field)
	}
	assert.NotContains(t, output.String(), "sk_full")
	assert.NotContains(t, output.String(), "0222405343240007")
}

func TestPaymentIntentAttemptErrorLogsKnownIDs(t *testing.T) {
	var output bytes.Buffer
	handler := NewPaymentIntentsHandler(nil, logging.NewConsole(&output))
	ctx := middleware.WithRequestID(context.Background(), "request")
	response := httptest.NewRecorder()
	handler.writeAttemptProblem(ctx, response, http.StatusConflict, "processing", &models.PaymentIntentResponse{
		PaymentIntentID: "intent",
		BankTransactions: []models.BankTransactionResponse{{
			BankTransactionID: "transaction", Status: "Unexpected",
		}},
	})
	require.Equal(t, http.StatusInternalServerError, response.Code)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	assert.Equal(t, "request", record["request_id"])
	assert.Equal(t, "intent", record["payment_intent_id"])
	assert.Equal(t, "transaction", record["bank_transaction_id"])
	assert.NotContains(t, record, "merchant_id")
}

func TestPaymentIntentHandlerLogsWriteFailureWithoutRetryingResponse(t *testing.T) {
	var logs bytes.Buffer
	fixture := newIntentHandlerWithLogger(t, logging.NewConsole(&logs))
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"currency":"GBP","amount":100}`, http.StatusCreated},
		{`{`, http.StatusBadRequest},
	} {
		logs.Reset()
		request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents", strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer sk_full")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Idempotency-Key", intentCreateKey)
		response := &unwritableResponse{header: make(http.Header)}
		fixture.router.ServeHTTP(response, request)
		assert.Equal(t, []int{test.status}, response.statuses)
		assert.Equal(t, 1, response.writes)
		var record map[string]interface{}
		require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
		assert.Equal(t, "ERROR", record["level"])
		assert.Equal(t, "failed to write HTTP response", record["message"])
		assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
		assert.Equal(t, "merchant", record["merchant_id"])
	}
	replay := fixture.create(t, "full", intentCreateKey)
	assert.Equal(t, models.PaymentIntentCreated, replay.Status)
	assert.Empty(t, replay.BankTransactions)
	assert.Zero(t, fixture.bank.calls.Load())
}
