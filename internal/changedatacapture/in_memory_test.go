package changedatacapture

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryConcurrentPublishersPreserveSequenceAndEvents(t *testing.T) {
	capture := NewInMemory[models.BankTransactionChangeEvent]()
	var publishers sync.WaitGroup
	for i := 0; i < 32; i++ {
		publishers.Add(1)
		go func(id int) {
			defer publishers.Done()
			assert.NoError(t, capture.Produce(models.BankTransactionChangeEvent{
				EventType: models.BankTransactionCreated, BankTransactionID: fmt.Sprint(id), RequestID: "original-request",
			}))
		}(i)
	}
	publishers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ids := make(map[string]bool)
	for sequence := int64(1); sequence <= 32; sequence++ {
		event, err := capture.Consume(ctx)
		require.NoError(t, err)
		assert.Equal(t, sequence, event.Sequence)
		assert.Equal(t, "original-request", event.RequestID)
		assert.False(t, ids[event.BankTransactionID])
		ids[event.BankTransactionID] = true
	}
	assert.Len(t, ids, 32)
}

func TestInMemoryConsumptionIsCancelableWithoutDiscardingQueuedEvents(t *testing.T) {
	capture := NewInMemory[models.BankTransactionChangeEvent]()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := capture.Consume(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, capture.Produce(models.BankTransactionChangeEvent{BankTransactionID: "pending"}))
	_, err = capture.Consume(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	event, err := capture.Consume(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "pending", event.BankTransactionID)
	assert.EqualValues(t, 1, event.Sequence)
}

func TestStreamsKeepEventTypesAndSequencesIndependent(t *testing.T) {
	streams := NewStreams()
	for sequence := int64(1); sequence <= 2; sequence++ {
		require.NoError(t, streams.BankTransactions.Produce(models.BankTransactionChangeEvent{
			BankTransactionID: "transaction", EventType: models.BankTransactionCreated,
		}))
		require.NoError(t, streams.PaymentIntents.Produce(models.PaymentIntentChangeEvent{
			PaymentIntentID: "intent", EventType: models.PaymentIntentCreatedEvent,
		}))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		bank, err := streams.BankTransactions.Consume(ctx)
		require.NoError(t, err)
		assert.Equal(t, "transaction", bank.BankTransactionID)
		assert.Equal(t, sequence, bank.Sequence)
		intent, err := streams.PaymentIntents.Consume(ctx)
		require.NoError(t, err)
		assert.Equal(t, "intent", intent.PaymentIntentID)
		assert.Equal(t, sequence, intent.Sequence)
		cancel()
	}
}

type waitingContext struct {
	context.Context
	waiting chan struct{}
}

func (c waitingContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestInMemoryPublicationWakesConsumerAfterEachDrain(t *testing.T) {
	capture := NewInMemory[models.BankTransactionChangeEvent]()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		event models.BankTransactionChangeEvent
		err   error
	}
	for sequence := int64(1); sequence <= 2; sequence++ {
		waiting := make(chan struct{}, 1)
		done := make(chan result, 1)
		go func() {
			event, err := capture.Consume(waitingContext{Context: ctx, waiting: waiting})
			done <- result{event: event, err: err}
		}()
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatal("consumer did not wait for publication")
		}
		require.NoError(t, capture.Produce(models.BankTransactionChangeEvent{BankTransactionID: fmt.Sprint(sequence)}))
		select {
		case outcome := <-done:
			require.NoError(t, outcome.err)
			assert.Equal(t, sequence, outcome.event.Sequence)
			assert.Equal(t, fmt.Sprint(sequence), outcome.event.BankTransactionID)
		case <-ctx.Done():
			t.Fatal("publication did not wake the waiting consumer")
		}
	}
}
