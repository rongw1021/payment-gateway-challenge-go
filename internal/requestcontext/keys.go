// Package requestcontext defines typed keys shared by HTTP middleware and logging.
package requestcontext

type RequestIDKey struct{}
type MerchantIDKey struct{}
type IdempotencyKey struct{}
type PaymentIntentIDKey struct{}
type BankTransactionIDKey struct{}
