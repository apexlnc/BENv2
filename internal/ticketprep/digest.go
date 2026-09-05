package ticketprep

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"unicode/utf8"
)

const issueDigestDomain = "ticketprep.issue-content.v1\x00"
const packetDigestDomain = "ticketprep.packet.v1\x00"

// IssueDigest binds the decoded UTF-8 bytes exactly. In particular it performs
// no trimming, Unicode normalization, newline conversion, or final-newline
// insertion; JSON escape spelling has already ceased to be relevant.
func IssueDigest(title, body string) (string, error) {
	if !utf8.ValidString(title) || !utf8.ValidString(body) {
		return "", ErrInvalidUTF8
	}
	h := sha256.New()
	h.Write([]byte(issueDigestDomain)) // hash.Hash never returns a write error
	writeLengthAndBytes(h, []byte(title))
	writeLengthAndBytes(h, []byte(body))
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeLengthAndBytes(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:]) // hash.Hash never returns a write error
	h.Write(value)   // hash.Hash never returns a write error
}

func PacketDigest(packet Packet) (string, error) {
	body, err := json.Marshal(packet)
	if err != nil {
		return "", fmt.Errorf("ticketprep: marshal packet digest: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(packetDigestDomain))
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
