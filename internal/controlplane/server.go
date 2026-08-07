// server.go — the loopback HTTP server (U2, R20). Binds 127.0.0.1 by
// default; a non-loopback bind is refused unless AllowNonLoopback is set
// explicitly. The per-process UI token is minted here at construction and
// exposed for the embedded UI (U8) to present on state-changing requests.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultListenAddress is the loopback default (R20).
const DefaultListenAddress = "127.0.0.1:8383"

// ServerConfig configures one control-plane server.
type ServerConfig struct {
	// Address is the listen address; empty means DefaultListenAddress.
	Address string
	// AllowNonLoopback is the explicit R20 opt-in. Without it a non-loopback
	// address refuses to construct, let alone bind.
	AllowNonLoopback bool
	Logger           *slog.Logger
}

// Server owns the HTTP listener and the sweeper for one Store.
type Server struct {
	store    *Store
	sweeper  *Sweeper
	http     *http.Server
	uiToken  string
	logger   *slog.Logger
	listener net.Listener
	stop     context.CancelFunc
}

// NewServer builds a server. The R20 bind refusal happens here — before any
// socket exists — so a misconfigured address never listens at all. ctx bounds
// the name resolution that refusal needs: a stalled resolver must not be able
// to hold startup open indefinitely.
func NewServer(ctx context.Context, store *Store, config ServerConfig) (*Server, error) {
	address := config.Address
	if address == "" {
		address = DefaultListenAddress
	}
	if !config.AllowNonLoopback {
		if err := validateLoopbackAddress(ctx, address); err != nil {
			return nil, fmt.Errorf(
				"refusing to bind %q: %w (set AllowNonLoopback to opt in explicitly)", address, err)
		}
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	token, err := newUIToken()
	if err != nil {
		return nil, fmt.Errorf("generate UI token: %w", err)
	}
	server := &Server{
		store:   store,
		sweeper: NewSweeper(store),
		uiToken: token,
		logger:  logger,
	}
	server.http = &http.Server{
		Addr:              address,
		Handler:           NewHandler(store, token, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	return server, nil
}

// UIToken is the per-process token the embedded UI presents on
// state-changing requests (R20).
func (s *Server) UIToken() string { return s.uiToken }

// Start binds the listener, launches the sweeper, and serves in the
// background until Shutdown. ctx bounds the BIND only — it is not the
// server's lifetime, which Shutdown ends: a caller passing a request-scoped
// context must not have its server torn down under it.
func (s *Server) Start(ctx context.Context) error {
	// Check the context ourselves rather than relying on the bind to do it.
	// ListenConfig.Listen consults the context only where it resolves a name,
	// so an address needing no resolution binds happily on a dead context —
	// and which addresses need resolution differs by platform and resolver.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("listen on %s: %w", s.http.Addr, err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.http.Addr, err)
	}
	s.listener = listener
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = cancel
	go s.sweeper.Run(serveCtx, s.logger)
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http_serve_failed", "error", err)
		}
	}()
	return nil
}

// Addr reports the bound address after Start (useful with a ":0" port).
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Shutdown stops the sweeper and drains the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.stop != nil {
		s.stop()
	}
	return s.http.Shutdown(ctx)
}

// validateLoopbackAddress accepts only addresses that cannot receive
// non-local traffic: a loopback IP literal, or "localhost" when every
// resolved address is loopback (R20). An empty host — which binds every
// interface — is refused. Both lookups run under the caller's context: the
// only host that reaches the resolver here is "localhost", and a resolver
// that cannot answer that promptly must fail the bind rather than hang it.
func validateLoopbackAddress(ctx context.Context, address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	if _, err := net.DefaultResolver.LookupPort(ctx, "tcp", port); err != nil {
		return fmt.Errorf("invalid listen port: %w", err)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return errors.New("an empty host binds every interface; name a loopback address")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return errors.New("listen host is not a loopback address")
		}
		return nil
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return errors.New("listen host must be a loopback IP or localhost")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("resolve listen host: %w", err)
	}
	for _, addr := range ips {
		if !addr.IP.IsLoopback() {
			return errors.New("listen host resolves outside loopback")
		}
	}
	return nil
}

func newUIToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
