package ids

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestMessageID_RoundTrip(t *testing.T) {
	for _, folder := range []string{"INBOX", "Odeslaná pošta", "Archive/2026"} {
		in := MessageID{Folder: folder, UIDValidity: 1731750000, UID: 42}
		s := EncodeMessage(in)
		if strings.ContainsAny(s, "+/=") {
			t.Errorf("%q is not unpadded base64url", s)
		}
		out, err := DecodeMessage(s)
		if err != nil {
			t.Fatalf("DecodeMessage(%q): %v", s, err)
		}
		if out != in {
			t.Errorf("round trip = %+v, want %+v", out, in)
		}
	}
}

func TestAttachmentID_RoundTrip(t *testing.T) {
	in := AttachmentID{
		MessageID: MessageID{Folder: "Odeslaná pošta", UIDValidity: 7, UID: 1000},
		Part:      []int{2, 1},
	}
	s := EncodeAttachment(in)
	out, err := DecodeAttachment(s)
	if err != nil {
		t.Fatalf("DecodeAttachment(%q): %v", s, err)
	}
	if out.MessageID != in.MessageID || !slices.Equal(out.Part, in.Part) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}

	// The two id kinds do not decode as each other.
	if _, err := DecodeMessage(s); !errors.Is(err, ErrBadID) {
		t.Errorf("DecodeMessage(attachment id) err = %v, want ErrBadID", err)
	}
	if _, err := DecodeAttachment(EncodeMessage(in.MessageID)); !errors.Is(err, ErrBadID) {
		t.Errorf("DecodeAttachment(message id) err = %v, want ErrBadID", err)
	}
}

func TestDecode_RejectsGarbage(t *testing.T) {
	good := MessageID{Folder: "INBOX", UIDValidity: 1, UID: 1}
	b64 := base64.RawURLEncoding.EncodeToString
	// raw decodes a valid id and lets the case change its bytes.
	raw := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	wrongVersion := raw(EncodeMessage(good))
	wrongVersion[0] = 99
	trailing := append(raw(EncodeMessage(good)), 0)
	deep := make([]int, 21)
	for i := range deep {
		deep[i] = 1
	}

	msgCases := map[string]string{
		"empty":         "",
		"non-base64":    "!!not*base64!!",
		"padded":        EncodeMessage(good) + "=",
		"wrong version": b64(wrongVersion),
		"trailing byte": b64(trailing),
		"uid 0":         EncodeMessage(MessageID{Folder: "INBOX", UIDValidity: 1, UID: 0}),
		"uidvalidity 0": EncodeMessage(MessageID{Folder: "INBOX", UIDValidity: 0, UID: 1}),
		"empty folder":  EncodeMessage(MessageID{Folder: "", UIDValidity: 1, UID: 1}),
		"bad utf-8":     EncodeMessage(MessageID{Folder: "a\xffb", UIDValidity: 1, UID: 1}),
		"long folder":   EncodeMessage(MessageID{Folder: strings.Repeat("a", 513), UIDValidity: 1, UID: 1}),
		"truncated":     EncodeMessage(good)[:6],
		"1 MB string":   strings.Repeat("A", 1<<20),
	}
	for name, s := range msgCases {
		t.Run("message/"+name, func(t *testing.T) {
			if _, err := DecodeMessage(s); !errors.Is(err, ErrBadID) {
				t.Errorf("err = %v, want ErrBadID", err)
			}
		})
	}

	att := func(part ...int) string { return EncodeAttachment(AttachmentID{MessageID: good, Part: part}) }
	attCases := map[string]string{
		"empty":         "",
		"1 MB string":   strings.Repeat("A", 1<<20),
		"no part":       att(),
		"part 0":        att(0),
		"part 1001":     att(1001),
		"part depth 21": att(deep...),
		"uid 0": EncodeAttachment(AttachmentID{
			MessageID: MessageID{Folder: "INBOX", UIDValidity: 1}, Part: []int{1},
		}),
	}
	for name, s := range attCases {
		t.Run("attachment/"+name, func(t *testing.T) {
			if _, err := DecodeAttachment(s); !errors.Is(err, ErrBadID) {
				t.Errorf("err = %v, want ErrBadID", err)
			}
		})
	}

	// The limits themselves are accepted.
	edge := make([]int, 20)
	for i := range edge {
		edge[i] = 1000
	}
	if _, err := DecodeAttachment(att(edge...)); err != nil {
		t.Errorf("depth 20, part 1000: %v", err)
	}
	if _, err := DecodeMessage(EncodeMessage(MessageID{Folder: strings.Repeat("é", 256), UIDValidity: 1, UID: 1})); err != nil {
		t.Errorf("512-byte folder: %v", err)
	}
}

func TestDraftID(t *testing.T) {
	a, b := NewDraftID(), NewDraftID()
	if a == b {
		t.Fatal("two draft ids are equal")
	}
	for _, s := range []string{a, b, "0b6e3c1a-5f2d-4c8e-9a7b-1d2e3f4a5b6c"} {
		if err := CheckDraftID(s); err != nil {
			t.Errorf("CheckDraftID(%q) = %v", s, err)
		}
	}
	for _, s := range []string{
		"",
		"0b6e3c1a-5f2d-1c8e-9a7b-1d2e3f4a5b6c", // version 1
		"0b6e3c1a-5f2d-4c8e-7a7b-1d2e3f4a5b6c", // wrong variant
		"0B6E3C1A-5F2D-4C8E-9A7B-1D2E3F4A5B6C", // upper case
		"0b6e3c1a5f2d4c8e9a7b1d2e3f4a5b6c",
		"0b6e3c1a-5f2d-4c8e-9a7b-1d2e3f4a5b6c\n",
	} {
		if err := CheckDraftID(s); !errors.Is(err, ErrBadID) {
			t.Errorf("CheckDraftID(%q) = %v, want ErrBadID", s, err)
		}
	}
}
