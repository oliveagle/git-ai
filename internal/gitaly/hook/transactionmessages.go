package hook

import (
	"context"
	"sync"
)

// TransactionMessages collects the messages a reference transaction wants the
// pusher to read. git gives the reference-transaction hook no side band, so
// the messages travel back in the response of the hook service, which writes
// them to stderr (internal/gitaly/service/hook). A warning is not a refusal:
// the transaction goes through, and the pusher is told what to repair.
type TransactionMessages struct {
	mu       sync.Mutex
	messages []string
}

// Add records a message for the pusher.
func (m *TransactionMessages) Add(message string) {
	if m == nil || message == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message)
}

// All returns the messages recorded so far, in the order they were added.
func (m *TransactionMessages) All() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.messages...)
}

// transactionMessagesKey is the context key of the collector.
type transactionMessagesKey struct{}

// WithTransactionMessages returns a context that collects the messages of the
// reference transaction into messages. The hook service settles it once per
// call; a caller that does not collect reads no message.
func WithTransactionMessages(ctx context.Context, messages *TransactionMessages) context.Context {
	return context.WithValue(ctx, transactionMessagesKey{}, messages)
}

// TransactionMessagesFrom returns the collector of ctx, nil when the caller
// does not collect the messages of the transaction.
func TransactionMessagesFrom(ctx context.Context) *TransactionMessages {
	messages, _ := ctx.Value(transactionMessagesKey{}).(*TransactionMessages)
	return messages
}
