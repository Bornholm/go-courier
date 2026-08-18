package rest

import (
	"sync"

	"github.com/bornholm/go-courier"
)

// hub fans outgoing messages out to the SSE subscribers of a channel, and
// keeps a bounded history so that a client reconnecting with a Last-Event-ID
// can catch up on what it missed.
type hub struct {
	historySize int
	bufferSize  int

	mutex    sync.RWMutex
	channels map[courier.ChannelID]*channelHub
}

type channelHub struct {
	subscribers map[*subscriber]struct{}
	history     []courier.Message
}

type subscriber struct {
	messages chan courier.Message
	// dropped records that the subscriber fell too far behind and was
	// disconnected rather than allowed to block the whole hub.
	dropped bool
}

func newHub(historySize, bufferSize int) *hub {
	return &hub{
		historySize: historySize,
		bufferSize:  bufferSize,
		channels:    map[courier.ChannelID]*channelHub{},
	}
}

// publish records the message in the channel history and hands it to every
// subscriber. Subscribers whose queue is full are disconnected: a stalled
// client must not stall the provider.
func (h *hub) publish(channelID courier.ChannelID, message courier.Message) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	channel := h.channelHub(channelID)

	if h.historySize > 0 {
		channel.history = append(channel.history, message)
		if len(channel.history) > h.historySize {
			channel.history = channel.history[len(channel.history)-h.historySize:]
		}
	}

	for sub := range channel.subscribers {
		select {
		case sub.messages <- message:
		default:
			sub.dropped = true
			delete(channel.subscribers, sub)
			close(sub.messages)
		}
	}
}

// subscribe registers a new subscriber and returns the backlog it should
// receive first. When lastEventID is known, the backlog holds every message
// recorded after it; otherwise it is empty.
func (h *hub) subscribe(channelID courier.ChannelID, lastEventID courier.MessageID) (*subscriber, []courier.Message) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	channel := h.channelHub(channelID)

	sub := &subscriber{
		messages: make(chan courier.Message, h.bufferSize),
	}

	channel.subscribers[sub] = struct{}{}

	return sub, backlogAfter(channel.history, lastEventID)
}

func (h *hub) unsubscribe(channelID courier.ChannelID, sub *subscriber) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	channel, exists := h.channels[channelID]
	if !exists {
		return
	}

	if _, subscribed := channel.subscribers[sub]; !subscribed {
		// Already removed by publish after falling behind, the channel is
		// closed there.
		return
	}

	delete(channel.subscribers, sub)
	close(sub.messages)
}

// close disconnects every subscriber of every channel.
func (h *hub) close() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	for _, channel := range h.channels {
		for sub := range channel.subscribers {
			delete(channel.subscribers, sub)
			close(sub.messages)
		}
	}
}

// channelHub returns the hub of a channel, creating it if needed. Callers must
// hold the lock.
func (h *hub) channelHub(channelID courier.ChannelID) *channelHub {
	channel, exists := h.channels[channelID]
	if exists {
		return channel
	}

	channel = &channelHub{
		subscribers: map[*subscriber]struct{}{},
		history:     []courier.Message{},
	}

	h.channels[channelID] = channel

	return channel
}

// backlogAfter returns the messages recorded after lastEventID. An unknown or
// empty identifier yields no backlog: replaying the whole history to a fresh
// client would duplicate what it may already have.
func backlogAfter(history []courier.Message, lastEventID courier.MessageID) []courier.Message {
	if lastEventID == "" {
		return nil
	}

	for idx, message := range history {
		if message.ID() != lastEventID {
			continue
		}

		backlog := make([]courier.Message, len(history)-idx-1)
		copy(backlog, history[idx+1:])

		return backlog
	}

	return nil
}
