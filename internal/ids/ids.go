// Package ids builds and checks the opaque ids that tools hand to agents
// (spec 7.4). Message and attachment ids are unpadded base64url of a small
// versioned binary form. Draft ids are UUID v4 strings.
package ids

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// ErrBadID means an id is malformed. Every decode error wraps it.
var ErrBadID = errors.New("invalid id")

// MessageID names one message: its folder, the folder's UIDVALIDITY and the
// message UID.
type MessageID struct {
	Folder           string
	UIDValidity, UID uint32
}

// AttachmentID names one MIME part of a message. Part is the IMAP section
// path (RFC 3501 numbering; a non-multipart body is part [1]).
type AttachmentID struct {
	MessageID
	Part []int
}

// Binary form (all integers big-endian):
//
//	version(1) kind(1) folderLen(2) folder uidValidity(4) uid(4)
//	attachment only: depth(1) part(2) * depth
const (
	version        = 1
	kindMessage    = 'm'
	kindAttachment = 'a'

	maxFolder  = 512
	maxDepth   = MaxPartDepth
	maxPartNum = MaxPartNumber
	maxRaw     = 2 + 2 + maxFolder + 8 + 1 + 2*maxDepth
)

// Part path limits of an attachment id. mailparse lists only parts inside
// them, so every listed attachment has a valid id.
const (
	MaxPartDepth  = 20
	MaxPartNumber = 1000
)

var enc = base64.RawURLEncoding.Strict()

// EncodeMessage returns the opaque id of m.
func EncodeMessage(m MessageID) string {
	return enc.EncodeToString(appendMessage(nil, kindMessage, m))
}

// EncodeAttachment returns the opaque id of a.
func EncodeAttachment(a AttachmentID) string {
	b := appendMessage(nil, kindAttachment, a.MessageID)
	b = append(b, byte(len(a.Part)))
	for _, p := range a.Part {
		b = binary.BigEndian.AppendUint16(b, uint16(p))
	}
	return enc.EncodeToString(b)
}

func appendMessage(b []byte, kind byte, m MessageID) []byte {
	b = append(b, version, kind)
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Folder)))
	b = append(b, m.Folder...)
	b = binary.BigEndian.AppendUint32(b, m.UIDValidity)
	return binary.BigEndian.AppendUint32(b, m.UID)
}

// DecodeMessage parses a message id. Any malformed input returns ErrBadID.
func DecodeMessage(s string) (MessageID, error) {
	b, err := decodeRaw(s, kindMessage)
	if err != nil {
		return MessageID{}, err
	}
	m, rest, err := readMessage(b)
	if err != nil {
		return MessageID{}, err
	}
	if len(rest) != 0 {
		return MessageID{}, bad("trailing bytes")
	}
	return m, nil
}

// DecodeAttachment parses an attachment id. Any malformed input returns
// ErrBadID.
func DecodeAttachment(s string) (AttachmentID, error) {
	b, err := decodeRaw(s, kindAttachment)
	if err != nil {
		return AttachmentID{}, err
	}
	m, rest, err := readMessage(b)
	if err != nil {
		return AttachmentID{}, err
	}
	if len(rest) < 1 {
		return AttachmentID{}, bad("no part depth")
	}
	depth := int(rest[0])
	rest = rest[1:]
	if depth < 1 || depth > maxDepth {
		return AttachmentID{}, bad("part depth out of range")
	}
	if len(rest) != 2*depth {
		return AttachmentID{}, bad("part length mismatch")
	}
	part := make([]int, depth)
	for i := range part {
		part[i] = int(binary.BigEndian.Uint16(rest[2*i:]))
		if part[i] < 1 || part[i] > maxPartNum {
			return AttachmentID{}, bad("part number out of range")
		}
	}
	return AttachmentID{MessageID: m, Part: part}, nil
}

func decodeRaw(s string, kind byte) ([]byte, error) {
	if s == "" || len(s) > enc.EncodedLen(maxRaw) {
		return nil, bad("bad length")
	}
	b, err := enc.DecodeString(s)
	if err != nil {
		return nil, bad("not base64url")
	}
	if len(b) < 2 || b[0] != version {
		return nil, bad("unknown version")
	}
	if b[1] != kind {
		return nil, bad("wrong id kind")
	}
	return b[2:], nil
}

func readMessage(b []byte) (MessageID, []byte, error) {
	if len(b) < 2 {
		return MessageID{}, nil, bad("truncated")
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if n == 0 || n > maxFolder {
		return MessageID{}, nil, bad("folder length out of range")
	}
	if len(b) < n+8 {
		return MessageID{}, nil, bad("truncated")
	}
	folder := string(b[:n])
	if !utf8.ValidString(folder) {
		return MessageID{}, nil, bad("folder is not UTF-8")
	}
	m := MessageID{
		Folder:      folder,
		UIDValidity: binary.BigEndian.Uint32(b[n:]),
		UID:         binary.BigEndian.Uint32(b[n+4:]),
	}
	if m.UIDValidity == 0 || m.UID == 0 {
		return MessageID{}, nil, bad("zero UID or UIDVALIDITY")
	}
	return m, b[n+8:], nil
}

func bad(why string) error { return fmt.Errorf("%w: %s", ErrBadID, why) }

var draftIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewDraftID returns a random UUID v4 in lower-case canonical form.
func NewDraftID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// CheckDraftID returns ErrBadID unless s is a lower-case UUID v4.
func CheckDraftID(s string) error {
	if !draftIDRe.MatchString(s) {
		return bad("not a UUID v4")
	}
	return nil
}
