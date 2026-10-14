package daemon

import "net/http"

// Handler exposes the private control API for trusted in-process adapters.
// Callers must authenticate and validate browser input before dispatching here;
// this handler must never be mounted directly on a public HTTP listener.
func (s *Server) Handler() http.Handler { return s.http.Handler }
