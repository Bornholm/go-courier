package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// keepAliveInterval bounds how long an idle SSE connection stays silent, so
// that proxies do not drop it.
const keepAliveInterval = 30 * time.Second

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleGetChannel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	channel := s.opts.ResolveChannel(courier.ChannelID(r.PathValue("channelID")))

	writeJSON(w, http.StatusOK, toChannelDTO(channel))
}

// handlePostMessage accepts an incoming message and pushes it onto the
// channel returned by Listen.
func (s *server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	from, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	channel := s.opts.ResolveChannel(courier.ChannelID(r.PathValue("channelID")))

	incoming, files, err := s.parseIncoming(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		slog.DebugContext(ctx, "could not parse incoming message", slog.Any("error", err))
		writeError(w, status, err)

		return
	}

	contentType := incoming.ContentType
	if contentType == "" {
		contentType = courier.TextPlain
	}

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageMainPartOfType(incoming.Content, contentType),
		courier.WithMessageSentAt(time.Now()),
	}

	if incoming.InReplyTo != "" {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(incoming.InReplyTo)))
	}

	if len(incoming.Mentions) > 0 {
		funcs = append(funcs, courier.WithMessageMentions(toMentions(incoming.Mentions)...))
	}

	for _, file := range files {
		funcs = append(funcs, courier.WithMessagePart(file))
	}

	message := courier.NewMessage(courier.RandomMessageID(), channel, from, funcs...)

	s.indexParts(message)

	select {
	case s.incoming <- message:
	case <-ctx.Done():
		writeError(w, http.StatusRequestTimeout, ctx.Err())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": string(message.ID())})
}

// ErrUploadTooLarge is returned when an uploaded file exceeds MaxUploadSize.
var ErrUploadTooLarge = errors.New("upload too large")

// parseIncoming reads the multipart body: a "message" field holding the JSON
// payload, plus any number of file fields.
func (s *server) parseIncoming(r *http.Request) (*IncomingMessageDTO, []courier.MessagePart, error) {
	incoming := &IncomingMessageDTO{}

	reader, err := r.MultipartReader()
	if err != nil {
		// Not a multipart request: accept a plain JSON body, which is enough
		// for text only messages.
		if err := json.NewDecoder(r.Body).Decode(incoming); err != nil {
			return nil, nil, errors.Wrap(err, "could not decode message body")
		}

		return incoming, nil, nil
	}

	parts := make([]courier.MessagePart, 0)
	index := 0

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}

		if part.FileName() == "" {
			if part.FormName() == "message" {
				if err := json.NewDecoder(part).Decode(incoming); err != nil {
					part.Close()
					return nil, nil, errors.Wrap(err, "could not decode message field")
				}
			}

			part.Close()

			continue
		}

		attachment, err := s.readUpload(part, index)
		part.Close()

		if err != nil {
			return nil, nil, errors.WithStack(err)
		}

		parts = append(parts, attachment)
		index++
	}

	return incoming, parts, nil
}

// readUpload materializes an uploaded file, since the request body is gone by
// the time the application reads the message.
func (s *server) readUpload(part *multipart.Part, index int) (courier.Attachment, error) {
	contentType := uploadContentType(part)

	// Read one byte past the limit to tell a file at the limit from one over
	// it.
	limited := io.LimitReader(part, s.opts.MaxUploadSize+1)

	open, release := courier.BufferedOpener(courier.OpenerOnce(io.NopCloser(limited)), s.opts.MaxInMemorySize)

	content, err := open(context.Background())
	if err != nil {
		release()
		return nil, errors.WithStack(err)
	}

	size, err := io.Copy(io.Discard, content)
	content.Close()

	if err != nil {
		release()
		return nil, errors.WithStack(err)
	}

	if size > s.opts.MaxUploadSize {
		release()
		return nil, errors.Wrapf(ErrUploadTooLarge, "file %q exceeds the %d bytes limit", part.FileName(), s.opts.MaxUploadSize)
	}

	s.trackRelease(release)

	return courier.NewAttachment(
		part.FileName(), contentType, open,
		courier.WithAttachmentName(fmt.Sprintf("att-%d", index)),
		courier.WithAttachmentSize(size),
	), nil
}

// uploadContentType resolves the content type of an uploaded file. Clients
// commonly send the generic application/octet-stream — curl does by default —
// which would classify a voice note as an opaque blob, so the filename
// extension gets the last word in that case.
func uploadContentType(part *multipart.Part) string {
	declared := part.Header.Get("Content-Type")

	if declared != "" && declared != "application/octet-stream" {
		return declared
	}

	guessed := mime.TypeByExtension(filepath.Ext(part.FileName()))
	if guessed != "" {
		return guessed
	}

	if declared != "" {
		return declared
	}

	return "application/octet-stream"
}

// handleEvents streams the messages sent to a channel as server sent events.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}

	channelID := courier.ChannelID(r.PathValue("channelID"))

	sub, backlog := s.hub.subscribe(channelID, courier.MessageID(r.Header.Get("Last-Event-ID")))
	defer s.hub.unsubscribe(channelID, sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher.Flush()

	for _, message := range backlog {
		if err := s.writeEvent(ctx, w, message); err != nil {
			slog.DebugContext(ctx, "could not write backlog event", slog.Any("error", err))
			return
		}
	}

	flusher.Flush()

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case message, ok := <-sub.messages:
			if !ok {
				// Disconnected by the hub, most likely after falling behind.
				return
			}

			if err := s.writeEvent(ctx, w, message); err != nil {
				slog.DebugContext(ctx, "could not write event", slog.Any("error", err))
				return
			}

			flusher.Flush()

		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

func (s *server) writeEvent(ctx context.Context, w io.Writer, message courier.Message) error {
	dto, err := toMessageDTO(ctx, message, s.opts.InlineTextLimit)
	if err != nil {
		return errors.WithStack(err)
	}

	payload, err := json.Marshal(dto)
	if err != nil {
		return errors.WithStack(err)
	}

	if _, err := fmt.Fprintf(w, "event: message\nid: %s\ndata: %s\n\n", message.ID(), payload); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// handleGetPart serves the raw content of a message part.
func (s *server) handleGetPart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	messageID := courier.MessageID(r.PathValue("messageID"))
	partName := r.PathValue("partName")

	part, exists := s.parts.Load(partKey(messageID, partName))
	if !exists {
		writeError(w, http.StatusNotFound, errors.WithStack(courier.ErrNotFound))
		return
	}

	reader, err := part.Reader(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not open part", slog.Any("error", errors.WithStack(err)))
		writeError(w, http.StatusInternalServerError, err)

		return
	}

	defer reader.Close()

	w.Header().Set("Content-Type", part.ContentType())

	if attachment, ok := part.(courier.Attachment); ok {
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			"%s; filename=%q", attachment.Disposition(), courier.FilenameFor(attachment),
		))

		if size := attachment.Size(); size >= 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		}
	}

	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		slog.DebugContext(ctx, "could not write part content", slog.Any("error", errors.WithStack(err)))
	}
}

// authenticate resolves the user behind the request, answering 401 and
// returning false when it cannot.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (courier.User, bool) {
	user, err := s.opts.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.WithStack(ErrUnauthorized))
		return nil, false
	}

	if user == nil {
		writeError(w, http.StatusUnauthorized, errors.WithStack(ErrUnauthorized))
		return nil, false
	}

	return user, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("could not encode response", slog.Any("error", errors.WithStack(err)))
	}
}

// writeError answers with the status text only: error details stay in the
// logs rather than leaking to clients.
func writeError(w http.ResponseWriter, status int, err error) {
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", slog.Int("status", status), slog.Any("error", err))
	}

	writeJSON(w, status, ErrorDTO{Error: http.StatusText(status)})
}
