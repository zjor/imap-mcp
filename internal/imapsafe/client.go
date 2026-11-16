// Package imapsafe is the only package that talks IMAP. It exposes only the
// operations the server allows (spec 7.3).
package imapsafe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zjor/imap-mcp/internal/logging"
)

const (
	defaultTimeout     = 30 * time.Second
	maxSessionLifetime = 10 * time.Minute
	slowWait           = 5 * time.Second
)

// ErrAuth means the IMAP server rejected the username or password.
var ErrAuth = errors.New("imap login failed: wrong username or password")

// Credentials are the IMAP login data of one request. Host is "host:port".
// Every text form (String, GoString, fmt verbs, slog) omits the password.
type Credentials struct {
	Username, Password, Host string
}

// String returns the username only.
func (c Credentials) String() string { return c.Username }

// GoString omits the password.
func (c Credentials) GoString() string {
	return fmt.Sprintf("imapsafe.Credentials{Username:%q, Host:%q}", c.Username, c.Host)
}

// Format makes every fmt verb (including %+v and %#v) safe.
func (c Credentials) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'v' && f.Flag('#'):
		io.WriteString(f, c.GoString())
	case verb == 'q':
		fmt.Fprintf(f, "%q", c.Username)
	default:
		io.WriteString(f, c.Username)
	}
}

// LogValue returns the username only.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.Username) }

// Caps are the server capabilities the tools care about.
type Caps struct {
	UIDPlus, Thread, Sort, Binary bool
	AppendLimit                   int64 // 0 when the server reports none
}

// Dialer opens IMAP sessions. A nil TLSConfig means system roots.
type Dialer struct {
	TLSConfig                    *tls.Config
	Limiter                      *Limiter
	DraftsOverride, SentOverride string
	// Timeout covers dial, TLS, login and the setup commands (default 30 s).
	Timeout time.Duration
}

// Session is one logged-in IMAP connection. Close it when done.
type Session struct {
	c       *imapclient.Client
	caps    Caps
	folders SpecialFolders
	stop    func() // stops the lifetime watchers
	release func() // frees the limiter slot
	once    sync.Once
}

// Caps returns the server capabilities read after login.
func (s *Session) Caps() Caps { return s.caps }

// Folders returns the resolved special-use folders.
func (s *Session) Folders() SpecialFolders { return s.folders }

// Close logs out, closes the connection and frees the limiter slot.
func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		s.stop()
		_ = s.c.Logout().Wait()
		err = s.c.Close()
		s.release()
	})
	return err
}

// Open waits for a limiter slot, connects with TLS (port 993) or STARTTLS
// (port 143), logs in, reads capabilities and resolves special folders.
// A rejected login returns ErrAuth.
func (d *Dialer) Open(ctx context.Context, c Credentials) (*Session, error) {
	log := logging.From(ctx)
	release := func() {}
	if d.Limiter != nil {
		rel, waited, err := d.Limiter.Acquire(ctx, c.Username)
		if err != nil {
			return nil, err
		}
		if waited > slowWait {
			log.Warn("imap.session_wait", "waited_ms", waited.Milliseconds())
		}
		release = rel
	}
	s, err := d.open(ctx, log, c)
	if err != nil {
		release()
		return nil, err
	}
	s.release = release
	return s, nil
}

func (d *Dialer) open(ctx context.Context, log *slog.Logger, c Credentials) (*Session, error) {
	host, port, err := net.SplitHostPort(c.Host)
	if err != nil {
		return nil, fmt.Errorf("imap host %q: %w", c.Host, err)
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	setupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var nd net.Dialer
	conn, err := nd.DialContext(setupCtx, "tcp", c.Host)
	if err != nil {
		return nil, fmt.Errorf("imap dial: %w", err)
	}
	return d.setup(ctx, setupCtx, log, conn, port == "143", host, c)
}

// setup runs TLS, login, CAPABILITY and LIST on a connected conn. startTLS
// selects STARTTLS instead of implicit TLS. setupCtx bounds the whole setup;
// ctx bounds the session lifetime.
func (d *Dialer) setup(ctx, setupCtx context.Context, log *slog.Logger, conn net.Conn, startTLS bool, host string, c Credentials) (*Session, error) {
	// When the setup budget runs out, close the connection so no step hangs.
	stopSetup := context.AfterFunc(setupCtx, func() { conn.Close() })
	fail := func(cl *imapclient.Client, err error) (*Session, error) {
		if cl != nil {
			cl.Close()
		}
		conn.Close()
		if setupCtx.Err() != nil && !errors.Is(err, ErrAuth) {
			return nil, fmt.Errorf("imap connect: %w", setupCtx.Err())
		}
		return nil, err
	}

	cl, err := newClient(setupCtx, conn, startTLS, d.tlsConfig(host))
	if err != nil {
		return fail(nil, err)
	}
	if err := cl.Login(c.Username, c.Password).Wait(); err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo {
			log.Warn("imap.login_failed", "code", string(ie.Code))
			return fail(cl, ErrAuth)
		}
		return fail(cl, fmt.Errorf("imap login: %w", err))
	}
	capSet, err := cl.Capability().Wait()
	if err != nil {
		return fail(cl, fmt.Errorf("imap capability: %w", err))
	}
	log.Debug("imap.capabilities", "caps", capList(capSet))
	folders, err := resolveFolders(cl, capSet, d.DraftsOverride, d.SentOverride)
	if err != nil {
		return fail(cl, err)
	}
	if !stopSetup() {
		return fail(cl, errors.New("imap setup timed out"))
	}

	// Overall session deadline: close the connection when the request ends
	// or after maxSessionLifetime. The library keeps its own per-command
	// read and write timeouts on top of this.
	timer := time.AfterFunc(maxSessionLifetime, func() { cl.Close() })
	stopCtx := context.AfterFunc(ctx, func() { cl.Close() })

	return &Session{
		c:       cl,
		caps:    toCaps(capSet),
		folders: folders,
		stop:    func() { timer.Stop(); stopCtx() },
	}, nil
}

// newClient starts TLS: STARTTLS when startTLS is set (an error when the
// server refuses it), implicit TLS otherwise. It never returns a client on a
// plain-text connection.
func newClient(ctx context.Context, conn net.Conn, startTLS bool, cfg *tls.Config) (*imapclient.Client, error) {
	opts := &imapclient.Options{TLSConfig: cfg}
	if startTLS {
		cl, err := imapclient.NewStartTLS(conn, opts)
		if err != nil {
			return nil, fmt.Errorf("imap starttls failed, refusing plain IMAP: %w", err)
		}
		return cl, nil
	}
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("imap tls: %w", err)
	}
	cl := imapclient.New(tc, opts)
	if err := cl.WaitGreeting(); err != nil {
		cl.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
	return cl, nil
}

func (d *Dialer) tlsConfig(host string) *tls.Config {
	cfg := &tls.Config{}
	if d.TLSConfig != nil {
		cfg = d.TLSConfig.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.NextProtos == nil {
		cfg.NextProtos = []string{"imap"}
	}
	return cfg
}

func toCaps(set imap.CapSet) Caps {
	c := Caps{
		UIDPlus: set.Has(imap.CapUIDPlus),
		Thread:  len(set.ThreadAlgorithms()) > 0,
		Sort:    set.Has(imap.CapSort),
		Binary:  set.Has(imap.CapBinary),
	}
	if limit, ok := set.AppendLimit(); ok && limit != nil {
		c.AppendLimit = int64(*limit)
	}
	return c
}

func capList(set imap.CapSet) []string {
	l := make([]string, 0, len(set))
	for c := range set {
		l = append(l, string(c))
	}
	slices.Sort(l)
	return l
}
