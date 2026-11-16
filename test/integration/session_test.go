//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/zjor/imap-mcp/internal/imapsafe"
)

func TestOpen_LoginAndCaps(t *testing.T) {
	ctx, logs := testCtx(t)
	s, err := testDialer(t).Open(ctx, freshUser(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	caps := s.Caps()
	t.Logf("caps: %+v", caps)
	if !caps.UIDPlus {
		t.Error("UIDPlus = false")
	}
	if !strings.Contains(logs.String(), "imap.capabilities") || !strings.Contains(logs.String(), "UIDPLUS") {
		t.Errorf("capability list not logged at debug: %s", logs.String())
	}
}

func TestOpen_WrongPassword_ErrAuth(t *testing.T) {
	ctx, logs := testCtx(t)
	c := freshUser(t)
	c.Password = "wrong-password"
	_, err := testDialer(t).Open(ctx, c)
	if !errors.Is(err, imapsafe.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warn log: %s", logs.String())
	}
	if strings.Contains(logs.String(), c.Password) {
		t.Error("password in log")
	}
}

func TestOpen_SpecialFolders(t *testing.T) {
	ctx, _ := testCtx(t)
	c := freshUser(t)
	s, err := testDialer(t).Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	f := s.Folders()
	s.Close()
	if f.Drafts != "Drafts" || f.Sent != "Sent" || f.Trash != "Trash" || f.Junk != "Junk" {
		t.Fatalf("folders = %+v", f)
	}

	// The harness can write into the resolved Drafts folder.
	if uids := seed(t, c, f.Drafts, "plain.eml"); len(uids) != 1 || uids[0] == 0 {
		t.Fatalf("seed uids = %v", uids)
	}
}
