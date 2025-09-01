package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/web"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

const (
	addr = "localhost:3000"
)

func main() {
	provider := web.NewProvider(addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("Listening on http://%s", addr)

	messages, err := provider.Listen(ctx)
	if err != nil {
		log.Fatalf("[ERROR] %+v", errors.WithStack(err))
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	log.Println("Ctrl+C to exit")

	serverUser := courier.NewUser("server", "Server")

	for {
		select {
		case m, ok := <-messages:
			if !ok {
				return
			}

			mainContent, err := courier.GetMessageMainContent(m)
			if err != nil {
				log.Printf("[ERROR] %+v", errors.WithStack(err))
				continue
			}

			messageID := courier.MessageID(xid.New().String())

			text := fmt.Sprintf("You've just sent: '%s'", mainContent)

			response := courier.NewMessage(messageID, m.ChannelID(), serverUser, courier.WithMessageMainPart(text))

			if err := provider.Send(ctx, response); err != nil {
				log.Printf("[ERROR] %+v", errors.WithStack(err))
				continue
			}

			log.Printf("[MESSAGE] %s: %s", m.From().DisplayName(), mainContent)

		case <-sig:
			return
		}
	}
}
