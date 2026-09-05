package localdomain

import (
	"errors"
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{
		Boot:       "01234567-89ab-cdef-0123-456789abcdef",
		Delegate:   ObjectID{DevMajor: 0x1, DevMinor: 0x2, Inode: 0x3},
		Root:       ObjectID{DevMajor: 0x4, DevMinor: 0x5, Inode: 0x6},
		Name:       "00112233445566778899aabbccddeeff",
		Leaf:       ObjectID{DevMajor: 0x7, DevMinor: 0x8, Inode: 0x9},
		PID:        12345,
		StartTicks: 987654321,
		PIDNS:      ObjectID{DevMajor: 0xa, DevMinor: 0xb, Inode: 0xc},
		CgroupNS:   ObjectID{DevMajor: 0xd, DevMinor: 0xe, Inode: 0xf},
	}
}

func TestEvidenceEncodingIsByteStable(t *testing.T) {
	want := Evidence{
		Scheme: EvidenceScheme,
		Boot:   "01234567-89ab-cdef-0123-456789abcdef",
		ID: "v1;delegate=00000001:00000002:0000000000000003;" +
			"root=00000004:00000005:0000000000000006;" +
			"name=00112233445566778899aabbccddeeff;" +
			"leaf=00000007:00000008:0000000000000009;pid=12345;" +
			"start=987654321;pidns=0000000a:0000000b:000000000000000c;" +
			"cgns=0000000d:0000000e:000000000000000f",
	}
	got, err := EncodeEvidence(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("EncodeEvidence = %#v, want %#v", got, want)
	}
	decoded, err := ParseEvidence(got)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != testIdentity() {
		t.Fatalf("ParseEvidence = %#v, want %#v", decoded, testIdentity())
	}
}

func TestEvidenceParserFailsClosed(t *testing.T) {
	valid, err := EncodeEvidence(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]Evidence{
		"unknown scheme":       {Scheme: "linux-domain-v2", Boot: valid.Boot, ID: valid.ID},
		"upper boot":           {Scheme: valid.Scheme, Boot: strings.ToUpper(valid.Boot), ID: valid.ID},
		"bad boot shape":       {Scheme: valid.Scheme, Boot: strings.Replace(valid.Boot, "-", "", 1), ID: valid.ID},
		"unknown version":      {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "v1;", "v2;", 1)},
		"reordered fields":     {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "delegate=", "root=", 1)},
		"extra field":          {Scheme: valid.Scheme, Boot: valid.Boot, ID: valid.ID + ";extra=x"},
		"upper object":         {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "0000000a", "0000000A", 1)},
		"short object":         {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "00000001", "1", 1)},
		"zero inode":           {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "0000000000000003", "0000000000000000", 1)},
		"upper nonce":          {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "aabb", "AABB", 1)},
		"short nonce":          {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, testIdentity().Name, testIdentity().Name[:31], 1)},
		"zero pid":             {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "pid=12345", "pid=0", 1)},
		"leading-zero pid":     {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "pid=12345", "pid=012345", 1)},
		"overflow pid":         {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "pid=12345", "pid=2147483648", 1)},
		"leading-zero start":   {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "start=987654321", "start=0987654321", 1)},
		"overflow start":       {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, "start=987654321", "start=18446744073709551616", 1)},
		"whitespace":           {Scheme: valid.Scheme, Boot: valid.Boot, ID: valid.ID + "\n"},
		"duplicated delimiter": {Scheme: valid.Scheme, Boot: valid.Boot, ID: strings.Replace(valid.ID, ";root=", ";;root=", 1)},
	}
	for name, evidence := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEvidence(evidence); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("ParseEvidence error = %v, want ErrInvalidEvidence", err)
			}
		})
	}
}

func TestZeroStartTicksIsCanonical(t *testing.T) {
	id := testIdentity()
	id.StartTicks = 0
	evidence, err := EncodeEvidence(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(evidence.ID, ";start=0;") {
		t.Fatalf("ID = %q, want canonical zero start", evidence.ID)
	}
	if _, err := ParseEvidence(evidence); err != nil {
		t.Fatal(err)
	}
}
