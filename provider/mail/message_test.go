package mail

import (
	"context"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/DusanKasan/parsemail"
	"github.com/bornholm/go-courier"
)

func TestThreadID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		email    parsemail.Email
		expected courier.ChannelID
	}{
		{
			name:     "thread root is its own channel",
			email:    parsemail.Email{MessageID: "root@example.org"},
			expected: "root@example.org",
		},
		{
			name: "reply joins the channel of the root it references",
			email: parsemail.Email{
				MessageID:  "reply@example.org",
				InReplyTo:  []string{"root@example.org"},
				References: []string{"root@example.org"},
			},
			expected: "root@example.org",
		},
		{
			name: "deep reply still joins the root, not its parent",
			email: parsemail.Email{
				MessageID:  "third@example.org",
				InReplyTo:  []string{"reply@example.org"},
				References: []string{"root@example.org", "reply@example.org"},
			},
			expected: "root@example.org",
		},
		{
			name: "reply without references falls back on its parent",
			email: parsemail.Email{
				MessageID: "reply@example.org",
				InReplyTo: []string{"root@example.org"},
			},
			expected: "root@example.org",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := threadID(&tc.email); got != tc.expected {
				t.Errorf("threadID: got %q, expected %q", got, tc.expected)
			}
		})
	}
}

func TestKindOf(t *testing.T) {
	address := func(raw string) *mail.Address {
		return &mail.Address{Address: raw}
	}

	for _, tc := range []struct {
		name     string
		email    parsemail.Email
		expected courier.ChannelKind
	}{
		{
			name:     "single recipient is a direct exchange",
			email:    parsemail.Email{To: []*mail.Address{address("me@example.org")}},
			expected: courier.ChannelKindDirect,
		},
		{
			name: "several recipients make it a group",
			email: parsemail.Email{To: []*mail.Address{
				address("me@example.org"), address("you@example.org"),
			}},
			expected: courier.ChannelKindGroup,
		},
		{
			name: "a recipient in copy also makes it a group",
			email: parsemail.Email{
				To: []*mail.Address{address("me@example.org")},
				Cc: []*mail.Address{address("you@example.org")},
			},
			expected: courier.ChannelKindGroup,
		},
		{
			name: "a mailing list is public",
			email: parsemail.Email{
				To:     []*mail.Address{address("list@example.org")},
				Header: mail.Header{"List-Id": []string{"<list.example.org>"}},
			},
			expected: courier.ChannelKindPublic,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindOf(&tc.email); got != tc.expected {
				t.Errorf("kindOf: got %q, expected %q", got, tc.expected)
			}
		})
	}
}

func TestSenderOfWithoutFrom(t *testing.T) {
	// A malformed email must not bring the provider down.
	user := senderOf(&parsemail.Email{})

	if user == nil {
		t.Fatal("senderOf: got nil, expected a user")
	}

	if got := user.ID(); got != "" {
		t.Errorf("user.ID(): got %q, expected empty", got)
	}
}

func TestToMessage(t *testing.T) {
	ctx := context.Background()

	provider := NewProvider()
	defer provider.releaseAll()

	email := &parsemail.Email{
		MessageID:  "reply@example.org",
		InReplyTo:  []string{"root@example.org"},
		References: []string{"root@example.org"},
		Subject:    "Quarterly report",
		Date:       time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
		From:       []*mail.Address{{Address: "alice@example.org", Name: "Alice"}},
		To:         []*mail.Address{{Address: "bob@example.org"}},
		TextBody:   "Here is the report.",
		HTMLBody:   "<p>Here is the report.</p>",
		Attachments: []parsemail.Attachment{{
			Filename:    "report.pdf",
			ContentType: "application/pdf",
			Data:        strings.NewReader("%PDF-1.4"),
		}},
	}

	message := provider.toMessage(email)

	if got := message.ID(); got != "reply@example.org" {
		t.Errorf("message.ID(): got %q, expected %q", got, "reply@example.org")
	}

	if got := message.Channel().ChannelID(); got != "root@example.org" {
		t.Errorf("channel id: got %q, expected %q", got, "root@example.org")
	}

	if got := message.Channel().Kind(); got != courier.ChannelKindDirect {
		t.Errorf("channel kind: got %q, expected %q", got, courier.ChannelKindDirect)
	}

	parent, ok := courier.InReplyTo(message)
	if !ok || parent != "root@example.org" {
		t.Errorf("courier.InReplyTo: got (%q, %v), expected (%q, true)", parent, ok, "root@example.org")
	}

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil {
		t.Fatalf("courier.GetMessageMainContent: %+v", err)
	}

	if content != "Here is the report." {
		t.Errorf("main content: got %q, expected %q", content, "Here is the report.")
	}

	attachments := courier.Attachments(message)
	if len(attachments) != 2 {
		t.Fatalf("len(courier.Attachments(message)): got %d, expected 2 (html body + pdf)", len(attachments))
	}

	var pdf courier.Attachment

	for _, attachment := range attachments {
		if attachment.ContentType() == "application/pdf" {
			pdf = attachment
		}
	}

	if pdf == nil {
		t.Fatal("no application/pdf attachment found")
	}

	// The IMAP reader is single use, so the content must have been buffered
	// to survive being read twice.
	for attempt := 1; attempt <= 2; attempt++ {
		data, err := courier.ReadPart(ctx, pdf)
		if err != nil {
			t.Fatalf("attempt %d: %+v", attempt, err)
		}

		if string(data) != "%PDF-1.4" {
			t.Errorf("attempt %d: got %q, expected %q", attempt, string(data), "%PDF-1.4")
		}
	}
}
