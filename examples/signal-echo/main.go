// signal-echo répète tout message reçu sur Signal, via un daemon signal-cli.
//
// Prérequis :
//
//	signal-cli -a +33600000000 daemon --tcp 127.0.0.1:7583
//
// Puis :
//
//	SIGNAL_ACCOUNT=+33600000000 go run ./examples/signal-echo
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bornholm/go-courier"
	signalprovider "github.com/bornholm/go-courier/provider/signal"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	address := os.Getenv("SIGNAL_ADDRESS")
	if address == "" {
		address = "tcp://127.0.0.1:7583"
	}

	provider := signalprovider.NewProvider(
		signalprovider.WithAddress(address),
		signalprovider.WithAccount(os.Getenv("SIGNAL_ACCOUNT")),
	)

	messages, err := provider.Listen(ctx)
	if err != nil {
		log.Fatalf("%+v", err)
	}

	log.Printf("en écoute sur %s", address)

	for message := range messages {
		content, err := courier.GetMessageMainContent(ctx, message)
		if err != nil {
			continue
		}

		log.Printf("reçu de %s sur %s", message.From().DisplayName(), message.Channel().ChannelID())

		reply := courier.NewMessage(
			courier.RandomMessageID(),
			message.Channel(),
			courier.NewUser("echo", "Echo"),
			courier.WithMessageMainPart("echo: "+content),
		)

		if err := provider.Send(ctx, reply); err != nil {
			log.Printf("échec de l'envoi: %+v", err)
		}
	}
}
