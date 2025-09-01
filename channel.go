package courier

type ChannelID string

type Channel interface {
	ChannelID() ChannelID
}
