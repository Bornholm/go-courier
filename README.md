# GoCourier

A simple and unified interface to send and receive messages from/to common chat platforms.

## Providers

| Package | Platform | Attachments | Channel kind | Mentions |
|---|---|---|---|---|
| `provider/whatsapp` | WhatsApp | yes, voice notes included | yes | yes |
| `provider/mail` | SMTP/IMAP | yes | yes | no |
| `provider/rocket` | Rocket.Chat | yes | yes | yes |
| `provider/discord` | Discord | yes | yes | yes |
| `provider/rest` | JSON HTTP API | yes | configurable | yes |
| `provider/memory` | in-process, for tests | yes | configurable | yes |

Providers forward everything they receive, group conversations and media
included. Deciding whether to answer is up to the application.

## Usage

```go
// Create an instance of your provider
provider := whatsapp.NewProvider(whatsapp.WithDBPath("whatsapp.db"))

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

// Listen for incoming messages
messages, err := provider.Listen(ctx)
if err != nil {
  log.Fatalf("[ERROR] %+v", errors.WithStack(err))
}

// Find out who we are on this platform, to know when we are addressed
self, err := provider.Self(ctx)
if err != nil {
  log.Fatalf("[ERROR] %+v", errors.WithStack(err))
}

for m := range messages {
  channel := m.Channel()

  // In a group, only answer when explicitly mentioned
  if courier.IsGroupChannel(channel) && !courier.IsMentioned(m, self.ID()) {
    continue
  }

  // Retrieve the "main" content of the message (i.e. the text message)
  mainContent, err := courier.GetMessageMainContent(ctx, m)
  if err != nil {
    log.Printf("[ERROR] %+v", errors.WithStack(err))
    continue
  }

  log.Printf("[MESSAGE] %s: %s", m.From().DisplayName(), mainContent)

  // Create your response
  response := courier.NewMessage(
    courier.RandomMessageID(),
    channel,
    self,
    courier.WithMessageMainPart(fmt.Sprintf("You've just sent: '%s'", mainContent)),
  )

  if err := provider.Send(ctx, response); err != nil {
    log.Printf("[ERROR] %+v", errors.WithStack(err))
  }
}
```

## Attachments

A message is made of parts. The main part holds the text; every other part may
be an attachment — a picture, a document, a WhatsApp or Signal voice note, a
file joined to an email.

Attachment content is fetched lazily: it only crosses the network when the
application reads it, which is why `Reader` takes a context.

```go
for _, attachment := range courier.Attachments(message) {
  switch courier.MediaKindOf(attachment.ContentType()) {
  case courier.MediaKindAudio:
    if !courier.IsVoiceNote(attachment) {
      continue
    }

    // Reading is what actually downloads the media
    audio, err := courier.ReadPart(ctx, attachment)
    if err != nil {
      return errors.WithStack(err)
    }

    transcript, err := transcribe(ctx, audio)
    // ...

  case courier.MediaKindDocument:
    log.Printf("received %s", courier.FilenameFor(attachment))
  }
}
```

Parts are replayable: reading one twice yields the same content, so a media
can be both transcribed and archived.

To send an attachment, add it as a part:

```go
response := courier.NewMessage(
  courier.RandomMessageID(), channel, self,
  courier.WithMessageMainPart("here is the report"),
  courier.WithMessagePart(courier.NewAttachment(
    "report.pdf", "application/pdf", courier.OpenerFromFile("/tmp/report.pdf"),
  )),
)
```

## Channels

`Message.Channel()` reports whether the conversation is one to one or shared:

| Kind | Meaning |
|---|---|
| `ChannelKindDirect` | one to one conversation |
| `ChannelKindGroup` | closed group |
| `ChannelKindPublic` | open room, mailing list, broadcast, newsletter |
| `ChannelKindUnknown` | the provider cannot tell |

## Capabilities

Providers advertise what they support, so an application can adapt instead of
failing at runtime:

```go
if courier.HasCapability(provider, courier.CapabilitySendAttachments) {
  // ...
}
```

Optional interfaces follow the same idea: `courier.SelfProvider`,
`courier.ChannelResolver`, `courier.PresenceProvider`, `courier.StatusProvider`.

## REST provider

`provider/rest` exposes a courier provider over a JSON HTTP API, so any client
can drive it:

| Route | Role |
|---|---|
| `POST /channels/{channelID}/messages` | incoming message (multipart) |
| `GET /channels/{channelID}/events` | outgoing messages (SSE) |
| `GET /channels/{channelID}` | channel metadata |
| `GET /messages/{messageID}/parts/{partName}` | raw part content |
| `GET /healthz` | liveness probe |

```bash
go run ./examples/rest-echo

# stream the replies
curl -N -H 'Authorization: Bearer demo-token' \
     http://localhost:8080/channels/demo/events

# send a message with a voice note
curl -H 'Authorization: Bearer demo-token' \
     -F 'message={"content":"listen to this"}' \
     -F 'files=@note.ogg' \
     http://localhost:8080/channels/demo/messages
```

## Testing

`couriertest.RunProviderSuite` checks that a provider honours the contract of
the courier interfaces. `provider/memory` is an in-process provider, handy to
develop an application without connecting to a real platform.

```go
func TestProviderConformance(t *testing.T) {
  couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
    provider := memory.NewProvider()

    return &couriertest.Harness{
      Provider: provider,
      Deliver:  provider.Deliver,
      Sent:     provider.Sent,
      Channel:  courier.NewChannel("memory", courier.ChannelKindDirect, "memory"),
      From:     courier.NewUser("user-1", "User"),
    }
  })
}
```

See [`./examples`](./examples) for more examples.

## License

This library is released under the [GNU General Public License v3.0](LICENSE).
