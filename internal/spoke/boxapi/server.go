package boxapi

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
)

// Server serves the box-port API for one box on a unix socket.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// ServeUnix creates (replacing any stale socket file) and serves the box-port
// API for one box at path, returning immediately; the caller stops it with
// Close. The socket is made group/other-accessible for the same reason as the
// guest control socket travelling the other way: across the container
// boundary the connecting peer runs as a different uid, and connect() needs
// write permission on the socket — the 0700 parent directory (the box's
// private socket dir) remains the access gate on the host side.
//
// @arg path The filesystem path of the socket to create.
// @arg boxID The identity of the box this socket serves; every request is stamped with it.
// @arg svc The service forwarding requests to the hub.
// @arg log Logger for serve-loop failures; nil uses slog.Default.
// @return *Server The serving listener; the caller must Close it.
// @error error if the socket cannot be created or its mode cannot be set.
//
// @testcase TestBoxAPIOpenPort serves requests over a socket created by ServeUnix.
// @testcase TestServeUnixReplacesStaleSocket replaces a stale socket file at the same path.
func ServeUnix(path, boxID string, svc PortService, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	// Remove a stale socket left by a previous run so bind succeeds on restart.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: NewHandler(boxID, svc)}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("box-port API server exited", "box", boxID, "path", path, "err", err)
		}
	}()
	return &Server{ln: ln, srv: srv}, nil
}

// Close stops the server, closing the listener and any in-flight connections.
// The listener is also closed directly: http.Server.Close only closes
// listeners Serve has already registered, and the serving goroutine may not
// have been scheduled yet when Close runs — without the direct close, the
// socket would keep accepting until that goroutine finally starts. Closing
// more than once is harmless.
//
// @error error if closing the underlying server fails.
//
// @testcase TestServeUnixCloseStopsServing refuses connections after Close, even when the serve goroutine had not started.
func (s *Server) Close() error {
	err := s.srv.Close()
	// Backstop for the not-yet-tracked listener; a second close of an already
	// closed listener just returns an error we ignore.
	_ = s.ln.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
