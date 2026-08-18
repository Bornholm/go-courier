package rest

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/pkg/errors"
)

// ErrUnauthorized is returned by an Authenticator rejecting a request.
var ErrUnauthorized = errors.New("unauthorized")

// server exposes the provider over HTTP.
type server struct {
	opts *Options
	http *http.Server
	hub  *hub

	// incoming carries the messages posted by clients towards Listen.
	incoming chan courier.Message

	// parts indexes the parts of every message reachable through the
	// download endpoint, both incoming and outgoing ones.
	parts syncx.Map[string, courier.MessagePart]

	// released holds the cleanup functions of buffered uploads.
	releaseMutex sync.Mutex
	release      []courier.CloseFunc
}

func newServer(opts *Options, incoming chan courier.Message) *server {
	s := &server{
		opts:     opts,
		hub:      newHub(opts.HistorySize, opts.SubscriberBufferSize),
		incoming: incoming,
		release:  []courier.CloseFunc{},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /channels/{channelID}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /channels/{channelID}/events", s.handleEvents)
	mux.HandleFunc("GET /channels/{channelID}", s.handleGetChannel)
	mux.HandleFunc("GET /messages/{messageID}/parts/{partName}", s.handleGetPart)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("OPTIONS /", s.handlePreflight)

	s.http = &http.Server{
		Addr:    opts.Address,
		Handler: s.withCORS(mux),
	}

	return s
}

func (s *server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "could not shutdown server properly", slog.Any("error", errors.WithStack(err)))
		}

		s.hub.close()
		s.releaseAll()
	}()

	slog.DebugContext(ctx, "listening", slog.String("addr", s.http.Addr))

	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.WithStack(err)
	}

	return nil
}

// publish hands an outgoing message to the subscribers of its channel and
// indexes its parts so they can be downloaded.
func (s *server) publish(message courier.Message) {
	s.indexParts(message)

	channelID := courier.ChannelID("")
	if channel := message.Channel(); channel != nil {
		channelID = channel.ChannelID()
	}

	s.hub.publish(channelID, message)
}

func (s *server) indexParts(message courier.Message) {
	for _, part := range message.Parts() {
		s.parts.Store(partKey(message.ID(), part.Name()), part)
	}
}

func (s *server) trackRelease(release courier.CloseFunc) {
	s.releaseMutex.Lock()
	defer s.releaseMutex.Unlock()

	s.release = append(s.release, release)
}

func (s *server) releaseAll() {
	s.releaseMutex.Lock()
	release := s.release
	s.release = nil
	s.releaseMutex.Unlock()

	for _, fn := range release {
		if err := fn(); err != nil {
			slog.Error("could not release part content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeCORS(w, r)
		next.ServeHTTP(w, r)
	})
}

func (s *server) writeCORS(w http.ResponseWriter, r *http.Request) {
	if len(s.opts.CORSOrigins) == 0 {
		return
	}

	origin := r.Header.Get("Origin")

	for _, allowed := range s.opts.CORSOrigins {
		if allowed != "*" && allowed != origin {
			continue
		}

		if allowed == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Type, Content-Disposition")

		return
	}
}

func partKey(messageID courier.MessageID, partName string) string {
	return string(messageID) + "\x00" + partName
}

func urlEscape(value string) string {
	return url.PathEscape(value)
}

// bearerToken extracts the token of an "Authorization: Bearer <token>"
// header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")

	token, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return "", false
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}

	return token, true
}
