package rocket

import (
	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/gopackage/ddp"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type Update struct {
	EventName string `mapstructure:"eventName"`
	Args      []any  `mapstructure:"args"`
}

type MessageInfo struct {
	ID      string `mapstructure:"_id"`
	RoomID  string `mapstructure:"rid"`
	Message string `mapstructure:"msg"`
	User    struct {
		ID       string `mapstructure:"_id"`
		Name     string `mapstructure:"name"`
		Username string `mapstructure:"username"`
	} `mapstructure:"u"`
}

type RoomInfo struct {
	IsParticipant bool   `mapstructure:"roomParticipant"`
	Type          string `mapstructure:"roomType"`
}

type messageListener struct {
	username         string
	messageChan      chan courier.Message
	receivedMessages syncx.Map[string, struct{}]
	platformID       string
}

// CollectionUpdate implements ddp.UpdateListener.
func (l *messageListener) CollectionUpdate(collection string, operation string, id string, doc ddp.Update) {
	update := Update{}
	if err := mapstructure.Decode(doc, &update); err != nil {
		slog.Error("could not decode ddp update", slog.Any("error", errors.WithStack(err)))
		return
	}

	if len(update.Args) != 2 || update.EventName != "__my_messages__" {
		return
	}

	roomInfo := RoomInfo{}

	if err := mapstructure.Decode(update.Args[1], &roomInfo); err != nil {
		slog.Error("could not decode argument", slog.Any("error", errors.WithStack(err)))
		return
	}

	if roomInfo.Type != "d" {
		return
	}

	messageInfo := MessageInfo{}

	if err := mapstructure.Decode(update.Args[0], &messageInfo); err != nil {
		slog.Error("could not decode argument", slog.Any("error", errors.WithStack(err)))
		return
	}

	if messageInfo.User.Username == l.username {
		return
	}

	if _, exists := l.receivedMessages.LoadOrStore(messageInfo.ID, struct{}{}); exists {
		return
	}

	message := courier.NewMessage(
		courier.MessageID(messageInfo.ID),
		courier.ChannelID(messageInfo.RoomID),
		courier.NewUser(courier.UserID(messageInfo.User.ID), messageInfo.User.Name),
		courier.WithMessageMainPart(messageInfo.Message),
	)

	l.messageChan <- message
}

var _ ddp.UpdateListener = &messageListener{}
