package banksimulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 64 * 1024

type Client interface {
	ProcessBankTransaction(context.Context, BankTransactionRequest) (BankAuthorizationResponse, error)
}

type HTTPClient struct {
	httpClient  *http.Client
	paymentsURL string
}

type BankTransactionRequest struct {
	CardNumber string `json:"card_number"`
	ExpiryDate string `json:"expiry_date"`
	Currency   string `json:"currency"`
	Amount     int32  `json:"amount"`
	Cvv        string `json:"cvv"`
}

func (BankTransactionRequest) String() string   { return "BankTransactionRequest" }
func (BankTransactionRequest) GoString() string { return "BankTransactionRequest" }

type BankAuthorizationResponse struct {
	Authorized        bool    `json:"authorized"`
	AuthorizationCode *string `json:"authorization_code"`
}

type FailureKind string

const (
	HTTPFailure            FailureKind = "http"
	NetworkFailure         FailureKind = "network"
	InvalidResponseFailure FailureKind = "invalid_response"
	TimeoutFailure         FailureKind = "timeout"
)

// Failure lets adapters classify failures without exposing bank response bodies.
type Failure struct {
	Kind       FailureKind
	StatusCode int
	Cause      error
}

func (f *Failure) Error() string { return "bank request failed (" + string(f.Kind) + ")" }
func (f *Failure) Unwrap() error { return f.Cause }

// NewClient owns a pool of up to five connections per host. Pool waits, connection
// establishment, and response reads share the overall HTTP request timeout.
func NewClient(baseAddress string, timeout time.Duration) (*HTTPClient, error) {
	address, err := url.Parse(baseAddress)
	if err != nil || address.Hostname() == "" ||
		(address.Scheme != "http" && address.Scheme != "https") ||
		(address.Path != "" && !strings.HasSuffix(address.EscapedPath(), "/")) ||
		address.User != nil || address.RawQuery != "" || address.ForceQuery ||
		strings.Contains(baseAddress, "#") {
		return nil, errors.New("bank simulator base address must be an absolute HTTP(S) URL ending in '/', without credentials, a query, or a fragment")
	}
	if timeout < time.Millisecond || timeout > (1<<31-1)*time.Millisecond {
		return nil, errors.New("bank simulator timeout must be between 1 and 2147483647 milliseconds")
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport must support cloning")
	}
	transport := defaultTransport.Clone()
	transport.MaxConnsPerHost = 5
	transport.MaxIdleConnsPerHost = 5
	transport.MaxIdleConns = 5
	return &HTTPClient{
		httpClient: &http.Client{
			Timeout: timeout, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		paymentsURL: address.ResolveReference(&url.URL{Path: "payments"}).String(),
	}, nil
}

// CloseIdleConnections releases pooled idle connections without interrupting active requests.
func (c *HTTPClient) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}

func (c *HTTPClient) ProcessBankTransaction(ctx context.Context, request BankTransactionRequest) (BankAuthorizationResponse, error) {
	if err := ctx.Err(); err != nil {
		return BankAuthorizationResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return BankAuthorizationResponse{}, fmt.Errorf("encode bank transaction: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.paymentsURL, bytes.NewReader(body))
	if err != nil {
		return BankAuthorizationResponse{}, fmt.Errorf("create bank request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return BankAuthorizationResponse{}, requestFailure(ctx, err)
	}
	defer response.Body.Close()
	if response.ContentLength > maxResponseBytes {
		return BankAuthorizationResponse{}, &Failure{Kind: InvalidResponseFailure}
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if len(body) > maxResponseBytes {
		return BankAuthorizationResponse{}, &Failure{Kind: InvalidResponseFailure}
	}
	if err != nil {
		return BankAuthorizationResponse{}, requestFailure(ctx, err)
	}
	if response.StatusCode != http.StatusOK {
		return BankAuthorizationResponse{}, &Failure{Kind: HTTPFailure, StatusCode: response.StatusCode}
	}

	// A missing or null decision must not be interpreted as a decline.
	var decision struct {
		Authorized        *bool   `json:"authorized"`
		AuthorizationCode *string `json:"authorization_code"`
	}
	if err := json.Unmarshal(body, &decision); err != nil {
		return BankAuthorizationResponse{}, &Failure{Kind: InvalidResponseFailure, Cause: err}
	}
	if decision.Authorized == nil ||
		(*decision.Authorized && (decision.AuthorizationCode == nil || strings.TrimSpace(*decision.AuthorizationCode) == "")) {
		return BankAuthorizationResponse{}, &Failure{Kind: InvalidResponseFailure}
	}
	return BankAuthorizationResponse{
		Authorized: *decision.Authorized, AuthorizationCode: decision.AuthorizationCode,
	}, nil
}

func requestFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return &Failure{Kind: TimeoutFailure, Cause: err}
	}
	return &Failure{Kind: NetworkFailure, Cause: err}
}
