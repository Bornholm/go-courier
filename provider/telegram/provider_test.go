package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/couriertest"
	"github.com/pkg/errors"
)

const (
	testToken    = "123456:secret-token"
	testBotID    = 999
	testBotName  = "goslyn_bot"
	testChatID   = 4242
	testUserID   = 4242
	testUserName = "Alice"
)

// fakeBotAPI stands in for the Telegram Bot API.
type fakeBotAPI struct {
	t      *testing.T
	server *httptest.Server

	updates chan apiUpdate

	mu      sync.Mutex
	nextID  int
	sent    []fakeSent
	actions []map[string]any
	files   map[string]fakeFile
	offsets []int64
}

// fakeSent is a message the provider sent.
type fakeSent struct {
	method   string
	fields   map[string]any
	filename string
	mimeType string
	content  []byte
}

type fakeFile struct {
	contentType string
	content     []byte
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	f := &fakeBotAPI{
		t:       t,
		updates: make(chan apiUpdate, 16),
		files:   map[string]fakeFile{},
		nextID:  100,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /bot"+testToken+"/{method}", f.handleMethod)
	mux.HandleFunc("GET /file/bot"+testToken+"/{path...}", f.handleFile)

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeBotAPI) ok(w http.ResponseWriter, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		f.t.Errorf("encoding result: %v", err)
	}
	fmt.Fprintf(w, `{"ok":true,"result":%s}`, data)
}

func (f *fakeBotAPI) handleMethod(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")

	fields := map[string]any{}
	var upload *fakeSent

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for name, values := range r.MultipartForm.Value {
			fields[name] = values[0]
		}
		for name, headers := range r.MultipartForm.File {
			file, err := headers[0].Open()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			content, _ := io.ReadAll(file)
			file.Close()
			upload = &fakeSent{filename: headers[0].Filename, mimeType: headers[0].Header.Get("Content-Type"), content: content}
			fields["_file_field"] = name
		}
	} else if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch method {
	case "getMe":
		f.ok(w, apiUser{ID: testBotID, IsBot: true, FirstName: "Goslyn", Username: testBotName})

	case "getUpdates":
		f.mu.Lock()
		f.offsets = append(f.offsets, int64(fields["offset"].(float64)))
		f.mu.Unlock()

		timeout := time.Duration(fields["timeout"].(float64)) * time.Second
		select {
		case update := <-f.updates:
			f.ok(w, []apiUpdate{update})
		case <-time.After(timeout):
			f.ok(w, []apiUpdate{})
		case <-r.Context().Done():
		}

	case "getFile":
		f.ok(w, map[string]any{"file_id": fields["file_id"], "file_path": "documents/" + fields["file_id"].(string)})

	case "getChat":
		f.ok(w, apiChat{ID: int64(fields["chat_id"].(float64)), Type: "supergroup", Title: "Family"})

	case "sendChatAction":
		f.mu.Lock()
		f.actions = append(f.actions, fields)
		f.mu.Unlock()
		f.ok(w, true)

	case "sendMessage", "sendPhoto", "sendDocument", "sendVoice", "sendAudio", "sendVideo":
		sent := fakeSent{method: method, fields: fields}
		if upload != nil {
			sent.filename, sent.mimeType, sent.content = upload.filename, upload.mimeType, upload.content
		}

		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.sent = append(f.sent, sent)
		f.mu.Unlock()

		f.ok(w, apiMessage{MessageID: id, Chat: apiChat{ID: testChatID, Type: "private"}})

	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"ok":false,"error_code":404,"description":"Not Found: method %s"}`, method)
	}
}

func (f *fakeBotAPI) handleFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.PathValue("path"), "documents/")

	f.mu.Lock()
	file, ok := f.files[id]
	f.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", file.contentType)
	_, _ = w.Write(file.content)
}

func (f *fakeBotAPI) storeFile(contentType string, content []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	id := "file-" + strconv.Itoa(f.nextID)
	f.files[id] = fakeFile{contentType: contentType, content: content}

	return id
}

func (f *fakeBotAPI) deliver(update apiUpdate) {
	f.updates <- update
}

func (f *fakeBotAPI) sentMessages() []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]fakeSent(nil), f.sent...)
}

func newTestProvider(f *fakeBotAPI) *Provider {
	return NewProvider(
		WithToken(testToken),
		WithBaseURL(f.server.URL),
		WithPollTimeout(time.Second),
		WithMaxRetryDelay(50*time.Millisecond),
	)
}

func privateMessage(id int, text string) *apiMessage {
	return &apiMessage{
		MessageID: id,
		From:      &apiUser{ID: testUserID, FirstName: testUserName},
		Date:      time.Now().Unix(),
		Chat:      apiChat{ID: testChatID, Type: "private", FirstName: testUserName},
		Text:      text,
	}
}

func listen(t *testing.T, provider *Provider) (context.Context, chan courier.Message) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen(ctx): %+v", err)
	}

	return ctx, messages
}

func receive(t *testing.T, messages chan courier.Message) courier.Message {
	t.Helper()

	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
		return nil
	}
}

// translatedMessage restores, on a received message, the identifiers the
// conformance suite chose. The suite names users and messages with random
// strings, where Telegram only knows integers: the harness hands the
// provider integers and maps them back here.
type translatedMessage struct {
	courier.Message
	mentions  []courier.Mention
	inReplyTo courier.MessageID
}

func (m *translatedMessage) Mentions() []courier.Mention  { return m.mentions }
func (m *translatedMessage) InReplyTo() courier.MessageID { return m.inReplyTo }

func TestProviderSuite(t *testing.T) {
	couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
		api := newFakeBotAPI(t)
		provider := newTestProvider(api)

		var (
			mu         sync.Mutex
			nextID     = 1
			users      = map[int64]courier.UserID{}
			messages   = map[int]courier.MessageID{}
			numericFor = func(table map[int]courier.MessageID, original courier.MessageID) int {
				mu.Lock()
				defer mu.Unlock()
				nextID++
				table[nextID] = original
				return nextID
			}
		)

		return &couriertest.Harness{
			Provider: provider,
			Channel:  courier.NewChannel(courier.ChannelID(strconv.Itoa(testChatID)), courier.ChannelKindDirect, testUserName),
			From:     courier.NewUser(courier.UserID(strconv.Itoa(testUserID)), testUserName),
			Listen: func(ctx context.Context) (chan courier.Message, error) {
				received, err := provider.Listen(ctx)
				if err != nil {
					return nil, err
				}

				translated := make(chan courier.Message)
				go func() {
					defer close(translated)
					for message := range received {
						out := &translatedMessage{Message: message}

						mu.Lock()
						for _, mention := range courier.Mentions(message) {
							id, _ := strconv.ParseInt(string(mention.UserID), 10, 64)
							mention.UserID = users[id]
							out.mentions = append(out.mentions, mention)
						}
						if parent, ok := courier.InReplyTo(message); ok {
							id, _ := parseMessageID(parent)
							out.inReplyTo = messages[id]
						}
						mu.Unlock()

						select {
						case translated <- out:
						case <-ctx.Done():
							return
						}
					}
				}()

				return translated, nil
			},
			Deliver: func(ctx context.Context, message courier.Message) error {
				text, err := courier.GetMessageMainContent(ctx, message)
				if err != nil && !errors.Is(err, courier.ErrNotFound) {
					return errors.WithStack(err)
				}

				raw := privateMessage(numericFor(map[int]courier.MessageID{}, message.ID()), text)

				for _, mention := range courier.Mentions(message) {
					mu.Lock()
					nextID++
					id := int64(nextID)
					users[id] = mention.UserID
					mu.Unlock()

					raw.Entities = append(raw.Entities, apiEntity{
						Type: "text_mention", Offset: 0, Length: len(utf16.Encode([]rune(text))),
						User: &apiUser{ID: id, FirstName: mention.DisplayName},
					})
				}

				if parent, ok := courier.InReplyTo(message); ok {
					raw.ReplyToMessage = &apiMessage{MessageID: numericFor(messages, parent), Chat: raw.Chat}
				}

				// Every attachment travels as a document, the only kind
				// carrying a filename.
				for _, attachment := range courier.Attachments(message) {
					content, err := courier.ReadPart(ctx, attachment)
					if err != nil {
						return errors.WithStack(err)
					}

					id := api.storeFile(attachment.ContentType(), content)
					raw.Text, raw.Caption = "", text
					raw.Document = &apiFile{FileID: id, FileName: attachment.Filename(), MimeType: attachment.ContentType(), FileSize: int64(len(content))}
				}

				api.deliver(apiUpdate{UpdateID: int64(raw.MessageID), Message: raw})
				return nil
			},
			Sent: func() []courier.Message {
				var sent []courier.Message
				for _, s := range api.sentMessages() {
					funcs := []courier.BaseMessageOptionFunc{}
					if text, ok := s.fields["text"].(string); ok {
						funcs = append(funcs, courier.WithMessageMainPart(text))
					}
					if caption, ok := s.fields["caption"].(string); ok {
						funcs = append(funcs, courier.WithMessageMainPart(caption))
					}
					if s.filename != "" {
						funcs = append(funcs, courier.WithMessagePart(courier.NewAttachment(
							s.filename, s.mimeType, courier.OpenerFromBytes(s.content),
						)))
					}
					sent = append(sent, courier.NewMessage(courier.RandomMessageID(), nil, nil, funcs...))
				}
				return sent
			},
		}
	})
}

func TestListenRejectsInvalidToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer server.Close()

	provider := NewProvider(WithToken("bad"), WithBaseURL(server.URL))

	_, err := provider.Listen(context.Background())

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusUnauthorized {
		t.Fatalf("Listen with a revoked token: got %v, expected a 401 APIError", err)
	}
}

func TestTransportErrorsDoNotLeakToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	baseURL := server.URL
	server.Close()

	provider := NewProvider(WithToken(testToken), WithBaseURL(baseURL))

	_, err := provider.Self(context.Background())
	if err == nil {
		t.Fatal("Self against a closed server: got no error")
	}
	if strings.Contains(fmt.Sprintf("%+v", err), "secret-token") {
		t.Errorf("error leaks the bot token: %+v", err)
	}
}

func TestSelfMentionByUsername(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	_, messages := listen(t, provider)

	// The emoji takes two UTF-16 code units: offsets count in those, not in
	// bytes nor runes.
	text := "👋 @Goslyn_Bot what time is it? @someone"
	raw := privateMessage(1, text)
	raw.Chat = apiChat{ID: -100, Type: "supergroup", Title: "Family"}
	raw.Entities = []apiEntity{
		{Type: "mention", Offset: 3, Length: 11},
		{Type: "mention", Offset: 32, Length: 8},
	}
	api.deliver(apiUpdate{UpdateID: 1, Message: raw})

	message := receive(t, messages)

	if !courier.IsMentioned(message, courier.UserID(strconv.Itoa(testBotID))) {
		t.Errorf("mentions = %+v, expected the bot", courier.Mentions(message))
	}
	if n := len(courier.Mentions(message)); n != 1 {
		t.Errorf("len(mentions) = %d, expected 1: other usernames cannot be resolved", n)
	}
	if got := message.Channel().Kind(); got != courier.ChannelKindGroup {
		t.Errorf("channel kind = %q, expected group", got)
	}
	if got := message.ID(); got != "-100:1" {
		t.Errorf("message id = %q, expected %q", got, "-100:1")
	}
}

func TestCommandAddressedToBot(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	_, messages := listen(t, provider)

	raw := privateMessage(1, "/help@goslyn_bot")
	raw.Entities = []apiEntity{{Type: "bot_command", Offset: 0, Length: 16}}
	api.deliver(apiUpdate{UpdateID: 1, Message: raw})

	message := receive(t, messages)

	if !courier.IsMentioned(message, courier.UserID(strconv.Itoa(testBotID))) {
		t.Errorf("a command suffixed with the bot username does not mention it")
	}
}

func TestVoiceNoteAndPhoto(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	ctx, messages := listen(t, provider)

	voice := api.storeFile("audio/ogg", []byte("opus"))
	small := api.storeFile("image/jpeg", []byte("small"))
	large := api.storeFile("image/jpeg", []byte("large"))

	raw := privateMessage(1, "")
	raw.Voice = &apiFile{FileID: voice, MimeType: "audio/ogg", Duration: 3}
	api.deliver(apiUpdate{UpdateID: 1, Message: raw})

	message := receive(t, messages)
	attachments := courier.Attachments(message)
	if len(attachments) != 1 || !courier.IsVoiceNote(attachments[0]) {
		t.Fatalf("attachments = %+v, expected a voice note", attachments)
	}
	if content, err := courier.ReadPart(ctx, attachments[0]); err != nil || string(content) != "opus" {
		t.Errorf("voice content = %q, %v", content, err)
	}

	raw = privateMessage(2, "")
	raw.Caption = "look"
	raw.Photo = []apiPhotoSize{{FileID: small, Width: 90}, {FileID: large, Width: 1280}}
	api.deliver(apiUpdate{UpdateID: 2, Message: raw})

	message = receive(t, messages)
	if text, _ := courier.GetMessageMainContent(ctx, message); text != "look" {
		t.Errorf("main content = %q, expected the caption", text)
	}
	attachments = courier.Attachments(message)
	if len(attachments) != 1 {
		t.Fatalf("len(attachments) = %d, expected only the largest photo", len(attachments))
	}
	if content, _ := courier.ReadPart(ctx, attachments[0]); string(content) != "large" {
		t.Errorf("photo content = %q, expected the largest size", content)
	}
}

func TestForumTopic(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	ctx, messages := listen(t, provider)

	raw := privateMessage(57, "hello")
	raw.Chat = apiChat{ID: -100, Type: "supergroup", Title: "Family"}
	raw.IsTopicMessage = true
	raw.MessageThreadID = 50
	raw.ReplyToMessage = &apiMessage{MessageID: 50}
	api.deliver(apiUpdate{UpdateID: 1, Message: raw})

	message := receive(t, messages)

	if got := message.Channel().ChannelID(); got != "-100/50" {
		t.Errorf("channel id = %q, expected the topic", got)
	}
	if parent, ok := courier.InReplyTo(message); ok {
		t.Errorf("InReplyTo = %q, expected none: the topic root is not what the user answered", parent)
	}

	reply := courier.NewMessage(courier.RandomMessageID(), message.Channel(), nil, courier.WithMessageMainPart("hi"))
	if _, err := provider.Send(ctx, reply); err != nil {
		t.Fatalf("Send: %+v", err)
	}

	sent := api.sentMessages()
	if got := sent[0].fields["message_thread_id"]; got != float64(50) {
		t.Errorf("message_thread_id = %v, expected the topic", got)
	}
	if got := sent[0].fields["chat_id"]; got != float64(-100) {
		t.Errorf("chat_id = %v, expected the supergroup", got)
	}
}

func TestSendLongTextAndReply(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)

	long := strings.Repeat("word ", 1000) // 5000 characters
	message := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(strconv.Itoa(testChatID))),
		nil,
		courier.WithMessageMainPart(long),
		courier.WithMessageInReplyTo(courier.MessageID(fmt.Sprintf("%d:7", testChatID))),
	)

	id, err := provider.Send(context.Background(), message)
	if err != nil {
		t.Fatalf("Send: %+v", err)
	}

	sent := api.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("len(sent) = %d, expected the text split in two", len(sent))
	}

	var joined string
	for _, s := range sent {
		chunk := s.fields["text"].(string)
		if n := len(utf16.Encode([]rune(chunk))); n > maxTextLength {
			t.Errorf("chunk of %d units exceeds the limit", n)
		}
		joined += chunk
	}
	if joined != long {
		t.Error("the chunks do not add up to the original text")
	}

	if _, ok := sent[0].fields["reply_parameters"]; !ok {
		t.Error("the first chunk does not reply to the parent")
	}
	if _, ok := sent[1].fields["reply_parameters"]; ok {
		t.Error("the second chunk replies too")
	}
	if id != courier.MessageID(fmt.Sprintf("%d:101", testChatID)) {
		t.Errorf("Send returned %q, expected the first message sent", id)
	}
}

func TestSendAttachmentKinds(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)

	message := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(strconv.Itoa(testChatID))),
		nil,
		courier.WithMessageMainPart("your answer"),
		courier.WithMessagePart(courier.NewAttachment("answer.ogg", "audio/ogg; codecs=opus",
			courier.OpenerFromString("opus"), courier.WithAttachmentVoiceNote(2*time.Second))),
		courier.WithMessagePart(courier.NewAttachment("chart.png", "image/png", courier.OpenerFromString("png"))),
	)

	if _, err := provider.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %+v", err)
	}

	sent := api.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("len(sent) = %d, expected one message per file, the text as caption", len(sent))
	}
	if sent[0].method != "sendVoice" || sent[0].fields["caption"] != "your answer" {
		t.Errorf("first = %s caption %v, expected a voice note carrying the text", sent[0].method, sent[0].fields["caption"])
	}
	if sent[1].method != "sendPhoto" || sent[1].fields["caption"] != nil {
		t.Errorf("second = %s caption %v, expected a photo without caption", sent[1].method, sent[1].fields["caption"])
	}
}

func TestReactions(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	ctx, _ := listen(t, provider)

	reactions, err := courier.ListenReactions(ctx, provider)
	if err != nil {
		t.Fatalf("ListenReactions: %+v", err)
	}

	update := &apiReactionUpdated{
		Chat:        apiChat{ID: testChatID, Type: "private"},
		MessageID:   12,
		User:        &apiUser{ID: testUserID, FirstName: testUserName},
		Date:        time.Now().Unix(),
		NewReaction: []apiReactionType{{Type: "custom_emoji"}, {Type: "emoji", Emoji: "👍"}},
	}
	api.deliver(apiUpdate{UpdateID: 1, MessageReaction: update})

	var got courier.Reaction
	select {
	case got = <-reactions:
	case <-time.After(5 * time.Second):
		t.Fatal("no reaction received")
	}

	if got.MessageID != courier.MessageID(fmt.Sprintf("%d:12", testChatID)) || got.Emoji != "👍" {
		t.Errorf("reaction = %+v, expected 👍 on the message", got)
	}

	removal := *update
	removal.OldReaction, removal.NewReaction = update.NewReaction, nil
	api.deliver(apiUpdate{UpdateID: 2, MessageReaction: &removal})

	select {
	case got = <-reactions:
	case <-time.After(5 * time.Second):
		t.Fatal("no removal received")
	}

	if !got.IsRemoval() {
		t.Errorf("reaction = %+v, expected a removal", got)
	}
}

func TestStatusAndChannel(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)
	ctx := context.Background()

	if err := provider.SetStatus(ctx, courier.StatusTyping, "-100/50"); err != nil {
		t.Fatalf("SetStatus(typing): %+v", err)
	}
	if err := provider.SetStatus(ctx, courier.StatusIdle, "-100/50"); err != nil {
		t.Fatalf("SetStatus(idle): %+v", err)
	}

	api.mu.Lock()
	actions := api.actions
	api.mu.Unlock()

	if len(actions) != 1 || actions[0]["action"] != "typing" || actions[0]["message_thread_id"] != float64(50) {
		t.Errorf("chat actions = %+v, expected a single typing in the topic", actions)
	}

	channel, err := provider.Channel(ctx, "-100")
	if err != nil {
		t.Fatalf("Channel: %+v", err)
	}
	if channel.Kind() != courier.ChannelKindGroup || channel.Name() != "Family" {
		t.Errorf("channel = %+v, expected the Family group", channel)
	}
}

func TestPollConfirmsOffsetOnStop(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)

	ctx, cancel := context.WithCancel(context.Background())
	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %+v", err)
	}

	api.deliver(apiUpdate{UpdateID: 41, Message: privateMessage(1, "hello")})
	receive(t, messages)

	cancel()
	for range messages {
	}

	api.mu.Lock()
	offsets := api.offsets
	api.mu.Unlock()

	if last := offsets[len(offsets)-1]; last != 42 {
		t.Errorf("last getUpdates offset = %d, expected 42 so that the handled update is not delivered again", last)
	}
}

func TestSplitText(t *testing.T) {
	text := "first line\nsecond line that is longer"
	chunks := splitText(text, 20)

	if strings.Join(chunks, "") != text {
		t.Fatalf("chunks %q do not rebuild the text", chunks)
	}
	if chunks[0] != "first line\n" {
		t.Errorf("first chunk = %q, expected a cut after the new line", chunks[0])
	}
	for _, chunk := range chunks {
		if utf16Len(chunk) > 20 {
			t.Errorf("chunk %q exceeds the limit", chunk)
		}
	}

	// Emojis outside the basic plane weigh two units each.
	emojis := strings.Repeat("😀", 15)
	for _, chunk := range splitText(emojis, 20) {
		if utf16Len(chunk) > 20 {
			t.Errorf("chunk %q exceeds the limit", chunk)
		}
	}

	// A limit below the size of a rune must still make progress.
	if chunks := splitText("😀😀", 1); strings.Join(chunks, "") != "😀😀" || len(chunks) != 2 {
		t.Errorf("splitText under a rune-sized limit = %q, expected one emoji per chunk", chunks)
	}
}

func TestSendWithForeignReplyTarget(t *testing.T) {
	api := newFakeBotAPI(t)
	provider := newTestProvider(api)

	message := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(strconv.Itoa(testChatID))),
		nil,
		courier.WithMessageMainPart("hello"),
		courier.WithMessageInReplyTo("not-a-telegram-id"),
	)

	if _, err := provider.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %+v", err)
	}

	sent := api.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("len(sent) = %d, expected the message to go anyway", len(sent))
	}
	if _, ok := sent[0].fields["reply_parameters"]; ok {
		t.Error("reply_parameters set for an unparsable reply target")
	}
}
