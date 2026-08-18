package signal

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/couriertest"
)

// fakeDaemon speaks the signal-cli JSON-RPC dialect over TCP: send,
// sendTyping and getAttachment calls are answered, receive notifications
// are pushed on demand. It stores attachments passed to Deliver so that
// getAttachment can serve them, as the real daemon serves files from disk.
type fakeDaemon struct {
	listener net.Listener

	mu          sync.Mutex
	conns       []net.Conn
	sent        []map[string]any
	attachments map[string][]byte
	nextAttID   int
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	daemon := &fakeDaemon{listener: listener, attachments: map[string][]byte{}}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			daemon.mu.Lock()
			daemon.conns = append(daemon.conns, conn)
			daemon.mu.Unlock()
			go daemon.serve(conn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })

	return daemon
}

func (d *fakeDaemon) address() string { return "tcp://" + d.listener.Addr().String() }

func (d *fakeDaemon) serve(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 128*1024*1024)

	for scanner.Scan() {
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		var result any = map[string]any{}

		switch req.Method {
		case "send":
			d.mu.Lock()
			d.sent = append(d.sent, req.Params)
			d.mu.Unlock()
			result = map[string]any{"timestamp": time.Now().UnixMilli()}
		case "getAttachment":
			id, _ := req.Params["id"].(string)
			d.mu.Lock()
			data, ok := d.attachments[id]
			d.mu.Unlock()
			if !ok {
				d.reply(conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown attachment"}})
				continue
			}
			result = map[string]any{"data": base64.StdEncoding.EncodeToString(data)}
		}

		d.reply(conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func (d *fakeDaemon) reply(conn net.Conn, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	_, _ = conn.Write(append(raw, '\n'))
}

// notify pushes a receive notification to every connected client.
func (d *fakeDaemon) notify(params map[string]any) error {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "receive", "params": params})
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, conn := range d.conns {
		if _, err := conn.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// deliver converts a courier.Message to the envelope the real daemon would
// produce, registering attachment bytes for later getAttachment calls.
func (d *fakeDaemon) deliver(ctx context.Context, message courier.Message) error {
	data := map[string]any{"timestamp": message.SentAt().UnixMilli()}

	if content, err := courier.GetMessageMainContent(ctx, message); err == nil {
		data["message"] = content
	}

	channelID := string(message.Channel().ChannelID())
	if groupID, ok := strings.CutPrefix(channelID, groupChannelPrefix); ok {
		data["groupInfo"] = map[string]any{"groupId": groupID}
	}

	var mentions []map[string]any
	for _, mention := range courier.Mentions(message) {
		mentions = append(mentions, map[string]any{"number": string(mention.UserID), "name": mention.DisplayName})
	}
	if mentions != nil {
		data["mentions"] = mentions
	}

	if parent, ok := courier.InReplyTo(message); ok {
		data["quote"] = map[string]any{"id": string(parent), "author": string(message.From().ID())}
	}

	var attachments []map[string]any
	for _, attachment := range courier.Attachments(message) {
		payload, err := courier.ReadPart(ctx, attachment)
		if err != nil {
			return err
		}

		d.mu.Lock()
		d.nextAttID++
		id := fmt.Sprintf("att-%d", d.nextAttID)
		d.attachments[id] = payload
		d.mu.Unlock()

		attachments = append(attachments, map[string]any{
			"id":          id,
			"contentType": attachment.ContentType(),
			"filename":    attachment.Filename(),
			"size":        len(payload),
			"voiceNote":   courier.IsVoiceNote(attachment),
		})
	}
	if attachments != nil {
		data["attachments"] = attachments
	}

	return d.notify(map[string]any{
		"account": "+33600000000",
		"envelope": map[string]any{
			"source":      string(message.From().ID()),
			"sourceName":  message.From().DisplayName(),
			"timestamp":   message.SentAt().UnixMilli(),
			"dataMessage": data,
		},
	})
}

// sentMessages rebuilds courier messages from the send calls the daemon
// received, data URIs decoded, so the suite can inspect what actually left.
func (d *fakeDaemon) sentMessages(t *testing.T) []courier.Message {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	var messages []courier.Message
	for _, params := range d.sent {
		var options []courier.BaseMessageOptionFunc
		if text, _ := params["message"].(string); text != "" {
			options = append(options, courier.WithMessageMainPart(text))
		}

		if rawAttachments, ok := params["attachments"].([]any); ok {
			for i, raw := range rawAttachments {
				uri, _ := raw.(string)
				contentType, filename, payload, err := parseDataURI(uri)
				if err != nil {
					t.Fatalf("attachment %d: %v", i, err)
				}
				options = append(options, courier.WithMessagePart(
					courier.NewAttachment(filename, contentType, courier.OpenerFromBytes(payload)),
				))
			}
		}

		var channel courier.Channel
		if groupID, ok := params["groupId"].(string); ok && groupID != "" {
			channel = courier.NewChannel(courier.ChannelID(groupChannelPrefix+groupID), courier.ChannelKindGroup, groupID)
		} else {
			recipients, _ := params["recipient"].([]any)
			recipient, _ := recipients[0].(string)
			channel = courier.NewChannel(courier.ChannelID(recipient), courier.ChannelKindDirect, recipient)
		}

		messages = append(messages, courier.NewMessage(courier.RandomMessageID(), channel, courier.NewUser("+33600000000", "moi"), options...))
	}

	return messages
}

// parseDataURI decodes an RFC 2397 data URI as produced by Send.
func parseDataURI(uri string) (contentType, filename string, data []byte, err error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", "", nil, fmt.Errorf("not a data URI: %q", uri)
	}

	meta, encoded, ok := strings.Cut(rest, ";base64,")
	if !ok {
		return "", "", nil, fmt.Errorf("not base64-encoded: %q", uri)
	}

	contentType, params, _ := strings.Cut(meta, ";")
	if value, ok := strings.CutPrefix(params, "filename="); ok {
		filename = value
	}

	data, err = base64.StdEncoding.DecodeString(encoded)
	return contentType, filename, data, err
}

func TestProviderConformance(t *testing.T) {
	for name, channel := range map[string]courier.Channel{
		"direct": courier.NewChannel("+33712345678", courier.ChannelKindDirect, "Alice"),
		"group":  courier.NewChannel(groupChannelPrefix+"Zm9vYmFyCg==", courier.ChannelKindGroup, "famille"),
	} {
		t.Run(name, func(t *testing.T) {
			couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
				daemon := newFakeDaemon(t)
				provider := NewProvider(WithAddress(daemon.address()), WithAccount("+33600000000"))

				return &couriertest.Harness{
					Provider: provider,
					Deliver:  daemon.deliver,
					Sent:     func() []courier.Message { return daemon.sentMessages(t) },
					Channel:  channel,
					From:     courier.NewUser("+33712345678", "Alice"),
				}
			})
		})
	}
}
