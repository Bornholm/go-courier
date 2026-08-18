# go-courier

Bibliothèque Go fournissant une interface unifiée pour envoyer et recevoir des messages depuis/vers différentes plateformes de messagerie.

## Module

`github.com/bornholm/go-courier` — Go 1.24.6

## Architecture

### Interfaces core (package racine `courier`)

- `Provider` — interface de base : `Listen(ctx) (chan Message, error)` et `Send(ctx, message) error`
- `Message` — `ID()`, `From()`, `SentAt()`, `Parts()`, `Channel()`
- `MessagePart` — `Name()`, `ContentType()`, `Reader(ctx) (io.ReadCloser, error)`
- `Attachment` — étend `MessagePart` avec `Filename()`, `Size()`, `Disposition()`, `Caption()`
- `VoiceNote` — interface optionnelle : `Duration()`, `IsVoiceNote()`
- `Channel` — `ChannelID()`, `Kind()`, `Name()`
- `User` / `UserID`, `Mention`

Interfaces provider optionnelles, détectées par type-assertion :

- `PresenceProvider` — `SetPresence(ctx, presence)`
- `StatusProvider` — `SetStatus(ctx, status, channelID)`
- `SelfProvider` — `Self(ctx) (User, error)` : qui suis-je sur cette plateforme
- `ChannelResolver` — `Channel(ctx, channelID) (Channel, error)`
- `CapabilityProvider` — `Capabilities() []Capability`

Interfaces message optionnelles :

- `MentionedMessage` — `Mentions() []Mention`, via le helper `courier.Mentions`
- `ThreadedMessage` — `InReplyTo() MessageID`, via le helper `courier.InReplyTo`

### Providers disponibles (`provider/`)

| Package | Plateforme | Pièces jointes | Type de canal | Mentions |
|---------|-----------|---|---|---|
| `provider/whatsapp` | WhatsApp (whatsmeow) | oui, notes vocales incluses | oui | oui |
| `provider/signal` | Signal (daemon signal-cli, JSON-RPC) | oui, notes vocales incluses | oui | oui |
| `provider/mail` | SMTP/IMAP | oui | oui | non |
| `provider/rocket` | Rocket.Chat (DDP + REST) | oui | oui | oui |
| `provider/discord` | Discord | oui | oui | oui |
| `provider/rest` | API REST JSON (multipart + SSE) | oui | configurable | oui |
| `provider/memory` | en mémoire, pour les tests | oui | configurable | oui |

**Les providers ne filtrent pas.** Ils remontent tout ce qu'ils reçoivent, groupes et médias inclus ; c'est l'application qui décide de répondre, via `Channel().Kind()` et `courier.IsMentioned`.

### Utilitaires

- `syncx/map.go` — map concurrente thread-safe
- `couriertest/` — suite de conformance réutilisable (`RunProviderSuite`)
- `opener.go` — `PartOpener` et ses constructeurs : `OpenerFromBytes`, `OpenerFromString`, `OpenerFromFile`, `OpenerOnce`, `BufferedOpener`
- `attachment.go` — `MediaKindOf`, `Attachments`, `AttachmentsByKind`, `FilenameFor`, `IsVoiceNote`
- `message.go` — `NewMessage`, `GetMessageMainContent`, `ReadPart`, `IsMentioned`, `InReplyTo`
- `channel.go` — `NewChannel`, `NewChannelRef`, `IsGroupChannel`

## Commandes utiles

```bash
go build ./...          # Compiler tout le projet
go test ./...           # Lancer les tests
go vet ./...            # Vérification statique
```

## Conventions

- Erreurs wrappées avec `github.com/pkg/errors` (`errors.WithStack`)
- Les providers utilisent `sync.Once` ou un mutex pour l'initialisation lazy
- Les `ChannelID` sont des strings typées (ex: JID WhatsApp, `rid` Rocket.Chat, Message-ID racine du fil pour le mail)
- Options fonctionnelles pour tous les providers : `NewProvider(WithX(...), WithY(...))`
- Le contenu des pièces jointes est **lazy** : il n'est téléchargé qu'à la lecture de la part. C'est la raison d'être du `ctx` dans `Reader(ctx)`.
- Les parts doivent être **rejouables** : deux lectures successives donnent le même contenu. Envelopper toute source à usage unique dans `courier.BufferedOpener`, dont la `CloseFunc` doit être enregistrée pour libérer le fichier temporaire éventuel.
- Les exemples sont dans `examples/` (rest-echo, attachment-echo, discord-echo, whatsapp-echo)

## Ajouter un nouveau provider

1. Créer `provider/<nom>/provider.go` et `provider/<nom>/options.go`
2. Implémenter au minimum `courier.Provider`
3. Remplir `Channel().Kind()`, les mentions et `InReplyTo` si la plateforme les expose
4. Implémenter les interfaces optionnelles pertinentes (`SelfProvider`, `ChannelResolver`, `PresenceProvider`, `StatusProvider`)
5. Déclarer `Capabilities()` en cohérence avec ce qui est réellement implémenté
6. Ajouter les assertions de compilation `var _ courier.<Interface> = &Provider{}` pour **chaque** interface implémentée
7. Brancher `couriertest.RunProviderSuite` dans un test
8. Créer un exemple dans `examples/<nom>-echo/main.go`
