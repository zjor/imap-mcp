package imapsafe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zjor/imap-mcp/internal/ids"
)

// partByte is byte i of the fake server's part 2 (decoded).
func partByte(i int) byte { return byte(i*13 + i/97) }

// fakeReadServer serves EXAMINE, UID FETCH BODYSTRUCTURE and
// UID FETCH BODY.PEEK[2] for UID 5 without the BINARY capability. Part 2 is
// size bytes in base64 (76-char lines), written to the wire in small chunks
// so the server itself never holds the encoded part. It returns every
// command line the client sent.
func fakeReadServer(t *testing.T, srv net.Conn, size int) func() []string {
	t.Helper()
	var (
		mu    sync.Mutex
		lines []string
		done  = make(chan struct{})
	)
	encLen := base64.StdEncoding.EncodedLen(size)
	litLen := encLen + 2*((encLen+75)/76) // CRLF after every line
	go func() {
		defer close(done)
		defer srv.Close()
		w := bufio.NewWriter(srv)
		fmt.Fprint(w, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		w.Flush()
		r := bufio.NewReader(srv)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
			tag, cmd, _ := strings.Cut(line, " ")
			switch {
			case strings.HasPrefix(cmd, "LIST"):
				// INBOX answers STATUS, Locked refuses it, Shared is \Noselect.
				fmt.Fprintf(w, "* LIST () \"/\" INBOX\r\n"+
					"* LIST () \"/\" Locked\r\n"+
					"* LIST (\\Noselect) \"/\" Shared\r\n%s OK done\r\n", tag)
			case strings.HasPrefix(cmd, "STATUS INBOX"):
				fmt.Fprintf(w, "* STATUS INBOX (MESSAGES 3 UNSEEN 1)\r\n%s OK done\r\n", tag)
			case strings.HasPrefix(cmd, "STATUS"):
				fmt.Fprintf(w, "%s NO [NOPERM] no read rights\r\n", tag)
			case strings.HasPrefix(cmd, "EXAMINE"):
				fmt.Fprintf(w, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] ok\r\n%s OK [READ-ONLY] done\r\n", tag)
			case strings.HasPrefix(cmd, "UID FETCH") && strings.Contains(cmd, "BODYSTRUCTURE"):
				fmt.Fprintf(w, `* 1 FETCH (UID 5 BODYSTRUCTURE (("text" "plain" ("charset" "utf-8") NIL NIL "7bit" 3 1 NIL NIL NIL NIL)`+
					`("application" "octet-stream" NIL NIL NIL "base64" %d NIL ("attachment" ("filename" "big.bin")) NIL NIL) "mixed" ("boundary" "b1") NIL NIL NIL))`+
					"\r\n%s OK done\r\n", litLen, tag)
			case strings.HasPrefix(cmd, "UID FETCH") && strings.Contains(cmd, "BODY.PEEK[2]"):
				fmt.Fprintf(w, "* 1 FETCH (UID 5 BODY[2] {%d}\r\n", litLen)
				chunk, line := make([]byte, 57), make([]byte, 76)
				for i := 0; i < size; i += 57 {
					k := min(57, size-i)
					for j := range k {
						chunk[j] = partByte(i + j)
					}
					n := base64.StdEncoding.EncodedLen(k)
					base64.StdEncoding.Encode(line, chunk[:k])
					w.Write(line[:n])
					w.WriteString("\r\n")
				}
				fmt.Fprintf(w, ")\r\n%s OK done\r\n", tag)
			case strings.HasPrefix(cmd, "LOGOUT"):
				fmt.Fprintf(w, "* BYE bye\r\n%s OK done\r\n", tag)
			default:
				// Accept anything else with no data, so every operation
				// reaches the wire and the caller can inspect it.
				fmt.Fprintf(w, "%s OK done\r\n", tag)
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	}()
	return func() []string {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("fake server did not stop")
		}
		mu.Lock()
		defer mu.Unlock()
		return lines
	}
}

func fakeSession(conn net.Conn) *Session {
	c := imapclient.New(conn, nil)
	return &Session{
		c:       c,
		w:       clientWire{c},
		stop:    func() {},
		release: func() {},
		log:     slog.New(slog.DiscardHandler),
	}
}

func TestFetchPart_BodyFallbackStreams20MB(t *testing.T) {
	const size = 20 << 20
	wantHash := sha256.New()
	for i := range size {
		wantHash.Write([]byte{partByte(i)})
	}
	want := wantHash.Sum(nil)
	cli, srv := net.Pipe()
	sent := fakeReadServer(t, srv, size)
	s := fakeSession(cli)
	ctx := context.Background()

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc

	id := ids.AttachmentID{MessageID: ids.MessageID{Folder: "INBOX", UIDValidity: 7, UID: 5}, Part: []int{2}}
	r, gotSize, err := s.FetchPart(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if gotSize != -1 {
		t.Errorf("size = %d, want -1 on the BODY.PEEK path", gotSize)
	}
	if _, err := s.Search(ctx, "INBOX", SearchCriteria{}, 0, 0); !errors.Is(err, errBusy) {
		t.Errorf("command during open reader: err = %v, want errBusy", err)
	}

	h := sha256.New()
	buf := make([]byte, 32<<10)
	var n, nextSample int64
	for {
		k, err := r.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if n >= nextSample {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
			nextSample = n + 1<<20
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", n, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("read=%d heap growth=%d KB", n, (peak-base)>>10)
	if n != size {
		t.Errorf("read %d bytes, want %d", n, size)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		t.Error("decoded content differs")
	}
	if growth := int64(peak) - int64(base); growth >= 8<<20 {
		t.Errorf("heap grew by %d bytes while streaming, want < 8 MB", growth)
	}

	// The session is usable again after Close.
	if _, err := s.Examine(ctx, "INBOX"); err != nil {
		t.Errorf("examine after close: %v", err)
	}
	s.Close()
	checkReadOnlyWire(t, sent())
}

// checkReadOnlyWire fails unless every command is on the read allowlist
// (spec 7.3) and every BODY/BINARY fetch item is .PEEK.
func checkReadOnlyWire(t *testing.T, lines []string) {
	t.Helper()
	allowed := []string{"EXAMINE ", "STATUS ", "LIST ", "UID SEARCH ", "UID FETCH ", "UID THREAD ", "LOGOUT"}
	peekless := regexp.MustCompile(`(?i)\b(BODY|BINARY)\[`)
	for _, l := range lines {
		_, cmd, _ := strings.Cut(l, " ")
		if !slices.ContainsFunc(allowed, func(p string) bool { return strings.HasPrefix(strings.ToUpper(cmd), p) }) {
			t.Errorf("command not on the read allowlist: %q", l)
		}
		if peekless.MatchString(cmd) {
			t.Errorf("fetch without .PEEK: %q", l)
		}
	}
}

func TestReadOps_WireIsReadOnly(t *testing.T) {
	cli, srv := net.Pipe()
	sent := fakeReadServer(t, srv, 100)
	s := fakeSession(cli)
	ctx := context.Background()
	msg := ids.MessageID{Folder: "INBOX", UIDValidity: 7, UID: 5}

	folders, err := s.ListFolders(ctx)
	if err != nil {
		t.Fatalf("one refused STATUS failed ListFolders: %v", err)
	}
	want := []Folder{
		{Name: "INBOX", Delimiter: "/", Role: "inbox", Total: 3, Unread: 1, CountsKnown: true},
		{Name: "Locked", Delimiter: "/"},
	}
	if !slices.Equal(folders, want) {
		t.Errorf("folders = %+v, want %+v", folders, want)
	}
	if _, err := s.Search(ctx, "INBOX", SearchCriteria{Subject: "x"}, 9, 5); err != nil {
		t.Error(err)
	}
	if _, err := s.SearchHeader(ctx, "INBOX", "Message-ID", "<a@b>"); err != nil {
		t.Error(err)
	}
	if _, err := s.FetchSummaries(ctx, "INBOX", []uint32{5, 6}); err != nil {
		t.Error(err)
	}
	if _, err := s.FetchRaw(ctx, msg); !errors.Is(err, ErrNotFound) {
		t.Errorf("FetchRaw err = %v, want ErrNotFound from the empty reply", err)
	}
	r, _, err := s.FetchPart(ctx, ids.AttachmentID{MessageID: msg, Part: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := s.Thread(ctx, "INBOX", 5); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Thread err = %v, want ErrUnsupported", err)
	}
	s.Close()

	lines := sent()
	for _, want := range []string{"LIST", "STATUS INBOX", `STATUS "Locked"`, "UID SEARCH", "BODY.PEEK[TEXT]", "BODY.PEEK[]", "BODY.PEEK[2]"} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, want) }) {
			t.Errorf("no command with %q in %q", want, lines)
		}
	}
	checkReadOnlyWire(t, lines)
	if slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "STATUS") && strings.Contains(l, "Shared") }) {
		t.Error("STATUS sent for a \\Noselect folder")
	}
}

func TestDecodeTransfer(t *testing.T) {
	cases := map[string]struct{ enc, in, want string }{
		"base64 with CRLF": {"base64", "aGVs\r\nbG8=\r\n", "hello"},
		"quoted-printable": {"quoted-printable", "caf=C3=A9 =\r\nau lait", "café au lait"},
		"7bit":             {"7bit", "plain", "plain"},
		"unknown":          {"x-uuencode", "raw", "raw"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := io.ReadAll(decodeTransfer(io.NopCloser(strings.NewReader(tc.in)), tc.enc))
			if err != nil || string(got) != tc.want {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDecodeParam(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"plain", map[string]string{"filename": "a.pdf"}, "a.pdf"},
		{"rfc2231", map[string]string{"filename*": "UTF-8''Faktura%20%C4%8D.pdf"}, "Faktura č.pdf"},
		{"continuation", map[string]string{"filename*0*": "UTF-8''Fak", "filename*1": "tura.pdf"}, "Faktura.pdf"},
		{"rfc2047", map[string]string{"filename": "=?UTF-8?Q?Faktura_=C4=8D.pdf?="}, "Faktura č.pdf"},
		{"absent", map[string]string{"other": "x"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeParam(tt.params, "filename"); got != tt.want {
				t.Errorf("decodeParam = %q, want %q", got, tt.want)
			}
		})
	}
}
