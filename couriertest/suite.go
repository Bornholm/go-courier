// Package couriertest provides a conformance suite that any courier.Provider
// implementation can run against, checking that it honours the contract of
// the courier interfaces.
package couriertest

import (
	"context"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
)

// DefaultTimeout bounds how long the suite waits for a message to travel
// through a provider.
const DefaultTimeout = 5 * time.Second

// Harness adapts a provider to the conformance suite. Providers talking to a
// real platform cannot be driven from a test, which is why injecting incoming
// messages and observing outgoing ones are supplied by the caller rather than
// assumed.
type Harness struct {
	// Provider under test.
	Provider courier.Provider

	// Deliver simulates an incoming message, as if it came from the platform.
	Deliver func(ctx context.Context, message courier.Message) error

	// Listen returns the channel carrying incoming messages. Optional,
	// defaults to Provider.Listen. Providers accepting a single Listen call,
	// such as ones owning a server socket, supply the channel they already
	// opened here.
	Listen func(ctx context.Context) (chan courier.Message, error)

	// Sent returns the messages the provider actually handed to the platform,
	// in order. Optional: send assertions are skipped when nil.
	Sent func() []courier.Message

	// Channel the suite sends to and receives from.
	Channel courier.Channel

	// From is the user incoming messages are attributed to.
	From courier.User

	// Timeout bounds each wait. Defaults to DefaultTimeout.
	Timeout time.Duration

	// Cleanup is called when the test ends. Optional.
	Cleanup func()
}

func (h *Harness) timeout() time.Duration {
	if h.Timeout <= 0 {
		return DefaultTimeout
	}

	return h.Timeout
}

// RunProviderSuite runs the whole conformance suite. newHarness is called once
// per sub test so that each starts from a clean provider.
func RunProviderSuite(t *testing.T, newHarness func(t *testing.T) *Harness) {
	t.Helper()

	for name, run := range map[string]func(t *testing.T, h *Harness){
		"TextRoundTrip":      testTextRoundTrip,
		"PartsAreReplayable": testPartsAreReplayable,
		"Attachments":        testAttachments,
		"ChannelKind":        testChannelKind,
		"Mentions":           testMentions,
		"Threads":            testThreads,
		"Send":               testSend,
		"SendAttachments":    testSendAttachments,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newHarness(t)

			if harness.Cleanup != nil {
				t.Cleanup(harness.Cleanup)
			}

			run(t, harness)
		})
	}
}

// listen starts listening and returns the channel, cancelling on test end.
func listen(t *testing.T, h *Harness) (context.Context, chan courier.Message) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := h.Listen
	if start == nil {
		start = h.Provider.Listen
	}

	messages, err := start(ctx)
	if err != nil {
		t.Fatalf("Listen(ctx): %+v", err)
	}

	return ctx, messages
}

// waitFor waits for the next message, failing the test on timeout.
func waitFor(t *testing.T, h *Harness, messages chan courier.Message) courier.Message {
	t.Helper()

	select {
	case message, ok := <-messages:
		if !ok {
			t.Fatal("listen channel closed before a message was received")
		}

		return message
	case <-time.After(h.timeout()):
		t.Fatalf("no message received after %s", h.timeout())
		return nil
	}
}

// deliver injects an incoming message. Delivery runs in the background
// because a provider may well block until the message is consumed, which only
// happens once the caller reads the listen channel. Pass the returned channel
// to checkDelivered once the message has been received.
func deliver(t *testing.T, ctx context.Context, h *Harness, message courier.Message) chan error {
	t.Helper()

	if h.Deliver == nil {
		t.Skip("harness cannot deliver incoming messages")
	}

	errs := make(chan error, 1)

	go func() {
		errs <- h.Deliver(ctx, message)
	}()

	return errs
}

// checkDelivered fails the test if delivery reported an error.
func checkDelivered(t *testing.T, h *Harness, errs chan error) {
	t.Helper()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("Harness.Deliver(ctx, message): %+v", err)
		}
	case <-time.After(h.timeout()):
		t.Fatalf("delivery did not complete after %s", h.timeout())
	}
}

func testTextRoundTrip(t *testing.T, h *Harness) {
	ctx, messages := listen(t, h)

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("hello"),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	content, err := courier.GetMessageMainContent(ctx, received)
	if err != nil {
		t.Fatalf("courier.GetMessageMainContent(ctx, received): %+v", err)
	}

	if content != "hello" {
		t.Errorf("main content: got %q, expected %q", content, "hello")
	}

	if received.Channel() == nil {
		t.Fatal("received.Channel(): got nil, expected a channel")
	}

	if got := received.Channel().ChannelID(); got != h.Channel.ChannelID() {
		t.Errorf("received.Channel().ChannelID(): got %q, expected %q", got, h.Channel.ChannelID())
	}

	if received.From() == nil {
		t.Fatal("received.From(): got nil, expected a user")
	}

	if received.ID() == "" {
		t.Error("received.ID(): got empty, expected an identifier")
	}

	if received.SentAt().IsZero() {
		t.Error("received.SentAt(): got zero time, expected a timestamp")
	}
}

func testPartsAreReplayable(t *testing.T, h *Harness) {
	ctx, messages := listen(t, h)

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("replay me"),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	for _, part := range received.Parts() {
		first, err := courier.ReadPart(ctx, part)
		if err != nil {
			t.Fatalf("first read of part %q: %+v", part.Name(), err)
		}

		second, err := courier.ReadPart(ctx, part)
		if err != nil {
			t.Fatalf("second read of part %q: %+v", part.Name(), err)
		}

		if string(first) != string(second) {
			t.Errorf("part %q is not replayable: first read %d bytes, second read %d bytes",
				part.Name(), len(first), len(second))
		}
	}
}

func testAttachments(t *testing.T, h *Harness) {
	if !courier.HasCapability(h.Provider, courier.CapabilityReceiveAttachments) {
		t.Skip("provider does not declare CapabilityReceiveAttachments")
	}

	ctx, messages := listen(t, h)

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("listen to this"),
		courier.WithMessagePart(courier.NewAttachment(
			"note.ogg", "audio/ogg", courier.OpenerFromString("fake audio"),
			courier.WithAttachmentVoiceNote(4*time.Second),
		)),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	attachments := courier.Attachments(received)
	if len(attachments) != 1 {
		t.Fatalf("len(courier.Attachments(received)): got %d, expected 1", len(attachments))
	}

	attachment := attachments[0]

	if got := courier.MediaKindOf(attachment.ContentType()); got != courier.MediaKindAudio {
		t.Errorf("attachment media kind: got %q, expected %q", got, courier.MediaKindAudio)
	}

	if got := courier.FilenameFor(attachment); got != "note.ogg" {
		t.Errorf("courier.FilenameFor(attachment): got %q, expected %q", got, "note.ogg")
	}

	content, err := courier.ReadPart(ctx, attachment)
	if err != nil {
		t.Fatalf("courier.ReadPart(ctx, attachment): %+v", err)
	}

	if string(content) != "fake audio" {
		t.Errorf("attachment content: got %q, expected %q", string(content), "fake audio")
	}
}

func testChannelKind(t *testing.T, h *Harness) {
	if !courier.HasCapability(h.Provider, courier.CapabilityChannelKind) {
		t.Skip("provider does not declare CapabilityChannelKind")
	}

	ctx, messages := listen(t, h)

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("hello"),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	if got := received.Channel().Kind(); got == courier.ChannelKindUnknown {
		t.Error("received.Channel().Kind(): got ChannelKindUnknown, expected a known kind " +
			"since the provider declares CapabilityChannelKind")
	}
}

func testMentions(t *testing.T, h *Harness) {
	if !courier.HasCapability(h.Provider, courier.CapabilityMentions) {
		t.Skip("provider does not declare CapabilityMentions")
	}

	ctx, messages := listen(t, h)

	mentioned := courier.UserID("mentioned-user")

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("hey you"),
		courier.WithMessageMentions(courier.Mention{UserID: mentioned, DisplayName: "You"}),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	if !courier.IsMentioned(received, mentioned) {
		t.Errorf("courier.IsMentioned(received, %q): got false, expected true", mentioned)
	}
}

func testThreads(t *testing.T, h *Harness) {
	if !courier.HasCapability(h.Provider, courier.CapabilityThreads) {
		t.Skip("provider does not declare CapabilityThreads")
	}

	ctx, messages := listen(t, h)

	parent := courier.RandomMessageID()

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("answering"),
		courier.WithMessageInReplyTo(parent),
	)

	errs := deliver(t, ctx, h, sent)

	received := waitFor(t, h, messages)
	checkDelivered(t, h, errs)

	got, ok := courier.InReplyTo(received)
	if !ok {
		t.Fatal("courier.InReplyTo(received): got not ok, expected ok")
	}

	if got != parent {
		t.Errorf("courier.InReplyTo(received): got %q, expected %q", got, parent)
	}
}

func testSend(t *testing.T, h *Harness) {
	if h.Sent == nil {
		t.Skip("harness cannot observe outgoing messages")
	}

	ctx := context.Background()

	message := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("outgoing"),
	)

	if err := h.Provider.Send(ctx, message); err != nil {
		t.Fatalf("Provider.Send(ctx, message): %+v", err)
	}

	sent := h.Sent()
	if len(sent) != 1 {
		t.Fatalf("len(Harness.Sent()): got %d, expected 1", len(sent))
	}

	content, err := courier.GetMessageMainContent(ctx, sent[0])
	if err != nil {
		t.Fatalf("courier.GetMessageMainContent(ctx, sent[0]): %+v", err)
	}

	if content != "outgoing" {
		t.Errorf("sent main content: got %q, expected %q", content, "outgoing")
	}
}

func testSendAttachments(t *testing.T, h *Harness) {
	if h.Sent == nil {
		t.Skip("harness cannot observe outgoing messages")
	}

	if !courier.HasCapability(h.Provider, courier.CapabilitySendAttachments) {
		t.Skip("provider does not declare CapabilitySendAttachments")
	}

	ctx := context.Background()

	message := courier.NewMessage(
		courier.RandomMessageID(),
		h.Channel,
		h.From,
		courier.WithMessageMainPart("here is a file"),
		courier.WithMessagePart(courier.NewAttachment(
			"report.pdf", "application/pdf", courier.OpenerFromString("%PDF-1.4"),
		)),
	)

	if err := h.Provider.Send(ctx, message); err != nil {
		t.Fatalf("Provider.Send(ctx, message): %+v", err)
	}

	sent := h.Sent()
	if len(sent) == 0 {
		t.Fatal("len(Harness.Sent()): got 0, expected at least 1")
	}

	var found bool

	for _, message := range sent {
		for _, attachment := range courier.Attachments(message) {
			if courier.FilenameFor(attachment) == "report.pdf" {
				found = true
			}
		}
	}

	if !found {
		t.Error("no sent message carried the report.pdf attachment")
	}
}
