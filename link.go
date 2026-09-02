package courier

// LinkPreview describes a link shared with a preview card: the sender's
// client resolved the URL into a title, a description and a thumbnail. The
// preview is metadata about a remote page — the message carries no file for
// it, and the thumbnail below is NOT part of the message attachments.
type LinkPreview struct {
	// URL is the canonical address of the shared page, falling back to the
	// text the platform matched as a link.
	URL         string
	Title       string
	Description string

	// Thumbnail is the preview image, when the platform provides one. It is
	// deliberately kept out of Attachments: an application that treated it
	// as a received file would mistake a link share for a media upload.
	Thumbnail MessagePart
}

// LinkPreviewMessage is implemented by messages carrying one or more link
// preview cards. Use the LinkPreviews helper rather than asserting directly.
type LinkPreviewMessage interface {
	Message
	LinkPreviews() []LinkPreview
}

// LinkPreviews returns the link previews carried by the message, or nil when
// the message is a plain text or the provider does not surface previews.
func LinkPreviews(message Message) []LinkPreview {
	previewed, ok := message.(LinkPreviewMessage)
	if !ok {
		return nil
	}

	return previewed.LinkPreviews()
}
