# Payment Gateway Service Design

## 1. Purpose and Design Goals

This service implements a merchant-facing payment gateway for the
[Checkout.com engineering assessment](https://github.com/cko-recruitment).
It separates card tokenization, payment intent creation, bank authorization, and
retrieval of payment history.

The design prioritizes:

- **Payment safety:** distinguish a definitive decline from an unknown bank
  outcome, and avoid submitting another attempt merely because a response was lost.
- **Merchant isolation:** derive ownership from verified API keys and apply it
  consistently to tokens, intents, attempts, and idempotency records.
- **Predictable concurrency:** reserve attempts and consume tokens without allowing
  concurrent requests to create duplicate work.
- **Data minimization:** keep raw card details out of payment history, normal logs,
  and telemetry attributes.
- **Operational clarity:** make uncertain outcomes, stalled attempts, and service
  failures visible.
- **Assessment-appropriate simplicity:** use an in-memory database substitute,
  small service boundaries, and a single deployable application.

## 2. Scope, Assumptions, and Non-Goals

### Functional scope

The gateway can issue card tokens per merchant, create payment intents,
confirm them against a bank simulator, and retrieve the current intent with its
transaction history. Supported currencies are GBP, USD, and EUR. Amounts are
positive 32-bit integers expressed in the currency's minor unit.

An `Authorized` result represents the simulator's authorization decision. This
service does not model capture, settlement, refunds, or a financial ledger.

### Assumptions

- **An in-memory test double represents the database.** Tokens, payment state,
  idempotency records, and change events are process-local and lost on restart.
  The intended deployment is one gateway instance, not a distributed payment system.
- **An upstream failure does not prove that no payment was attempted.** A timeout,
  disconnected request, invalid response, or bank HTTP 503 can leave the actual bank
  outcome unknown. The gateway must not silently convert uncertainty into decline.
- **The bank exposes only a synchronous payment endpoint.** There is no
  transaction-status lookup or callback mechanism available to resolve uncertainty.
- **Local infrastructure is a development environment.** Public fake API keys
  and security-disabled observability services are not production configurations.

### Explicit non-goals

- **Request signing:** merchant authentication uses bearer API keys, not signed
  request bodies. Internal request fingerprints are not authentication signatures.
- **Incoming bank webhooks:** no callback endpoint, signature verification,
  callback deduplication, or asynchronous bank-event ingestion is implemented.
- **Outgoing merchant webhooks:** payment-intent events have a notification
  placeholder, but it only logs a warning; no merchant callback is sent.

## 3. Architecture and Responsibility Boundaries

The client and merchant backend are separate integration actors outside this
repository. Card tokenization goes from the client to the Payment Gateway Service
without passing through the merchant backend. Payment-intent creation, confirmation,
and retrieval go through the merchant backend. Both paths enter through the load
balancer shown in the deployment architecture.

![Payment Gateway Service architecture with middleware, controllers, application and background services, and repositories and integrations; an external load balancer sits between the merchant backend and service, and direct client tokenization also passes through it](images/payment-gateway-architecture.svg)

[Open the full-size SVG](images/payment-gateway-architecture.svg).

The load balancer represents external deployment infrastructure; it is not included
in the local Compose stack. It does not imply that this implementation supports
multiple gateway instances: payment state, tokens, idempotency, and rate limits
remain process-local.

The gateway is organized into four explicit layers:

1. **Middleware:** request policies, authentication, authorization, and HTTP telemetry.
2. **Controllers:** HTTP handlers, model binding, input validation, and response mapping.
3. **Application services and background services:** separate, color-coded groups
   within the same layer. Audit and Reconciliation, Audit and Webhook, and the
   Timeout Sweeper run as three distinct background services.
4. **Repositories and integrations:** repositories and Change Data Capture (CDC)
   sit alongside the separately colored bank integration. Two independent in-memory
   CDC streams carry bank-transaction and payment-intent changes.

Observability spans all four layers rather than forming another layer. Solid
arrows show selected API, state, and event dependencies. Purple dashed arrows show
telemetry export; the amber dashed arrow shows merchant webhook delivery, which is
not implemented. This is a component view, not an execution sequence or an exhaustive
call graph. HTTP responses return over their corresponding request paths.

The gateway returns the card token directly to the client. The client then passes
that token, rather than PAN/CVV, to the merchant backend. The backend owns checkout
orchestration, validates order details and amounts, supplies payment idempotency
keys, and relays gateway results to the client. Those merchant-side responsibilities
are not implemented by this gateway.

| Component | Responsibility |
|-----------|----------------|
| Client / checkout UI | Submit card details directly for tokenization; send the resulting token and checkout requests to the merchant backend |
| Merchant backend | Own order/payment orchestration and call the gateway's payment-intent endpoints using server-side API keys |
| Load balancer | Route client and merchant-backend requests to the service as external deployment infrastructure |
| API composition and lifecycle | Construct dependencies, register routes, and supervise HTTP and background workers |
| Middleware | Request correlation, cache policy, admission control, authentication, authorization, idempotency-header validation, and HTTP telemetry |
| Controllers and model binding | Validate HTTP input and map service outcomes to stable response contracts |
| Application services | Coordinate business operations, token consumption, bank calls, and outcome recording |
| Repositories | Enforce ownership, idempotency, state transitions, and consistent in-process mutations |
| Change Data Capture (CDC) | Carry bank-transaction and payment-intent changes through separate in-process FIFO streams |
| Bank adapter | Reuse bounded connections, enforce the overall request timeout, and classify bank responses and transport failures |
| Background services | Audit events, report unresolved outcomes and unimplemented notifications, and recover stalled attempts |

The API retains services and workers rather than exposing repositories as direct
API dependencies. Dependencies are injected through interfaces where substitution
is useful, especially for the bank and storage boundaries.

Static input validation belongs at the HTTP boundary. Time-sensitive checks and
business invariants remain in services and repositories. Internal callers must
supply validated requests and trusted metadata; the design does not duplicate
every HTTP validation rule throughout the stack.

### Successful Payment Authorization

The following sequence shows one successful checkout with valid API keys, fresh
creation and confirmation idempotency keys, and an unused card token. Gateway
self-messages summarize service and repository work; middleware, background workers,
and observability are omitted. The external load balancer and client API key
boundaries described above still apply.

```mermaid
sequenceDiagram
    autonumber
    actor Client as Client / checkout UI
    participant Merchant as Merchant backend
    participant LB as Load balancer
    participant Gateway as Payment Gateway Service
    participant Bank as Bank simulator

    Note over Client,Gateway: Tokenize without sending card details to the merchant backend
    Client->>LB: POST /api/CardTokens<br/>Card details
    LB->>Gateway: Tokenize card
    Gateway->>Gateway: Store merchant-scoped, single-use token
    Gateway-->>LB: Card token
    LB-->>Client: Card token

    Client->>Merchant: Checkout request + card token
    Merchant->>Merchant: Validate order<br/>Determine amount and currency

    Note over Merchant,Gateway: Create the payment intent
    Merchant->>LB: POST /api/PaymentIntents<br/>Amount, currency + creation key
    LB->>Gateway: Create payment intent
    Gateway->>Gateway: Create intent in Created state
    Gateway-->>LB: Created intent + intent ID
    LB-->>Merchant: Created intent + intent ID

    Note over Merchant,Bank: Confirm and authorize
    Merchant->>LB: POST /api/PaymentIntents/{id}/confirm<br/>Card token + confirmation key
    LB->>Gateway: Confirm payment intent
    Gateway->>Gateway: Consume token and reserve Scheduled attempt<br/>Created to Processing
    Gateway->>Bank: Authorize card for the intent's amount and currency
    Bank-->>Gateway: Authorized + authorization code
    Gateway->>Gateway: Record authorization and read current intent<br/>Processing to Authorized
    Gateway-->>LB: Authorized intent + transaction history
    LB-->>Merchant: Authorized intent + transaction history
    Merchant-->>Client: Authorization succeeded

    opt Retrieve the payment later
        Client->>Merchant: Request payment status
        Merchant->>LB: GET /api/PaymentIntents/{id}
        LB->>Gateway: Retrieve merchant-owned payment intent
        Gateway-->>LB: Authorized intent + transaction history
        LB-->>Merchant: Authorized intent + transaction history
        Merchant-->>Client: Current payment status
    end
```

Confirmation already returns the payment intent and its transaction history, so a
separate retrieval is optional. `Authorized` means bank authorization, not capture
or settlement. Tokenization is shown first here, but intent creation can precede
it; both must complete before a new confirmation attempt.

## 4. Authentication, Authorization, and Card Data Protection

### Merchant identity and permissions

An API key identifies a merchant and grants explicit endpoint scopes. The server
stores a SHA-256 hash of the API key payload, excluding the `sk_` prefix, and
compares hashes in constant time. This avoids retaining a directly usable API key
in the authentication configuration; it still assumes strong API keys outside
development.

Scopes are independent: token creation, intent creation, confirmation, and retrieval
can be granted separately. Authentication and scope checks occur before request
model binding. Invalid API keys produce 401; missing permissions produce 403.

The direct-client tokenization path must not expose the merchant backend's API key
with payment-intent permissions. Client authorization should be limited to
tokenization and bound to the intended merchant. The current gateway supports
separately scoped static API keys, but does not implement a publishable API key
or short-lived client-session mechanism. A production browser integration also needs
an appropriate origin/CORS policy when the gateway is on another origin. These
client-integration controls remain to be designed; the diagram does not imply
that a production browser SDK or client API key provisioning already exists.

Ownership is a separate rule from permissions. A merchant with read permission
still cannot retrieve another merchant's intent. Missing and foreign intents
both produce 404, avoiding disclosure of another merchant's resources. Client
headers cannot substitute an unverified merchant identity.

### Card tokenization and data lifecycle

A token is a merchant-bound, single-use reference to card details:

1. Validate card syntax and expiry.
2. Generate a canonical UUIDv7 token and assign a 15-minute lifetime.
3. Store the card details under the merchant and the token's SHA-256 hash.
4. Return the plaintext token once to the client, which passes it to the merchant
   backend for payment confirmation.
5. On a new confirmation attempt, consume the token atomically and remove its
   repository entry.

Tokenization and intent creation are independent preparation operations; either
can occur first. The merchant backend's payment-intent requests carry a token
rather than PAN/CVV, so it need not receive raw card details in this integration.
Payment records retain only the last four digits and expiry information.
Card expiry is also checked at consumption, since it may have changed since issuance.

Expiry is enforced at the exact token deadline, but cleanup is lazy: expired entries
are removed during consumption attempts, not by a dedicated cleanup worker.
Consequently, an expired token is unusable even if its raw card data has not yet
been physically removed from memory.

### Compliance boundary

The gateway still receives PAN/CVV, temporarily holds them in process memory, and
sends them to the bank. Go garbage collection is not secure memory erasure.
UUIDv7 tokens also contain time information and are not equivalent to a dedicated
high-entropy vault-token design.

Production use would require an appropriate card-data handling architecture,
transport protection, access controls, retention policies, and compliance review.
The current tokenization design is a data-minimization measure, not a compliance
claim. Normal application logs exclude card bodies and tokens; explicitly enabled
development-test transcripts are a separate, unredacted diagnostic facility.

## 5. Payment Lifecycle and Consistency

### Domain model

| Entity | Meaning |
|--------|---------|
| Card token | A short-lived, merchant-owned reference that can fund one new attempt |
| Payment intent | The merchant's payment request: fixed amount/currency, current state, and associated history |
| Bank transaction | One authorization attempt, with its own status, masked card snapshot, timestamps, and originating request ID |
| Change event | A creation snapshot or update delta used for operational observation |

A payment intent can have several attempts after definitive declines. History is
returned oldest first. Public responses omit full card details, internal
fingerprints, idempotency keys, and bank authorization codes.

The following tables use the Go domain field names. **Public API** indicates whether
the field is included in the response models, rather than whether it can be
serialized from the internal entity.

#### Payment intent fields

The [payment intent](internal/models/payment_intent.go) owns the payment's fixed
amount and currency, current business state, and attempt history.

| Field | Type | Purpose | Public API |
|-------|------|---------|------------|
| `PaymentIntentID` | `string` | Gateway-generated UUIDv7 identifying the payment intent throughout its lifecycle. | Yes |
| `MerchantID` | `string` | Owner derived from the authenticated API key; used to enforce merchant isolation. | No |
| `Status` | `PaymentIntentStatus` | Current business state: `Created`, `Processing`, `Authorized`, `Declined`, or `PendingVerification`. | Yes |
| `Currency` | `string` | Fixed currency for all attempts: GBP, USD, or EUR. | Yes |
| `Amount` | `int32` | Fixed positive amount in the currency's minor unit; all attempts authorize this amount. | Yes |
| `CreatedAt` | `time.Time` | Immutable UTC time the intent was created. | Yes |
| `UpdatedAt` | `time.Time` | UTC time of the latest intent status change; initially equal to `CreatedAt`. | Yes |
| `IdempotencyKey` | `string` | Merchant-scoped creation idempotency key, allowing repeated creation requests to resolve to the same intent. | No |
| `RequestFingerprint` | `string` | HMAC-SHA256 fingerprint of the creation request's amount and currency; detects changed input under an existing creation key. | No |
| `BankTransactions` | `[]BankTransaction` | Associated authorization attempts, returned oldest first; empty before the first attempt. | Yes, using the public transaction fields below |

Reads, rejected requests, and idempotent replays do not advance these timestamps.
A bank-transaction update that leaves the intent status unchanged does not advance
`UpdatedAt` either. Responses expose the timestamps as `created_at` and `updated_at`.

#### Bank transaction fields

A [bank transaction](internal/models/bank_transaction.go) represents one reserved
authorization attempt. Its amount and currency come from the parent intent; its
card snapshot contains neither PAN/CVV nor the card token.

| Field | Type | Purpose | Public API |
|-------|------|---------|------------|
| `BankTransactionID` | `string` | Gateway-generated UUIDv7 identifying this attempt, not an identifier assigned by the bank. | Yes |
| `PaymentIntentID` | `string` | Links the attempt to its parent intent; not repeated in the public transaction nested under that intent. | No |
| `MerchantID` | `string` | Owner of the attempt, matching the parent intent's merchant. | No |
| `Status` | `BankTransactionStatus` | Attempt-specific state: `Scheduled`, a definitive bank decision, or an uncertainty reason detailed in the state model below. | Yes |
| `CardNumberLastFour` | `string` | Last four digits of the card used for this attempt; the full card number is not retained here. | Yes |
| `ExpiryMonth` | `int` | Expiry month copied from the consumed token's card details. | Yes |
| `ExpiryYear` | `int` | Expiry year copied from the consumed token's card details. | Yes |
| `CreatedAt` | `time.Time` | UTC time the attempt was reserved; also used to detect locally stalled attempts. | Yes |
| `IdempotencyKey` | `string` | Merchant-scoped confirmation idempotency key; matching replays reuse this attempt rather than submitting another bank request. | No |
| `RequestFingerprint` | `string` | HMAC-SHA256 fingerprint binding the confirmation to its intent ID and card token; detects conflicting reuse of the confirmation key. | No |
| `OriginatingRequestID` | `string` | Identifies the confirmation request that first reserved the attempt; retained for correlation, including subsequent change events. | No |
| `CompletedAt` | `*time.Time` | Nil while `Scheduled`; otherwise the UTC time the gateway recorded the latest outcome, including uncertainty or a later refinement. It is not a settlement timestamp. | Yes, nullable |
| `AuthorizationCode` | `*string` | Optional bank-supplied code; a nonblank value is required for a successful authorization. Retained internally, not returned to the merchant. | No |

The HTTP response also includes `request_id`, identifying the current request.
It is response metadata, not a payment-intent field, and is distinct from the
attempt's retained `OriginatingRequestID`.

### Two-step intent workflow

**Create:** the merchant backend asks the gateway to record the intended amount
and currency without contacting the bank. This gives the backend a stable resource
and supports retries before payment submission.

**Confirm:** the merchant backend submits the client-provided card token and a
confirmation idempotency key. The gateway reserves an attempt, calls the bank, and
records the result. Retrieval requests also go through the merchant backend.
This is not an authorization/capture split or a distributed two-phase commit.

For a new confirmation:

1. Check merchant ownership and whether the key identifies a replay or conflict.
2. Check whether the intent permits a new attempt.
3. Consume the token and reserve a `Scheduled` bank transaction, moving the intent
   to `Processing`.
4. Release repository locks before making the external HTTP call.
5. Record a definitive or uncertain outcome and expose the resulting intent state.

### In-process consistency

A single payment-repository mutex serializes idempotency checks, state checks,
attempt reservation, and completion. Token consumption uses the card repository's
own mutex while reservation is serialized. Returned snapshots are detached from
stored state so callers cannot mutate repository data indirectly.

This deliberately simple approach makes races easier to reason about:

- One token cannot be consumed by two competing intents.
- Concurrent matching confirmations reuse one reserved attempt.
- An intent cannot start another attempt while processing, authorized, or pending
  verification.
- A timeout sweep must recheck the attempt before changing it, so a concurrent
  completion can win safely.

These are **concurrency guarantees, not database transaction guarantees**. There
is no general rollback across token consumption, attempt creation, and event
publication. For example, a later failure can occur after a token has been consumed;
publication failure can occur after payment state has already changed.

The trade-off is limited scalability: payment operations share a lock, and
in-memory collections are scanned. This is suitable for the assessment, not a
substitute for transactional storage and indexed queries.

### State model

The intent summarizes business progress; the bank transaction preserves the
specific attempt outcome.

| Bank transaction status | Payment intent state | Interpretation |
|-------------------------|----------------------|----------------|
| No attempt | `Created` | Ready for confirmation |
| `Scheduled` | `Processing` | Reserved; bank submission/completion is in progress |
| `Authorized` | `Authorized` | Definitive authorization |
| `Declined` | `Declined` | Definitive decline; a new attempt is permitted |
| `ServiceUnavailable`, `NetworkError`, `InvalidResponse`, `RequestTimeout`, `RequestCanceled`, `StatusUpdateTimeout` | `PendingVerification` | No reliable final bank decision |

```mermaid
stateDiagram-v2
    [*] --> Created
    Created --> Processing: Reserve first attempt
    Declined --> Processing: Reserve a new attempt
    Processing --> Authorized: Definitive authorization
    Processing --> Declined: Definitive decline
    Processing --> PendingVerification: Uncertain or stalled outcome
    PendingVerification --> Authorized: Late definitive result
    PendingVerification --> Declined: Late definitive result
```

Only `Created` and `Declined` permit new attempts. A pending attempt may be refined
by a valid completion of the already-running operation; the repository supports
this transition, but there is no bank verification endpoint or webhook to fetch
such a result independently. Definitive outcomes of an individual bank transaction
cannot be changed into another outcome.

`CompletedAt` records a gateway outcome transition, including uncertain outcomes.
It is not proof of settlement or proof that the bank did not process a request.

### Idempotency and replay semantics

Idempotency prevents repeated side effects, rather than caching an immutable HTTP
response. Creation and confirmation use separate merchant-scoped key namespaces.

| Operation | Fingerprinted request |
|-----------|-----------------------|
| Intent creation | Currency and amount |
| Confirmation | Intent ID and card token |

Fingerprints use HMAC-SHA256 over the typed request data, with an in-memory
repository key. A matching key and fingerprint return the current snapshot.
Different details under a previously reserved key produce a conflict.

Confirmation replay is checked before token availability and payability. Therefore
a matching retry can succeed after token consumption or expiry without consuming
another token or submitting another bank request. Validation failures that precede
reservation do not permanently reserve the key.

Responses describe the **current intent**, including later history:

| Current intent state on a matching confirmation replay | HTTP response |
|--------------------------------------------------------|---------------|
| `Authorized` or `Declined` | 200 with current intent and full history |
| `Processing` | 409 with attempt context |
| `PendingVerification` | 202 with current intent, full history, and a Location lookup URL |

For example, replaying an old declined attempt after a newer attempt authorizes
returns the authorized intent. The old attempt remains declined in history.
Clients must not interpret the aggregate status as a changed outcome for that
specific old attempt.

Idempotency records, fingerprints, and keys are not durable. These semantics do
not provide exactly-once execution across crashes, restarts, or multiple gateway
instances, and the bank protocol does not provide a coordinated transaction with
local storage.

## 6. Bank Integration and Uncertain Outcomes

The bank adapter has a deliberately narrow success contract: HTTP 200 with an
explicit boolean authorization decision, and a nonblank authorization code when
authorized. Missing or malformed decisions are errors, never implicit declines.

### Connection reuse and timeout budget

The adapter reuses a pool of up to five connections per host, reducing connection
setup overhead while bounding connection usage. Requests wait for an available
connection when the pool is saturated. This is separate from the gateway's
request-rate limit, not a global limit on concurrent payment requests.

The adapter adds no automatic retry policy. HTTP error responses, lost responses,
and timeouts after submission do not trigger another payment POST. Pool waiting,
connection establishment, and response reading share one overall HTTP request
timeout, configured in [config.development.yaml](config.development.yaml).

The adapter also limits response size and does not follow redirects, avoiding
forwarding card data to a redirected destination.

### Uncertain outcomes

- **Declined:** the bank provided a valid, definitive refusal. The merchant may
  create a new attempt using a new token/key.
- **Pending verification:** the outcome is uncertain. Retrying under a new key
  could duplicate a real bank operation, so the intent is blocked from new attempts.

Likewise, client cancellation is not evidence that the bank stopped processing.
The service records recognized uncertain outcomes when possible, although a
disconnected caller may receive no response. Unexpected internal errors propagate
rather than being disguised as a bank decision.

Automatic recovery is constrained by the bank simulator's limited contract. For a
valid request, the card number determines the authorization decision or simulated
HTTP 503. Repeating the same bank request repeats that configured outcome, although
an authorization code may change. The simulator exposes no transaction-status or
verification API, so resubmission cannot verify the result of an earlier attempt.

The gateway therefore avoids automatic resubmission and retains
`PendingVerification` unless the original in-flight request later yields a
definitive result.

## 7. Background Processing and Event Auditing

Three supervised services separate event observation and stalled-attempt recovery
from HTTP request handling:

| Service | Input | Responsibility |
|---------|-------|----------------|
| Audit and Reconciliation | Bank-transaction events | Audit each event and warn when an updated status requires verification |
| Audit and Webhook | Payment-intent events | Audit each event and log a merchant-notification placeholder; no webhook is delivered |
| Timeout Sweeper | Scheduled attempts exceeding the status-update timeout | Move stale attempts and their intents into pending verification |

### Change Data Capture (CDC)

The repository acts as a test double for database storage, and its change events
simulate database CDC. Bank transactions publish `banktransaction.created` and
`banktransaction.updated`; payment intents publish `paymentintent.created` and
`paymentintent.updated` on creation and actual intent status changes.

The streams use independent FIFO queues. Publication order and sequence numbers
apply within each stream, not across both streams. Creation events capture the
entity's state at creation; update events carry the changes at that point in time.
Consumers audit these events rather than re-reading potentially newer state.

Reads, rejected operations, and matching replays emit no new intent events. A
changed bank uncertainty reason can produce a bank-transaction event without an
intent event if the intent remains `PendingVerification`.

Correlation follows the operation that caused the change: intent creation retains
the creation request ID, while attempt-driven changes retain the original
confirmation request ID. Merchant and entity identifiers connect both streams
without duplicating bank history inside payment-intent events.

### Recovery from a failed database status update

The timeout worker models recovery from a production failure in which **the bank
API has returned a decision, but the subsequent database update fails**. The bank
call and local status update are separate operations: the bank may have authorized
the payment while the stored bank transaction remains `Scheduled` and its payment
intent remains `Processing`.

The repository test double represents the database, and a stale `Scheduled`
attempt models this missing status update. The background sweep detects such
records so that they do not remain indefinitely in progress.

The timeout worker runs a sweep on startup and then periodically. An attempt is
eligible when it is still `Scheduled`, has no completion time, and:

```text
current time - creation time >= configured status-update timeout
```

Each sweep processes a bounded batch. The repository rechecks eligibility under
its mutation lock, then marks `StatusUpdateTimeout`, moves the intent to
`PendingVerification`, and emits the corresponding bank-transaction and
payment-intent CDC events.

`StatusUpdateTimeout` identifies a missing recorded outcome, not a bank decline.
The transition makes the unresolved status visible to the reconciliation service
and keeps new attempts blocked, avoiding resubmission of a payment the bank may
already have authorized. If the original operation later records a definitive
result, it can refine the existing attempt without submitting another payment.

## 8. Resilience and Service Lifecycle

### Admission control

Two independent sliding-window limiters bound admissions in any rolling second:

- **Global:** defaults to 100 requests per second per process, before authentication.
  It covers all routes except `GET /ping`, which remains available for health checks.
- **Per merchant:** defaults to 10 requests per second, after authentication and
  scope checks but before idempotency-key validation, body binding, and payment
  processing. All API keys and protected routes for the same merchant share one quota.

Either stage rejects excess requests with 429 and `Retry-After: 1`. Authentication
and scope failures do not consume merchant quota; admitted requests with invalid
idempotency keys or bodies do. A merchant-level rejection still consumes global capacity,
because that request has already passed the coarse limiter.

Both limits are per process. They do not bound in-flight concurrency, guarantee
fairness under global saturation, or coordinate a multi-instance deployment.

### Bounded work and failure handling

| Control | Design purpose |
|---------|----------------|
| 1 MiB HTTP request-body limit | Bound model-binding input |
| Header and request-read deadlines | Bound slow uploads before payment processing |
| Response-write and idle-connection deadlines | Release stalled responses and unused keep-alive connections |
| 64 KiB bank response limit | Bound untrusted upstream data |
| Five bank connections per host | Bound connection usage and enable reuse |
| Overall bank request timeout | Bound connection acquisition and the complete bank interaction |
| Bounded timeout-sweep batches | Limit work performed by each recovery pass |
| Bounded asynchronous telemetry queues and export timeouts | Keep observability outages off the payment response path |

The response-write budget must leave room for request reading and the configured
bank timeout. Slow POST body uploads return 408 before business processing;
incomplete headers or stalled response writes may instead close the connection.

Unexpected handler panics produce sanitized 500 ProblemDetails if no response has
started. Once a response is committed, recovery aborts the connection or stream
rather than appending another response. Response-delivery failures are logged;
they do not roll back payment state or prove that the bank declined the payment.

The HTTP server and all three background workers share a supervised lifecycle.
Unexpected worker failure stops the server and sibling workers rather than leaving
a partially functioning instance running unnoticed.

Cancellation propagates to active work. HTTP shutdown uses a fresh bounded context
after cancellation, and observability gets a separate bounded flush phase. This
does not guarantee completion of in-flight bank operations or draining of all
change events. The liveness endpoint does not assert bank or telemetry availability.

### Retry policy

Automatic exponential-backoff retries are intentionally omitted for bank submissions
and repository operations in this assessment. The bank simulator returns the same
configured outcome for a given valid input and provides no verification API. The
in-memory repository represents database storage without a remote connection or
transient database failures to recover from.

Clients can replay requests using the same idempotency key without creating another
payment attempt. An uncertain bank outcome must not trigger automatic resubmission.
With real integrations, bounded backoff retries would be considered only for
transient failures where repeating the operation is safe; backoff alone does not
prevent duplicate payments.

## 9. Observability

Logs, metrics, and traces share a service identity and use OpenTelemetry to reach
the Collector, which owns Elasticsearch-specific export. JSON console logging
remains independent of that infrastructure.

| Signal | Current scope and purpose |
|--------|---------------------------|
| HTTP request summaries | INFO for card-token/payment-intent route families, including error responses; route template, method, status, latency, request ID |
| Application errors | ERROR records for unexpected application failures, recovered panics, and failed response writes |
| Event audits | INFO records containing sanitized serialized events, with merchant, event type, sequence, and available correlation IDs promoted as structured fields |
| Pending-verification warnings | Highlight outcomes that cannot be resolved automatically |
| Webhook placeholder warnings | Indicate that merchant notification delivery is not implemented |
| HTTP metrics | Request counts, non-5xx counts, and duration histograms by method, route template, and status |
| Traces | One server span around the card-token handler, with upstream trace-context propagation |

Metrics use delta counts and aggregatable histograms:

- **QPS:** admitted and rejected HTTP requests recorded in the instrumented route
  families, divided by the query/bucket duration.
- **HTTP success rate:** non-5xx responses divided by all recorded responses.
  This intentionally includes 4xx responses and is not the payment authorization rate.
- **P95 latency:** an estimate from merged histograms, not an average of independently
  calculated percentiles.

Handlers, services, and workers receive a shared structured logger. Context-aware
records include `request_id`, `merchant_id`, `payment_intent_id`, and
`bank_transaction_id` when available; absent fields are omitted. Errors are separate
attributes rather than interpolated into message text. Audit messages deliberately
retain the serialized event, with selected fields also promoted for filtering.

Request IDs connect HTTP logs with both event streams. Replays receive new request
IDs, while attempt-driven events retain the originating confirmation's ID. Metric
dimensions exclude high-cardinality request and transaction identifiers.

Export is best effort. Queues can overflow, failed delta exports can undercount
metrics, and shutdown may leave telemetry undelivered. Telemetry is therefore
diagnostic evidence, not the authoritative payment record or a durable audit trail.

Kibana supplies the
[Payment Gateway Dashboard](http://localhost:5601/app/dashboards#/view/payment-gateway-dashboard),
which combines HTTP throughput, success rate, P95 latency, gateway logs, and
card-token traces in a single view. Separate data views support exploration of each
signal. Startup provisions stable saved-object IDs without replacing existing
customizations.

> **Note:** The dashboard is available after starting the observability stack and
> completing its automatic setup. Follow the observability option in
> [Running in Docker](#running-in-docker).

The trace panel is currently a span table: Jaeger-style waterfalls, nested
card-operation spans, and full payment/bank tracing are not implemented.

![Payment Gateway Dashboard showing HTTP request metrics, gateway logs, and card-token traces](images/payment_gateway_dashboard.png)

*Screenshot captured after an E2E test run, showing the resulting metrics, logs, and
traces. Metrics may take tens of seconds to appear: the gateway exports every 10
seconds, followed by Collector batching, Elasticsearch indexing, and the dashboard's
default 10-second refresh cycle. Leave the stack running and refresh the dashboard
after allowing time for export.*

## 10. API Contract and Documentation

| Endpoint | Responsibility | Main success response |
|----------|----------------|-----------------------|
| `POST /api/CardTokens` | Tokenize validated card details | 200 with token and expiry |
| `POST /api/PaymentIntents` | Create or replay an intent | 201 with current intent and Location |
| `POST /api/PaymentIntents/{id}/confirm` | Submit or replay a confirmation | 200 for a definitive authorization or decline; 202 with Location for pending verification |
| `GET /api/PaymentIntents/{id}` | Retrieve merchant-owned current state/history | 200 for an existing owned intent |

Creation, retrieval, and confirmation responses with HTTP 200 or 202 share the
payment-intent response shape, including complete transaction history. A 202
response means the confirmation was accepted but the bank outcome remains
unconfirmed; it is not an authorization or a promise of eventual verification.
Its `Location` header points to the intent's retrieval endpoint.

Matching confirmation replays remain 202 while the intent is pending. Retrieval
returns 200 with the current state, while a new attempt on a pending intent returns
409. Processing confirmations also return 409, using ProblemDetails with attempt
context. Clients must not resubmit an uncertain payment with a new idempotency key.

The HTTP contract distinguishes validation errors (400), authentication (401),
authorization (403), unavailable/foreign resources (404), request-body read timeouts
(408), state/idempotency conflicts (409), oversized or unsupported input (413/415),
and throttling (429).
Unexpected request-processing or outcome-recording failures remain server errors
(500), rather than being converted to 202, and do not expose raw bank payloads.

Swagger 2.0 is generated from source annotations and model metadata. Generated
JSON, YAML, and the embedded document are checked for consistency, including error
media types, correlation/cache headers, retry guidance, and nullable timestamps.
Swagger UI uses the current origin rather than a hard-coded deployment address.
See the [generated API contract](docs/swagger.json) for field-level details.

## 11. Testing Strategy

> **Verified environment:** All test commands below were verified on Windows 11
> Enterprise (version 10.0.26300, build 26300), PowerShell 7.6.10 (Core), and
> Go 1.27.4 (`windows/amd64`).

### Unit and component tests

Tests alongside the implementation verify repository concurrency and snapshots,
token expiry, validation boundaries, error mapping, bank-protocol failures, worker
lifecycle, rate limiting, and observability. Fake clocks and controlled dependencies
exercise timing and failure cases that cannot reliably be forced through a deployed
API. Race-enabled runs complement functional assertions.

Focused cases also cover connection-pool limits and reuse, request timeouts and
cancellation, slow requests, panic recovery, failed response writes, independent
event streams, and timestamps that remain unchanged on replay.

Run the internal unit and component tests from the repository root:

```powershell
go test -v -race .\internal\... -count=1
```

Unit/component statement coverage:

| Scope | Statement coverage |
|-------|--------------------|
| Internal application packages | **92.5%** |

This figure covers production Go packages under [internal](internal/), exercised
by their unit and component tests. Integration and E2E tests are assessed
separately.

To measure this scope:

```powershell
go test -race -covermode=atomic '-coverpkg=github.com/cko-recruitment/payment-gateway-challenge-go/internal/...' .\internal\... -coverprofile=coverage.out -count=1
go tool cover -func=coverage.out
```

### Integration and end-to-end tests

Both fixtures execute the same [shared HTTP scenario suite](test/scenarios/scenarios.go)
and response assertions. Only the test environment differs; business scenarios
are not duplicated between integration and E2E tests.

#### Test scenarios

| Category | Main cases |
|----------|------------|
| [Authentication & Authorization](test/scenarios/authentication.go) | Missing, malformed, or unknown API keys; duplicate authorization headers; required scopes; forbidden operations; merchant isolation; authentication before request validation |
| [Card tokenization](test/scenarios/card_tokenization.go) | Valid cards and input boundaries; missing/invalid fields; expired cards; malformed JSON; content type and body-size limits; token format, expiry metadata, and response privacy |
| [Payment intent creation](test/scenarios/payment_intent_creation.go) | Supported currencies and amount boundaries; missing/invalid idempotency keys; matching replay; changed-payload conflicts; merchant-scoped keys; concurrent creation |
| [Payment intent confirmation](test/scenarios/payment_intent_confirmation.go) | Input validation; permitted and blocked states; authorization, decline, and 202 pending-verification outcomes; new attempts after declines; immediate/historical replays; token ownership and single use; concurrent confirmations |
| [Payment intent retrieval](test/scenarios/payment_intent_retrieval.go) | Invalid, missing, or foreign IDs; Created, Authorized, Declined, and PendingVerification states; complete ordered history; masked card details |
| [Rate limiting](test/scenarios/rate_limit.go) | Global throttling before authentication; per-merchant quota across routes and merchant isolation; 429 responses and Retry-After headers; recovery; GET `/ping` exemption |

Cases cover meaningful input equivalence classes, state transitions, ownership,
replays/conflicts, and competing requests rather than a Cartesian product of invalid
inputs. Exact token-expiry boundaries, controlled `Processing` states, cancellations,
and late completions remain deterministic lower-level tests.

#### Integration tests

Integration tests start the HTTP API, application services, repository test doubles,
bank HTTP client, and background workers on an ephemeral loopback listener. A local
mock HTTP server substitutes only for the bank; no Docker stack is required.

Run from the repository root:

```powershell
go test -v .\test\integration -count=1
```

#### End-to-end tests

E2E tests exercise the deployed HTTP contract against an already-running gateway
and bank simulator. They do not start, reconfigure, or stop those services.
Configure the gateway URL and test API keys in
[config.e2e.yml](test/e2e/config.e2e.yml) to match the deployment. Use an isolated
development instance with the default limits of 100 requests per second globally
and 10 per merchant, as expected by the shared rate-limit scenarios. Ordinary
scenarios are paced below the merchant limit; business-concurrency cases run
separately from intentional rate-limit bursts.

1. Start the gateway and bank simulator following [Running in Docker](#running-in-docker).
2. Run the E2E tests from the repository root with the opt-in `e2e` build tag:

   ```powershell
   go test -v -tags=e2e .\test\e2e -count=1
   ```

Both commands use native Go verbose output to report scenario and case results.
Append `-args -http-verbose` for development-only HTTP transcripts, including
unredacted request/response headers and bodies.

Bootstrap tests separately verify Kibana provisioning and preservation of existing
objects.

## 12. Deployment and Configuration

The gateway is a single process packaged in a multi-stage Docker image. The runtime
is non-root and uses a read-only filesystem. Configuration is mounted at runtime,
not embedded in the image.

The base Compose definition runs the gateway and bank simulator. The observability
override adds the Collector, Elasticsearch, Kibana, and a one-shot setup service.
Elasticsearch retains telemetry and saved dashboards in its volume; that persistence
does not make gateway payment state durable.

Typed YAML is loaded once at startup. `ENVIRONMENT` selects the profile, defaulting
to `development`; `OBSERVABILITY` can explicitly override its export-enabled flag.
The container uses the development file directly with Docker service addresses.
Host execution uses a local profile with localhost addresses.

The profile also controls inbound HTTP deadlines and the overall bank request
timeout. Omitted HTTP deadlines default to 5s for headers, 15s for request reading,
30s for response writing, and 60s for idle connections; the supplied development
profile overrides request reading to 10s.
Timeout combinations that leave no response budget beyond request reading and
the bank call are rejected at startup.

Rate-limit defaults and the supplied development profile use:

```yaml
rate_limit:
  global_requests_per_second: 100
  per_merchant_requests_per_second: 10
```

Both values must be positive integers. Older profiles must rename
`requests_per_second` to `global_requests_per_second`; an omitted merchant limit
defaults to 10.

Configuration changes require a restart or recreation, which also clears gateway
state. The development stack exposes the gateway and observability interfaces on
loopback, but uses local HTTP and a security-disabled Elasticsearch/Kibana setup.
Real deployments need protected secrets, TLS, infrastructure access controls, and
an explicit operational security policy.

### Running in Docker

> **Verified environment:** All Docker commands below were verified on Windows 11
> Enterprise (version 10.0.26300, build 26300), PowerShell 7.6.10 (Core), and
> Docker Desktop 4.98.0 (build 245422).

Run either command from the repository root. Both modes mount
[config.development.yaml](config.development.yaml); no separate Docker configuration
is needed.

**Gateway and bank simulator:**

```powershell
docker compose up --build -d
```

**With observability (Collector, Elasticsearch, and Kibana):**

```powershell
docker compose -f docker-compose.yml -f docker-compose.observability.yml up --build -d
```

> **First-run download:** Pulling and extracting the three additional observability
> images can take several minutes, depending on network and disk speed. Docker
> reports approximate local image sizes of **445 MB** for OpenTelemetry Collector,
> **2.55 GB** for Elasticsearch, and **2.51 GB** for Kibana; compressed download sizes
> differ. Subsequent runs reuse cached images. The Kibana setup service reuses the
> same Kibana image rather than requiring a fourth download.

Local endpoints:

- [Health check](http://localhost:8090/ping)
- [Swagger UI](http://localhost:8090/swagger/index.html)
- [Payment Gateway Dashboard](http://localhost:5601/app/dashboards#/view/payment-gateway-dashboard) (observability mode)

To stop the selected stack, replace `up --build -d` with `down` in its command.

### Manual API Testing

Start the services using [Running in Docker](#running-in-docker), then use
`curl`, Insomnia, or another HTTP client against `http://localhost:8090`.
Use only synthetic card data and the supplied development API keys.

#### API keys and request headers

Valid development API keys are documented in the comments under
`authentication.api_keys` in [config.development.yaml](config.development.yaml),
alongside their merchant IDs and scopes. Send the actual API key, **not**
`key_hash`. The examples use the full-access `sk_development1` API key.
Keep the workflow under the same merchant: another merchant's API key cannot
use its card tokens or retrieve its intents, even with the required scopes.

- **Authorization:** `Bearer <API key>` on all four endpoints below.
- **Content-Type:** `application/json` for POST request bodies.
- **X-Idempotency-Key:** required only for intent creation and confirmation.
  Supply exactly one lowercase, hyphenated UUID, such as
  `01234567-89ab-4cde-8123-456789abcdef`. Generate a value in PowerShell with
  `[guid]::NewGuid().ToString()`.

| Request | Required scope | Idempotency key |
|---------|----------------|-----------------|
| `POST /api/CardTokens` | `card-tokens:create` | Not required; each call issues a new token |
| `POST /api/PaymentIntents` | `payment-intents:create` | Required |
| `POST /api/PaymentIntents/{id}/confirm` | `payment-intents:confirm` | Required |
| `GET /api/PaymentIntents/{id}` | `payment-intents:read` | Not required |

Use a fresh idempotency key for each new creation or confirmation operation.
To replay an operation, keep its key and request details unchanged; changed details
under an existing key return 409. Creation and confirmation have separate key
namespaces, although using distinct values makes manual testing easier.

### Supported currencies

Only three currencies are supported: `GBP` (British pound), `USD` (US dollar),
and `EUR` (euro). The `currency` field must use these exact uppercase codes;
unsupported or lowercase values return `400 Bad Request`.

### API Reference

For detailed endpoint descriptions, request and response schemas, and interactive
API testing, see the [Swagger UI](http://localhost:8090/swagger/index.html),
available after starting the gateway.

### Successful Payment Authorization with cURL

This walkthrough pays **GBP 1.00** (`amount: 100`) using a synthetic card ending in
`7`, which the bank simulator authorizes. Run the snippets in order in the same
**PowerShell 7** session, with the gateway and bank simulator running.

`curl.exe --verbose` shows outgoing request headers and incoming response headers;
the commands specify the request bodies and print the captured response bodies.
`--fail-with-body` and the exit-code checks stop the walkthrough on HTTP errors.
This output contains the public development API key and synthetic card details:
do not use real credentials or card data in a recorded walkthrough.

```powershell
$baseUrl = 'http://localhost:8090'
$apiKey = 'sk_development1'
$createKey = [guid]::NewGuid().ToString()
$confirmKey = [guid]::NewGuid().ToString()
```

The exchanges below were captured from the local Docker stack. Connection
diagnostics are omitted and JSON is formatted for readability; `Content-Length`
reflects the original compact JSON. Your IDs and timestamps will differ. Use the
variables populated by the commands, not the captured IDs, and confirm within the
card token's 15-minute lifetime.

#### 1. Tokenize the card

The client exchanges card details for a merchant-owned, single-use token. This
does not submit a payment to the bank.

```powershell
$tokenBody = curl.exe --silent --show-error --fail-with-body --verbose `
  "$baseUrl/api/CardTokens" `
  --header "Authorization: Bearer $apiKey" `
  --header 'Content-Type: application/json' `
  --data-raw '{"card_number":"2222405343240007","expiry_month":12,"expiry_year":2099,"cvv":"012"}'
if ($LASTEXITCODE -ne 0) { throw 'Card tokenization failed.' }
$tokenBody
$cardToken = ($tokenBody | ConvertFrom-Json).token
if (-not $cardToken) { throw 'Token response is missing its token.' }
```

**Captured request:**

```http
POST /api/CardTokens HTTP/1.1
Host: localhost:8090
User-Agent: curl/8.21.0
Accept: */*
Authorization: Bearer sk_development1
Content-Type: application/json
Content-Length: 83

{
  "card_number": "2222405343240007",
  "expiry_month": 12,
  "expiry_year": 2099,
  "cvv": "012"
}
```

**Captured response:**

```http
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json
Pragma: no-cache
X-Request-Id: 01a20703-db8d-7840-8009-f9e51212a62c
Date: Mon, 23 Nov 2026 06:07:03 GMT
Content-Length: 146

{
  "token": "01a20703-db8d-7c81-9a6a-95c0278cc8f1",
  "expires_at": "2026-11-23T06:22:03.053818058Z",
  "request_id": "01a20703-db8d-7840-8009-f9e51212a62c"
}
```

#### 2. Create the payment intent

The merchant backend supplies the currency, amount in minor units, and a fresh
creation idempotency key. The intent starts as `Created`, with no bank attempts.

```powershell
$intentBody = curl.exe --silent --show-error --fail-with-body --verbose `
  "$baseUrl/api/PaymentIntents" `
  --header "Authorization: Bearer $apiKey" `
  --header "X-Idempotency-Key: $createKey" `
  --header 'Content-Type: application/json' `
  --data-raw '{"currency":"GBP","amount":100}'
if ($LASTEXITCODE -ne 0) { throw 'Payment intent creation failed.' }
$intentBody
$intentId = ($intentBody | ConvertFrom-Json).payment_intent_id
if (-not $intentId) { throw 'Intent response is missing its ID.' }
```

**Captured request:**

```http
POST /api/PaymentIntents HTTP/1.1
Host: localhost:8090
User-Agent: curl/8.21.0
Accept: */*
Authorization: Bearer sk_development1
X-Idempotency-Key: fd1477c6-1bce-43b0-8c1e-366785d3c200
Content-Type: application/json
Content-Length: 31

{
  "currency": "GBP",
  "amount": 100
}
```

**Captured response:**

```http
HTTP/1.1 201 Created
Cache-Control: no-store
Content-Type: application/json
Location: /api/PaymentIntents/01a20703-dc9c-7416-8d35-d5b79916aa7f
Pragma: no-cache
X-Request-Id: 01a20703-dc9c-7152-bac0-3f10eb799002
Date: Mon, 23 Nov 2026 06:07:03 GMT
Content-Length: 274

{
  "payment_intent_id": "01a20703-dc9c-7416-8d35-d5b79916aa7f",
  "status": "Created",
  "currency": "GBP",
  "amount": 100,
  "created_at": "2026-11-23T06:07:03.32426856Z",
  "updated_at": "2026-11-23T06:07:03.32426856Z",
  "bank_transactions": [],
  "request_id": "01a20703-dc9c-7152-bac0-3f10eb799002"
}
```

The `Location` header identifies the retrieval endpoint for this intent.

#### 3. Confirm and authorize the payment

The merchant backend submits the token and a separate confirmation idempotency
key. The gateway consumes the token, calls the bank simulator, and records its
authorization before returning the intent and transaction history.

```powershell
$confirmationJson = @{ card_token = $cardToken } | ConvertTo-Json -Compress
Write-Output "Request body: $confirmationJson"
$confirmedBody = curl.exe --silent --show-error --fail-with-body --verbose `
  "$baseUrl/api/PaymentIntents/$intentId/confirm" `
  --header "Authorization: Bearer $apiKey" `
  --header "X-Idempotency-Key: $confirmKey" `
  --header 'Content-Type: application/json' `
  --data-raw $confirmationJson
if ($LASTEXITCODE -ne 0) { throw 'Payment confirmation failed.' }
$confirmedBody
$confirmed = $confirmedBody | ConvertFrom-Json
if ($confirmed.status -ne 'Authorized') {
  throw "Expected Authorized, received $($confirmed.status)."
}
```

**Captured request:**

```http
POST /api/PaymentIntents/01a20703-dc9c-7416-8d35-d5b79916aa7f/confirm HTTP/1.1
Host: localhost:8090
User-Agent: curl/8.21.0
Accept: */*
Authorization: Bearer sk_development1
X-Idempotency-Key: 9464e1dd-d8b4-4dbb-b79d-1f5dca803ab8
Content-Type: application/json
Content-Length: 53

{
  "card_token": "01a20703-db8d-7c81-9a6a-95c0278cc8f1"
}
```

**Captured response:**

```http
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json
Pragma: no-cache
X-Request-Id: 01a20703-ddb3-7532-ba5b-96598bf52047
Date: Mon, 23 Nov 2026 06:07:03 GMT
Content-Length: 522

{
  "payment_intent_id": "01a20703-dc9c-7416-8d35-d5b79916aa7f",
  "status": "Authorized",
  "currency": "GBP",
  "amount": 100,
  "created_at": "2026-11-23T06:07:03.32426856Z",
  "updated_at": "2026-11-23T06:07:03.61232233Z",
  "bank_transactions": [
    {
      "bank_transaction_id": "01a20703-ddb3-7850-8bc5-948148be3e5f",
      "status": "Authorized",
      "card_number_last_four": "0007",
      "expiry_month": 12,
      "expiry_year": 2099,
      "created_at": "2026-11-23T06:07:03.603545518Z",
      "completed_at": "2026-11-23T06:07:03.61232233Z"
    }
  ],
  "request_id": "01a20703-ddb3-7532-ba5b-96598bf52047"
}
```

Both the intent and its attempt are `Authorized`. The response includes every
required payment field: ID, status, currency, amount, and the last four card digits
and expiry details nested inside `bank_transactions`. It contains neither the full
card number nor the CVV.

#### 4. Retrieve the authorized payment

Use the payment-intent ID to retrieve the persisted outcome. This is a GET request
with no request body or idempotency key, and it does not call the bank again.

```powershell
$retrievedBody = curl.exe --silent --show-error --fail-with-body --verbose `
  "$baseUrl/api/PaymentIntents/$intentId" `
  --header "Authorization: Bearer $apiKey"
if ($LASTEXITCODE -ne 0) { throw 'Payment retrieval failed.' }
$retrievedBody
```

**Captured request (no body):**

```http
GET /api/PaymentIntents/01a20703-dc9c-7416-8d35-d5b79916aa7f HTTP/1.1
Host: localhost:8090
User-Agent: curl/8.21.0
Accept: */*
Authorization: Bearer sk_development1
```

**Captured response:**

```http
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json
Pragma: no-cache
X-Request-Id: 01a20703-deb1-752a-8d49-f76e0a315e71
Date: Mon, 23 Nov 2026 06:07:03 GMT
Content-Length: 522

{
  "payment_intent_id": "01a20703-dc9c-7416-8d35-d5b79916aa7f",
  "status": "Authorized",
  "currency": "GBP",
  "amount": 100,
  "created_at": "2026-11-23T06:07:03.32426856Z",
  "updated_at": "2026-11-23T06:07:03.61232233Z",
  "bank_transactions": [
    {
      "bank_transaction_id": "01a20703-ddb3-7850-8bc5-948148be3e5f",
      "status": "Authorized",
      "card_number_last_four": "0007",
      "expiry_month": 12,
      "expiry_year": 2099,
      "created_at": "2026-11-23T06:07:03.603545518Z",
      "completed_at": "2026-11-23T06:07:03.61232233Z"
    }
  ],
  "request_id": "01a20703-deb1-752a-8d49-f76e0a315e71"
}
```

The intent ID, transaction ID, payment details, and timestamps match the
confirmation response. Only `request_id` changes, identifying this separate HTTP
request; it also matches the response's `X-Request-Id` header.

## 13. Known Limitations and Production Evolution

The implementation establishes useful payment semantics without claiming
production-grade distributed guarantees. The main next steps would be:

1. **Durable storage and change capture:** use a transactional database with native
   CDC for durable state and change capture instead of in-memory storage and queues.
   Consider merchant-aware sharding as load grows to distribute storage and
   concurrent writes. The shard design must preserve merchant-scoped idempotency
   and uniqueness/transaction guarantees. Retain fingerprint-key material across
   restarts and define event delivery, replay, and consumer-recovery semantics.
   Use indexed queries and retention policies to keep data access efficient and
   storage growth bounded.
2. **Authoritative bank reconciliation:** integrate bank lookup APIs or signed
   bank-transaction webhooks to resolve uncertain outcomes. Define safe
   reconciliation transitions and coordinate upstream idempotency before
   considering automatic payment resubmission.
3. **Stronger card-data isolation:** use an appropriate tokenization/vault design
   and eliminate retention that is not justified or permitted.
4. **Resilience and admission control:** introduce distributed rate limits,
   per-merchant fairness, bounded concurrency, circuit breakers, and backpressure.
   Apply retries only where operations are safe to repeat; uncertain payments must
   not be blindly resubmitted.
5. **Production operations:** add readiness policy, security hardening, durable
   audit requirements, alerting/SLOs, and broader trace instrumentation. Introduce
   CI/CD pipelines, for example with GitHub Actions, to automate testing, security
   checks, container builds, and controlled deployments with approval and rollback
   procedures.

These are deliberate evolution points, not guarantees supplied by the current
in-memory assessment implementation.
