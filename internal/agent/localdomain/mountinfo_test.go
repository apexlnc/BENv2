package localdomain

import (
	"strings"
	"testing"
)

func TestParseMountInfo(t *testing.T) {
	input := "36 25 0:32 / /proc rw,nosuid,nodev,noexec,relatime shared:7 - proc proc rw\n" +
		"42 25 0:29 /user.slice /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate\n" +
		"43 36 0:32 /sys /proc/a\\040b rw - proc proc rw\n" +
		"44 25 0:29 /../../../../.. /old-cgroup rw - cgroup2 cgroup rw,nsdelegate\n" +
		"45 25 0:32 / /slave-proc rw master:7 propagate_from:9 - proc proc rw\n"
	records, err := parseMountInfo(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 {
		t.Fatalf("records = %d", len(records))
	}
	if records[2].Target != "/proc/a b" {
		t.Fatalf("escaped target = %q", records[2].Target)
	}
	if !isSharedMount(records[0]) {
		t.Fatal("shared propagation was not decoded")
	}
	if !hasMountPropagation(records[0]) || !hasMountPropagation(records[4]) || isSharedMount(records[4]) {
		t.Fatal("shared/slave propagation linkage was not decoded")
	}
	if !hasMountOption(records[1], "nsdelegate") {
		t.Fatal("superblock nsdelegate option was not decoded")
	}
	if records[3].Root != "/../../../../.." {
		t.Fatalf("namespace-relative cgroup root = %q", records[3].Root)
	}
}

func TestMountInfoParserRejectsMalformedInput(t *testing.T) {
	cases := []string{
		"",
		"36 25 0:32 / /proc rw - proc proc\n",
		"0 25 0:32 / /proc rw - proc proc rw\n",
		"36 25 bad / /proc rw - proc proc rw\n",
		"36 25 0:32 relative /proc rw - proc proc rw\n",
		"36 25 0:32 /bad//root /proc rw - proc proc rw\n",
		"36 25 0:32 /bad/./root /proc rw - proc proc rw\n",
		"36 25 0:32 / /proc/../proc rw - proc proc rw\n",
		"36 25 0:32 / /bad\\777 rw - proc proc rw\n",
		"36 25 0:32 / /proc rw - - proc proc rw\n",
	}
	for _, input := range cases {
		t.Run(strings.ReplaceAll(input, " ", "_"), func(t *testing.T) {
			if _, err := parseMountInfo(strings.NewReader(input)); err == nil {
				t.Fatalf("parseMountInfo(%q) succeeded", input)
			}
		})
	}
}
