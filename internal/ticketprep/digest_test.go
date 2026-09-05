package ticketprep

import "testing"

func TestIssueDigestVectors(t *testing.T) {
	// Generated independently from the contract bytes (domain, uint64 big-endian
	// lengths, then exact values), not by calling IssueDigest itself.
	tests := []struct {
		name, title, body, want string
	}{
		{"empty values", "", "", "sha256:d672e879911efd17ccc45dbcd15d797427a298be92fb8d0d3e37ef3313b63f95"},
		{"field order and lengths", "A", "B", "sha256:10cf22ac161c7967de92abbc917da0ccfe6fd52b3742990bf84aaffa25ce91c7"},
		{"UTF-8 NFC and NFD bytes", "é", "e\u0301", "sha256:1a32214d3a80a6d099315b41fdc290d2e04d9503cb6e0d7ad432a81b915d356a"},
		{"CRLF LF and final newline", "title", "line1\r\nline2\n", "sha256:44f30500a0ae386e1c2396449aeb352bb8f430659ceb18874b60d57a5100b949"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IssueDigest(tt.title, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("IssueDigest(%q, %q) = %s, want %s", tt.title, tt.body, got, tt.want)
			}
		})
	}
}

func TestIssueDigestDoesNotNormalizeContent(t *testing.T) {
	pairs := []struct{ leftTitle, leftBody, rightTitle, rightBody string }{
		{"same", "line\n", "same", "line\r\n"},
		{"same", "line", "same", "line\n"},
		{"é", "", "e\u0301", ""},
		{"A", "B", "B", "A"},
	}
	for _, pair := range pairs {
		left, err := IssueDigest(pair.leftTitle, pair.leftBody)
		if err != nil {
			t.Fatal(err)
		}
		right, err := IssueDigest(pair.rightTitle, pair.rightBody)
		if err != nil {
			t.Fatal(err)
		}
		if left == right {
			t.Fatalf("distinct exact inputs collided: %#v", pair)
		}
	}
}

func TestIssueDigestRejectsInvalidUTF8(t *testing.T) {
	if _, err := IssueDigest(string([]byte{0xff}), ""); err != ErrInvalidUTF8 {
		t.Fatalf("error = %v, want %v", err, ErrInvalidUTF8)
	}
}
