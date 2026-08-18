// Package signal implements a courier.Provider backed by the signal-cli
// daemon (https://github.com/AsamK/signal-cli) and its JSON-RPC interface
// (`signal-cli daemon --tcp` or `--socket`).
//
// The daemon owns the Signal account, its registration and its message
// store; this provider is a thin client. Incoming attachments are fetched
// lazily through the getAttachment RPC method, outgoing ones are inlined as
// RFC 2397 data URIs in the send call.
//
// Channel identifiers: a direct conversation uses the peer's E.164 number
// (or UUID) as ChannelID; a group uses "group.<base64 group id>". The
// prefix removes the ambiguity between the two spaces of identifiers,
// which nothing in signal-cli distinguishes syntactically.
package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/bornholm/go-courier"
)

// groupChannelPrefix marks a ChannelID as designating a group.
const groupChannelPrefix = "group."

type Provider struct {
	opts *Options

	connectOnce sync.Once
	connectErr  error
	client      *rpcClient

	listenOnce sync.Once
	messages   chan courier.Message
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:     NewOptions(funcs...),
		messages: make(chan courier.Message),
	}
}

// connect lazily dials the daemon, once for the provider's lifetime.
func (p *Provider) connect(ctx context.Context) (*rpcClient, error) {
	p.connectOnce.Do(func() {
		p.client, p.connectErr = dialRPC(ctx, p.opts.Address)
	})
	return p.client, errors.WithStack(p.connectErr)
}

// params returns the base parameters of every call: in multi-account mode
// the daemon requires the account on each request, and sending it to a
// single-account daemon is accepted.
func (p *Provider) params() map[string]any {
	params := map[string]any{}
	if p.opts.Account != "" {
		params["account"] = p.opts.Account
	}
	return params
}

// Listen implements [courier.Provider].
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	client, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}

	p.listenOnce.Do(func() {
		go func() {
			defer close(p.messages)

			for {
				select {
				case <-ctx.Done():
					_ = client.close()
					return
				case raw, ok := <-client.notifications:
					if !ok {
						return
					}

					message, ok := p.toMessage(raw)
					if !ok {
						continue // reçus, frappes, messages sans contenu
					}

					select {
					case p.messages <- message:
					case <-ctx.Done():
						_ = client.close()
						return
					}
				}
			}
		}()
	})

	return p.messages, nil
}

// Send implements [courier.Provider].
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	client, err := p.connect(ctx)
	if err != nil {
		return err
	}

	text, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return errors.WithStack(err)
	}

	params := p.params()
	params["message"] = text

	addressSendParams(params, string(message.Channel().ChannelID()))

	// Outgoing attachments travel inline as RFC 2397 data URIs: the daemon
	// may live on another host, a filesystem path would not survive that.
	var attachments []string
	for _, attachment := range courier.Attachments(message) {
		data, err := courier.ReadPart(ctx, attachment)
		if err != nil {
			return errors.Wrapf(err, "could not read attachment %q", attachment.Name())
		}

		uri := fmt.Sprintf("data:%s;filename=%s;base64,%s",
			attachment.ContentType(),
			courier.FilenameFor(attachment),
			base64.StdEncoding.EncodeToString(data),
		)
		attachments = append(attachments, uri)
	}
	if len(attachments) > 0 {
		params["attachments"] = attachments
	}

	if _, err := client.call(ctx, "send", params); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// addressSendParams routes channelID to either a group or a direct
// recipient parameter.
func addressSendParams(params map[string]any, channelID string) {
	if groupID, ok := strings.CutPrefix(channelID, groupChannelPrefix); ok {
		params["groupId"] = groupID
		return
	}
	params["recipient"] = []string{channelID}
}

// SetStatus implements [courier.StatusProvider] through sendTyping. Signal
// shows the indicator for 15 seconds unless a stop message is sent.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
	client, err := p.connect(ctx)
	if err != nil {
		return err
	}

	params := p.params()
	addressSendParams(params, string(channelID))
	if status != courier.StatusTyping {
		params["stop"] = true
	}

	if _, err := client.call(ctx, "sendTyping", params); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Self implements [courier.SelfProvider]. The account number is
// configuration, not discovery: the daemon is started for a given account
// and the option mirrors it.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	if p.opts.Account == "" {
		return nil, errors.New("signal: account number not configured (WithAccount)")
	}
	return courier.NewUser(courier.UserID(p.opts.Account), p.opts.Account), nil
}

// Channel implements [courier.ChannelResolver] from the identifier alone:
// the two identifier spaces are distinguished by the group prefix, no RPC
// needed.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	id := string(channelID)
	if groupID, ok := strings.CutPrefix(id, groupChannelPrefix); ok {
		name := groupID

		client, err := p.connect(ctx)
		if err == nil {
			if groupName, ok := p.groupName(ctx, client, groupID); ok {
				name = groupName
			}
		}

		return courier.NewChannel(channelID, courier.ChannelKindGroup, name), nil
	}

	return courier.NewChannel(channelID, courier.ChannelKindDirect, id), nil
}

// groupName resolves a group title through listGroups, best effort.
func (p *Provider) groupName(ctx context.Context, client *rpcClient, groupID string) (string, bool) {
	result, err := client.call(ctx, "listGroups", p.params())
	if err != nil {
		return "", false
	}

	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result, &groups); err != nil {
		return "", false
	}

	for _, group := range groups {
		if group.ID == groupID && group.Name != "" {
			return group.Name, true
		}
	}

	return "", false
}

// Capabilities implements [courier.CapabilityProvider].
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityMentions,
		courier.CapabilityThreads,
		courier.CapabilityStatus,
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.StatusProvider     = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
