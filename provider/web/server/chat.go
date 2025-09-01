package server

import "github.com/bornholm/go-courier"

func (s *Server) Send(message courier.Message) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.messages = append(s.messages, message)

	return nil
}
