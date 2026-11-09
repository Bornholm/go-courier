package mail

import (
	"context"
	"encoding/base64"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/DusanKasan/parsemail"
	"github.com/bornholm/go-courier"
)

const parseHeader = "From: =?UTF-8?Q?Ren=C3=A9?= <client@example.org>\r\nSubject: =?UTF-8?Q?D=C3=A9j=C3=A0?=\r\n" +
	"Message-ID: <id@example.org>\r\nDate: Tue, 27 Oct 2026 09:41:00 +0100\r\nMIME-Version: 1.0\r\n"

func TestParseEmailBody(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("été à Noël"))
	alternative := "Content-Type: multipart/alternative;\r\n boundary=\"b\"\r\n\r\n--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=C3=A9t=C3=A9 =C3=A0 No=C3=ABl\r\n--b\r\n" +
		"Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("<p>été</p>")) + "\r\n--b--\r\n"

	for name, body := range map[string]string{
		"none":   "\r\nété à Noël\r\n",
		"8bit":   "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nété à Noël\r\n",
		"qp":     "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=C3=A9t=C3=A9 =C3=A0 No=C3=ABl\r\n",
		"base64": "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: BASE64\r\n\r\n" + b64 + "\r\n",
		"latin1": "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: 8bit\r\n\r\n\xe9t\xe9 \xe0 No\xebl\r\n",
		"cp1252": "Content-Type: text/plain; charset=windows-1252\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=E9t=E9 =E0 No=EBl\r\n",
		"alt":    alternative,
	} {
		t.Run(name, func(t *testing.T) {
			email, err := parseEmail(strings.NewReader(parseHeader + body))
			if err != nil {
				t.Fatal(err)
			}
			if email.TextBody != "été à Noël" {
				t.Errorf("body: %q", email.TextBody)
			}
			if email.Subject != "Déjà" || email.From[0].Address != "client@example.org" || email.From[0].Name != "René" ||
				email.MessageID != "id@example.org" || email.Date.IsZero() {
				t.Errorf("header: %+v", email)
			}
			if name == "alt" && email.HTMLBody != "<p>été</p>" {
				t.Errorf("html: %q", email.HTMLBody)
			}
		})
	}
}

// Every attachment keeps its content, whatever its type and encoding, and text
// attachments stay out of the body.
func TestParseEmailAttachments(t *testing.T) {
	for name, part := range map[string]struct{ header, body string }{
		"none":   {"", "contenu"},
		"7bit":   {"Content-Transfer-Encoding: 7bit\r\n", "contenu"},
		"8bit":   {"Content-Transfer-Encoding: 8bit\r\n", "contenu"},
		"binary": {"Content-Transfer-Encoding: binary\r\n", "contenu"},
		"qp":     {"Content-Transfer-Encoding: quoted-printable\r\n", "contenu"},
		"base64": {"Content-Transfer-Encoding: base64\r\n", base64.StdEncoding.EncodeToString([]byte("contenu"))},
		"BASE64": {"Content-Transfer-Encoding: BASE64\r\n", base64.StdEncoding.EncodeToString([]byte("contenu"))},
		"7BIT":   {"Content-Transfer-Encoding: 7BIT\r\n", "contenu"},
	} {
		for _, contentType := range []string{"text/plain", "application/pdf", "message/rfc822"} {
			t.Run(name+"/"+contentType, func(t *testing.T) {
				raw := parseHeader + "Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n--b\r\n" +
					"Content-Type: text/plain; charset=utf-8\r\n\r\nvoir la pièce jointe\r\n--b\r\n" +
					"Content-Type: " + contentType + "\r\nContent-Disposition: attachment; filename=\"=?UTF-8?Q?r=C3=A9sum=C3=A9.txt?=\"\r\n" +
					part.header + "\r\n" + part.body + "\r\n--b--\r\n"
				email, err := parseEmail(strings.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if email.TextBody != "voir la pièce jointe" {
					t.Errorf("body: %q", email.TextBody)
				}
				if len(email.Attachments) != 1 {
					t.Fatalf("attachments: %+v", email.Attachments)
				}
				a := email.Attachments[0]
				content, _ := io.ReadAll(a.Data)
				if strings.TrimRight(string(content), "\r\n") != "contenu" || a.Filename != "résumé.txt" || a.ContentType != contentType {
					t.Errorf("attachment: %q, %+v", content, a)
				}
			})
		}
	}
}

// The kinds of parts: a named text part sent inline is a file, an image with a
// Content-ID is embedded, an unnamed part of another type is still kept, and a
// nested tree is walked.
func TestParseEmailParts(t *testing.T) {
	raw := parseHeader + "Content-Type: multipart/mixed; boundary=\"m\"\r\n\r\n--m\r\n" +
		"Content-Type: multipart/related; boundary=\"r\"\r\n\r\n--r\r\n" +
		"Content-Type: multipart/alternative; boundary=\"a\"\r\n\r\n--a\r\nContent-Type: text/plain\r\n\r\nle texte\r\n--a\r\n" +
		"Content-Type: text/html\r\n\r\n<p>le texte</p>\r\n--a--\r\n--r\r\n" +
		"Content-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Disposition: inline; filename=\"logo.png\"\r\n\r\nPNG\r\n--r--\r\n--m\r\n" +
		"Content-Type: text/plain; name=\"note.txt\"\r\nContent-Disposition: inline\r\n\r\nune note\r\n--m\r\n" +
		"Content-Type: application/zip\r\n\r\nZIP\r\n--m--\r\n"
	email, err := parseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if email.TextBody != "le texte" || email.HTMLBody != "<p>le texte</p>" {
		t.Errorf("bodies: %q, %q", email.TextBody, email.HTMLBody)
	}
	if len(email.EmbeddedFiles) != 1 || email.EmbeddedFiles[0].CID != "logo@x" {
		t.Errorf("embedded: %+v", email.EmbeddedFiles)
	}
	if len(email.Attachments) != 2 || email.Attachments[0].Filename != "note.txt" || email.Attachments[1].ContentType != "application/zip" {
		t.Errorf("attachments: %+v", email.Attachments)
	}
}

// A damaged or unusual message is read as far as possible rather than lost.
func TestParseEmailDamaged(t *testing.T) {
	for name, c := range map[string]struct{ raw, body string }{
		"unknown encoding": {parseHeader + "Content-Type: text/plain\r\nContent-Transfer-Encoding: x-maison\r\n\r\ntexte\r\n", "texte"},
		"unknown charset":  {parseHeader + "Content-Type: text/plain; charset=x-maison\r\n\r\ntexte\r\n", "texte"},
		"bad base64":       {parseHeader + "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\ndGV4dGU=!!!\r\n", "texte"},
		"no closing":       {parseHeader + "Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\ntexte\r\n", ""},
		"bad content type": {parseHeader + "Content-Type: ;;;\r\n\r\ntexte\r\n", "texte"},
		"folded type":      {parseHeader + "Content-Type: text/plain;\r\n\tcharset=utf-8\r\n\r\ntexte\r\n", "texte"},
		"bare LF":          {strings.ReplaceAll(parseHeader+"Content-Type: text/plain\r\n\r\ntexte\r\n", "\r\n", "\n"), "texte"},
		"no recipient":     {"To: undisclosed-recipients:;\r\n" + parseHeader + "\r\ntexte\r\n", "texte"},
	} {
		t.Run(name, func(t *testing.T) {
			email, err := parseEmail(strings.NewReader(c.raw))
			if err != nil {
				t.Fatal(err)
			}
			if email.MessageID != "id@example.org" {
				t.Errorf("header: %+v", email)
			}
			if c.body != "" && email.TextBody != c.body {
				t.Errorf("body: %q", email.TextBody)
			}
			t.Logf("body: %q", email.TextBody)
		})
	}
}

// The main part is the reply alone; the full text keeps quotations and the
// content of a forwarded email, which the reply parser cuts.
func TestFullTextPart(t *testing.T) {
	ctx := context.Background()
	provider := NewProvider()
	defer provider.releaseAll()

	full := "Peux-tu regarder ?\n\n---------- Forwarded message ---------\nFrom: Client <c@example.org>\n" +
		"Date: Mon, Nov 9, 2026\nSubject: Devis\nTo: Moi <m@example.org>\n\nIl nous faudrait un devis."
	message := provider.toMessage(&parsemail.Email{MessageID: "id@example.org", TextBody: full})

	var found string
	for _, attachment := range courier.Attachments(message) {
		if attachment.Name() == FullTextPartName {
			content, err := courier.ReadPart(ctx, attachment)
			if err != nil {
				t.Fatal(err)
			}
			found = string(content)
			if attachment.Disposition() != courier.DispositionInline {
				t.Errorf("disposition: %q", attachment.Disposition())
			}
		}
	}
	if found != full {
		t.Errorf("full text: %q", found)
	}

	if empty := provider.toMessage(&parsemail.Email{MessageID: "id2@example.org", HTMLBody: "<p>x</p>"}); slices.ContainsFunc(
		courier.Attachments(empty), func(a courier.Attachment) bool { return a.Name() == FullTextPartName }) {
		t.Error("full text part without a text body")
	}
}
