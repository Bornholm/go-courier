package whatsapp_test

import (
	"context"
	"testing"

	"github.com/bornholm/go-courier/provider/whatsapp"
)

// A nil handler must keep the historical behaviour: pairing codes go to
// standard output, which is what a terminal deployment expects.
func TestOptions_QRHandlerDefaultsToNil(t *testing.T) {
	if opts := whatsapp.NewOptions(); opts.QRHandler != nil {
		t.Error("no QR handler should be installed by default")
	}
}

// With a handler installed, the provider hands over both the refreshed
// pairing codes and the outcome, so a web UI can render and follow them.
func TestOptions_WithQRHandlerIsInstalled(t *testing.T) {
	var (
		codes  []string
		linked bool
	)

	opts := whatsapp.NewOptions(whatsapp.WithQRHandler(func(_ context.Context, code string, ok bool) {
		if code != "" {
			codes = append(codes, code)
			return
		}
		linked = ok
	}))

	if opts.QRHandler == nil {
		t.Fatal("QR handler not installed")
	}

	opts.QRHandler(context.Background(), "code-1", false)
	opts.QRHandler(context.Background(), "code-2", false)
	opts.QRHandler(context.Background(), "", true)

	if len(codes) != 2 || codes[1] != "code-2" {
		t.Errorf("unexpected codes: %v", codes)
	}
	if !linked {
		t.Error("the successful outcome should have been reported")
	}
}
