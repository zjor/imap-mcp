package imapsafe

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer is a plain-text IMAP server on one side of a net.Pipe. It never
// does TLS. It answers STARTTLS with starttlsReply and records every byte the
// client writes.
func fakeServer(t *testing.T, srv net.Conn, greeting, starttlsReply string) <-chan string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		defer srv.Close()
		var rec bytes.Buffer
		defer func() { got <- rec.String() }()
		if _, err := io.WriteString(srv, greeting+"\r\n"); err != nil {
			return
		}
		r := bufio.NewReader(io.TeeReader(srv, &rec))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, cmd, _ := strings.Cut(strings.TrimSpace(line), " ")
			var reply string
			switch strings.ToUpper(cmd) {
			case "STARTTLS":
				reply = tag + " " + starttlsReply
			case "CAPABILITY":
				reply = "* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\n" + tag + " OK done"
			default:
				// Accept anything else, so a client that skips TLS would
				// log in successfully and the test would catch it.
				reply = tag + " OK done"
			}
			if _, err := io.WriteString(srv, reply+"\r\n"); err != nil {
				return
			}
		}
	}()
	return got
}

func TestSetup_StartTLSRefused_NoLogin(t *testing.T) {
	cases := map[string]struct{ greeting, reply string }{
		"not offered": {
			greeting: "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready",
			reply:    "BAD unknown command",
		},
		"offered but NO": {
			greeting: "* OK [CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN] ready",
			reply:    "NO not now",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cli, srv := net.Pipe()
			written := fakeServer(t, srv, tc.greeting, tc.reply)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			d := &Dialer{}
			s, err := d.setup(ctx, ctx, log, cli, true, "imap.example.com",
				Credentials{Username: "alice", Password: "secret-pass", Host: "imap.example.com:143"})
			if err == nil {
				t.Errorf("setup succeeded without TLS")
				s.c.Close()
			} else if !strings.Contains(err.Error(), "starttls") {
				t.Errorf("err = %v, want a starttls error", err)
			}

			var out string
			select {
			case out = <-written:
			case <-time.After(5 * time.Second):
				t.Fatal("client did not close the connection")
			}
			if !strings.Contains(strings.ToUpper(out), "STARTTLS") {
				t.Errorf("client never sent STARTTLS: %q", out)
			}
			if strings.Contains(strings.ToUpper(out), "LOGIN") || strings.Contains(out, "secret-pass") {
				t.Fatalf("client sent LOGIN over plain text: %q", out)
			}
		})
	}
}
