package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/gataky/afto/daemon/internal/provider"
)

// Handler is the daemon core the server dispatches to. The split exists so
// the transport (this package) knows nothing about providers, scoring, or
// storage — M5 wires the real engine in; tests substitute fakes.
//
// Suggest returns ranked candidates (never an error: an empty slice IS the
// error representation on the wire — the client just shows nothing).
// Record ingests a finished command and must be cheap; it is called inline
// from the connection's read loop.
type Handler interface {
	Suggest(ctx context.Context, q provider.Query) []provider.Candidate
	Record(q Request)
	Version() string
}

// Server reads JSON-lines requests from unix-socket connections and writes
// responses. Malformed or unknown messages are logged and dropped without
// closing the connection (plans/phase-1.md §5).
//
// Connection model: one goroutine per connection, requests on a connection
// are handled sequentially in arrival order. That matches the clients: the
// zsh plugin keeps exactly one request in flight per shell, so per-
// connection concurrency would buy nothing. Different shells are different
// connections and proceed in parallel.
//
// ActiveConns/LastActivity exist for the daemon's idle-shutdown loop: a
// persistent shell connection counts as activity, so the daemon only exits
// when every terminal using it has closed and the idle window has passed.
type Server struct {
	h   Handler
	log *slog.Logger

	active       atomic.Int64
	lastActivity atomic.Int64 // UnixNano
}

func NewServer(h Handler, log *slog.Logger) *Server {
	s := &Server{h: h, log: log}
	s.touch()
	return s
}

// ActiveConns reports currently open client connections.
func (s *Server) ActiveConns() int64 { return s.active.Load() }

// LastActivity reports the time of the most recent request or connection
// event; used by the daemon's idle-shutdown loop.
func (s *Server) LastActivity() time.Time { return time.Unix(0, s.lastActivity.Load()) }

func (s *Server) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	s.active.Add(1)
	s.touch()
	defer func() {
		s.active.Add(-1)
		s.touch()
		_ = conn.Close()
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		s.touch()
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			s.log.Debug("malformed request dropped", "err", err)
			continue
		}
		switch req.Type {
		case TypeSuggest:
			s.suggest(conn, req)
		case TypeRecord:
			s.h.Record(req)
		case TypePing:
			s.reply(conn, PingResponse{V: V, OK: true, Version: s.h.Version()})
		default:
			s.log.Debug("unknown request type dropped", "type", req.Type)
		}
	}
	if err := sc.Err(); err != nil {
		s.log.Debug("connection read ended", "err", err)
	}
}

func (s *Server) suggest(conn net.Conn, req Request) {
	start := time.Now()
	cands := s.h.Suggest(context.Background(), req.Query())
	s.log.Debug("suggest handled", "id", req.ID, "buffer_len", len(req.Buffer),
		"candidates", len(cands), "took", time.Since(start))

	if req.Fmt == FmtTSV {
		text := ""
		if len(cands) > 0 {
			text = cands[0].Text
		}
		if _, err := conn.Write(EncodeTSV(req.ID, text)); err != nil {
			s.log.Debug("write failed", "err", err)
		}
		return
	}
	s.reply(conn, SuggestResponse{V: V, ID: req.ID, Candidates: toWire(cands)})
}

func (s *Server) reply(conn net.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Error("marshal response", "err", err)
		return
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		s.log.Debug("write failed", "err", err)
	}
}
