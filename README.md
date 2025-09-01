# GoCourier

A simple and unified interface to send and receive messages from/to common chat platforms.

## Usage

```go
// Create a instance of your provider
provider := provider.NewProvider(/** ... **/)

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

// Listen for incoming messages
messages, err := provider.Listen(ctx)
if err != nil {
  log.Fatalf("[ERROR] %+v", errors.WithStack(err))
  return
}

// Create a "user" representing your bot
courierUser := courier.NewUser("gocourier", "GoCourier")

// Read incoming messages
for {
  m, ok := <-messages:
  if !ok {
    return
  }

  // Retrieve the "main" content of the message (i.e. the text message)
  // Attachments can be retrieved too
  mainContent, err := courier.GetMessageMainContent(m)
  if err != nil {
    log.Printf("[ERROR] %+v", errors.WithStack(err))
    continue
  }

  log.Printf("[MESSAGE] %s: %s", m.From().DisplayName(), mainContent)

  // Generate a new message id to send a response to the incoming message
  messageID := courier.RandomMessageID()

  text := fmt.Sprintf("You've just sent: '%s'", mainContent)

  // Create your response
  response := courier.NewMessage(
    messageID,
    m.ChannelID(),
    courierUser,
    courier.WithMessageMainPart(text),
  )

  if err := provider.Send(context.TODO(), response); err != nil {
    log.Printf("[ERROR] %+v", errors.WithStack(err))
    continue
  }
}
```

See [`./examples`](./examples) for more examples.
