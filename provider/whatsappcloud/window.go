package whatsappcloud

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// ErrWindowClosed is returned by Send when the customer service window with
// the recipient is closed and no template could stand in for the message.
var ErrWindowClosed = errors.New("whatsapp customer service window is closed")

// The customer service window.
//
// WhatsApp only accepts free-form messages to a user who wrote in the last
// 24 hours. Past that, only approved templates go through, and they are
// billed. An assistant runs into it all the time: a reminder, a scheduled
// report or the result of a long task usually leaves long after the last
// exchange.
//
// When the window is closed, the provider holds the message back in an
// outbox, sends the template chosen by ReengageFunc instead, and delivers
// what it held back as soon as the user writes again — which reopens the
// window. One template covers everything held back for a user: a second
// reminder does not send a second template.
//
// The window is known in two ways. The provider remembers when each user
// last wrote (WindowStore); and WhatsApp, which often accepts the message
// first and reports the failure later through a status notification, has
// the last word: a message it refuses that way goes through the same path.

// WindowStore remembers when each user last wrote.
type WindowStore interface {
	// LastInbound returns when the user last wrote, or the zero time when
	// it is not known.
	LastInbound(ctx context.Context, userID courier.UserID) (time.Time, error)
	// RecordInbound notes that the user wrote at the given time.
	RecordInbound(ctx context.Context, userID courier.UserID, at time.Time) error
}

// MemoryWindowStore keeps the windows in memory.
type MemoryWindowStore struct {
	mu   sync.Mutex
	last map[courier.UserID]time.Time
}

func NewMemoryWindowStore() *MemoryWindowStore {
	return &MemoryWindowStore{last: map[courier.UserID]time.Time{}}
}

// LastInbound implements WindowStore.
func (s *MemoryWindowStore) LastInbound(_ context.Context, userID courier.UserID) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[userID], nil
}

// RecordInbound implements WindowStore.
func (s *MemoryWindowStore) RecordInbound(_ context.Context, userID courier.UserID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.last[userID]) {
		s.last[userID] = at
	}
	return nil
}

var _ WindowStore = &MemoryWindowStore{}

type held struct {
	message  courier.Message
	queuedAt time.Time
}

// outbox holds back the messages waiting for a window to reopen.
type outbox struct {
	mu      sync.Mutex
	pending map[courier.ChannelID][]held
}

// push holds message back and reports whether it is the first one waiting
// for this recipient, that is whether a template must be sent.
func (o *outbox) push(recipient courier.ChannelID, message courier.Message, now time.Time, ttl time.Duration, size int) (first bool, dropped int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.pending == nil {
		o.pending = map[courier.ChannelID][]held{}
	}

	queue := fresh(o.pending[recipient], now, ttl)
	first = len(queue) == 0

	queue = append(queue, held{message: message, queuedAt: now})
	if size > 0 && len(queue) > size {
		dropped = len(queue) - size
		queue = queue[dropped:]
	}

	o.pending[recipient] = queue

	return first, dropped
}

// take removes and returns what is held back for recipient, oldest first.
func (o *outbox) take(recipient courier.ChannelID, now time.Time, ttl time.Duration) []courier.Message {
	o.mu.Lock()
	queue := fresh(o.pending[recipient], now, ttl)
	delete(o.pending, recipient)
	o.mu.Unlock()

	messages := make([]courier.Message, 0, len(queue))
	for _, h := range queue {
		messages = append(messages, h.message)
	}

	return messages
}

// fresh drops the entries older than ttl.
func fresh(queue []held, now time.Time, ttl time.Duration) []held {
	if ttl <= 0 {
		return queue
	}

	kept := queue[:0]
	for _, h := range queue {
		if now.Sub(h.queuedAt) <= ttl {
			kept = append(kept, h)
		}
	}

	return kept
}

// previewLength bounds the preview handed to ReengageFunc, well under the
// size WhatsApp accepts for a template parameter.
const previewLength = 500

// previewOf flattens text into a single line fit for a template parameter:
// WhatsApp rejects parameters holding new lines, tabs or more than four
// consecutive spaces.
func previewOf(text string) string {
	fields := strings.FieldsFunc(text, unicode.IsSpace)
	flat := strings.Join(fields, " ")

	runes := []rune(flat)
	if len(runes) > previewLength {
		flat = strings.TrimRightFunc(string(runes[:previewLength-1]), unicode.IsSpace) + "…"
	}

	return flat
}
