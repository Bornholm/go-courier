package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

type Server struct {
	http *http.Server
	send chan courier.Message

	mutex sync.RWMutex

	messages []courier.Message
}

func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "could not shutdown server properly", slog.Any("error", errors.WithStack(err)))
		}
	}()

	slog.DebugContext(ctx, "listening", slog.String("addr", s.http.Addr))

	if err := s.http.ListenAndServe(); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func New(addr string, send chan courier.Message) *Server {
	server := &Server{
		http: &http.Server{
			Addr: addr,
		},
		send:     send,
		messages: []courier.Message{},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", server.serveIndex)
	mux.HandleFunc("POST /send", server.sendMessage)
	mux.HandleFunc("GET /messages", server.serveMessages)

	server.http.Handler = mux

	return server
}
