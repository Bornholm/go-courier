package memory_test

import (
	"testing"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/couriertest"
	"github.com/bornholm/go-courier/provider/memory"
)

func TestProviderConformance(t *testing.T) {
	couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
		provider := memory.NewProvider()

		return &couriertest.Harness{
			Provider: provider,
			Deliver:  provider.Deliver,
			Sent:     provider.Sent,
			Channel:  courier.NewChannel("memory", courier.ChannelKindDirect, "memory"),
			From:     courier.NewUser("user-1", "User"),
			Cleanup:  func() { provider.Close() },
		}
	})
}

func TestProviderLoopback(t *testing.T) {
	ctx := t.Context()

	provider := memory.NewProvider(memory.WithLoopback(true))
	defer provider.Close()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatalf("provider.Listen(ctx): %+v", err)
	}

	sent := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("memory"),
		courier.NewUser("user-1", "User"),
		courier.WithMessageMainPart("echo"),
	)

	if err := provider.Send(ctx, sent); err != nil {
		t.Fatalf("provider.Send(ctx, sent): %+v", err)
	}

	received := <-messages

	content, err := courier.GetMessageMainContent(ctx, received)
	if err != nil {
		t.Fatalf("courier.GetMessageMainContent(ctx, received): %+v", err)
	}

	if content != "echo" {
		t.Errorf("main content: got %q, expected %q", content, "echo")
	}
}
