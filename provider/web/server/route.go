package server

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

const (
	channelID = "web"
)

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if err := templates.ExecuteTemplate(w, "index", nil); err != nil {
		slog.ErrorContext(r.Context(), "could not execute template", slog.Any("error", errors.WithStack(err)))
	}
}

func (s *Server) serveMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher := w.(http.Flusher)

	flusher.Flush()

	var buff bytes.Buffer
	writeMessage := func(message courier.Message) error {
		if err := templates.ExecuteTemplate(&buff, "message", message); err != nil {
			return errors.WithStack(err)
		}

		if _, err := fmt.Fprintf(w, "event: message\n"); err != nil {
			return errors.WithStack(err)
		}

		if _, err := fmt.Fprintf(w, "id: %s\n", message.ID()); err != nil {
			return errors.WithStack(err)
		}

		scanner := bufio.NewScanner(&buff)

		for scanner.Scan() {
			if _, err := fmt.Fprintf(w, "data: %s\n", scanner.Text()); err != nil {
				return errors.WithStack(err)
			}
		}

		if _, err := fmt.Fprintf(w, "\n\n"); err != nil {
			return errors.WithStack(err)
		}

		flusher.Flush()
		buff.Reset()

		return nil
	}

	s.mutex.RLock()
	prevTotal := len(s.messages)

	for _, m := range s.messages {
		if err := writeMessage(m); err != nil {
			slog.ErrorContext(r.Context(), "could not write message", slog.Any("error", errors.WithStack(err)))
			return
		}
	}

	s.mutex.RUnlock()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C

		s.mutex.RLock()
		currTotal := len(s.messages)
		if changed := prevTotal != currTotal; !changed {
			s.mutex.RUnlock()
			continue
		}

		lasts := s.messages[prevTotal:]
		s.mutex.RUnlock()

		for _, l := range lasts {
			if err := writeMessage(l); err != nil {
				slog.ErrorContext(r.Context(), "could not write message", slog.Any("error", errors.WithStack(err)))
				return
			}
		}

		prevTotal = currTotal
	}
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		slog.ErrorContext(ctx, "could not parse form", slog.Any("error", errors.WithStack(err)))
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	text := r.Form.Get("message")
	if text == "" {
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	message := courier.NewMessage(
		courier.MessageID(xid.New().String()),
		courier.ChannelID("web"),
		courier.NewUser("user", "User"),
		courier.WithMessageMainPart(text),
	)

	s.messages = append(s.messages, message)

	s.send <- message
}
