package courier

type ChannelID string

// ChannelKind describes the conversational nature of a channel. Providers
// unable to determine it report ChannelKindUnknown.
type ChannelKind string

const (
	// ChannelKindUnknown is reported when the provider cannot tell direct
	// conversations from group ones.
	ChannelKindUnknown ChannelKind = ""
	// ChannelKindDirect is a one to one conversation.
	ChannelKindDirect ChannelKind = "direct"
	// ChannelKindGroup is a closed conversation between several users.
	ChannelKindGroup ChannelKind = "group"
	// ChannelKindPublic is an open room, broadcast list or newsletter.
	ChannelKindPublic ChannelKind = "public"
)

type Channel interface {
	ChannelID() ChannelID
	Kind() ChannelKind
	Name() string
}

type BaseChannel struct {
	id   ChannelID
	kind ChannelKind
	name string
}

// ChannelID implements Channel.
func (c *BaseChannel) ChannelID() ChannelID {
	return c.id
}

// Kind implements Channel.
func (c *BaseChannel) Kind() ChannelKind {
	return c.kind
}

// Name implements Channel.
func (c *BaseChannel) Name() string {
	return c.name
}

var _ Channel = &BaseChannel{}

func NewChannel(id ChannelID, kind ChannelKind, name string) *BaseChannel {
	return &BaseChannel{
		id:   id,
		kind: kind,
		name: name,
	}
}

// NewChannelRef returns a channel only carrying its identifier, for cases
// where the caller addresses a channel without knowing anything else about
// it, typically when building an outgoing message.
func NewChannelRef(id ChannelID) *BaseChannel {
	return NewChannel(id, ChannelKindUnknown, "")
}

// IsGroupChannel returns true when several users may read the channel.
func IsGroupChannel(channel Channel) bool {
	if channel == nil {
		return false
	}

	switch channel.Kind() {
	case ChannelKindGroup, ChannelKindPublic:
		return true
	default:
		return false
	}
}
