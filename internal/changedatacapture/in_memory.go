package changedatacapture

import (
	"context"
	"sync"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

type ChangeDataCapture interface {
	Produce(models.BankTransactionChangeEvent) error
	Consume(context.Context) (models.BankTransactionChangeEvent, error)
}

type InMemory struct {
	mu       sync.Mutex
	changes  []models.BankTransactionChangeEvent
	sequence int64
	ready    chan struct{}
}

func NewInMemory() *InMemory {
	return &InMemory{ready: make(chan struct{})}
}

func (c *InMemory) Produce(change models.BankTransactionChangeEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequence++
	change.Sequence = c.sequence
	c.changes = append(c.changes, change)
	close(c.ready)
	c.ready = make(chan struct{})
	return nil
}

func (c *InMemory) Consume(ctx context.Context) (models.BankTransactionChangeEvent, error) {
	for {
		if err := ctx.Err(); err != nil {
			return models.BankTransactionChangeEvent{}, err
		}
		c.mu.Lock()
		if len(c.changes) > 0 {
			change := c.changes[0]
			c.changes[0] = models.BankTransactionChangeEvent{}
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
			return models.BankTransactionChangeEvent{}, ctx.Err()
		}
	}
}
