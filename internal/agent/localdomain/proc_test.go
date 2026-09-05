package localdomain

import "testing"

func TestParseUnifiedCgroup(t *testing.T) {
	got, err := parseUnifiedCgroup("0::/user.slice/ben.service\n")
	if err != nil || got != "/user.slice/ben.service" {
		t.Fatalf("parse = (%q, %v)", got, err)
	}
	for _, value := range []string{"", "1:name=/x\n", "0::relative\n", "0::/x/../y\n", "0::/x\n0::/y\n"} {
		if _, err := parseUnifiedCgroup(value); err == nil {
			t.Errorf("parseUnifiedCgroup(%q) succeeded", value)
		}
	}
}

func TestParseProcStatHandlesSpacesAndClosingParentheses(t *testing.T) {
	const stat = "123 (provider ) odd) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987654 20"
	state, start, err := parseProcStat(stat)
	if err != nil {
		t.Fatal(err)
	}
	if state != 'S' || start != 987654 {
		t.Fatalf("parseProcStat = (%q, %d)", state, start)
	}
}

func TestParseProcStatRejectsMalformedRecords(t *testing.T) {
	for _, value := range []string{"", "x (a) S 1", "1 a) S 1", "1 (a) SS 1 2 3"} {
		if _, _, err := parseProcStat(value); err == nil {
			t.Errorf("parseProcStat(%q) succeeded", value)
		}
	}
}
