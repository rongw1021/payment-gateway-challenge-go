package changedatacapture

import (
	"context"
	"sync"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

type ChangeDataCapture[T any] interface {
	Produce(T) error
	Consume(context.Context) (T, error)
}

type Streams struct {
	BankTransactions ChangeDataCapture[models.BankTransactionChangeEvent]
	PaymentIntents   ChangeDataCapture[models.PaymentIntentChangeEvent]
}

func NewStreams() Streams {
	return Streams{
		BankTransactions: NewInMemory[models.BankTransactionChangeEvent](),
		PaymentIntents:   NewInMemory[models.PaymentIntentChangeEvent](),
	}
}

type sequenced[T any] interface {
	WithSequence(int64) T
}

type InMemory[T sequenced[T]] struct {
	mu       sync.Mutex
	changes  []T
	sequence int64
	ready    chan struct{}
}

func NewInMemory[T sequenced[T]]() *InMemory[T] {
	return &InMemory[T]{ready: make(chan struct{})}
}

func (c *InMemory[T]) Produce(change T) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequence++
	change = change.WithSequence(c.sequence)
	c.changes = append(c.changes, change)
	close(c.ready)
	c.ready = make(chan struct{})
	return nil
}

func (c *InMemory[T]) Consume(ctx context.Context) (T, error) {
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		c.mu.Lock()
		if len(c.changes) > 0 {
			change := c.changes[0]
			c.changes[0] = zero
			c.changes = c.changes[1:]
			if len(c.changes) == 0 {
				c.changes = nil
			}
			c.mu.Unlock()
			return change, nil
		}
		ready := c.ready
		c.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}
