package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFor(t *testing.T) {
	t.Run("XDG_STATE_HOME wins", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
		if got, want := For("wf").Root(), filepath.Join("/tmp/xdg", "ben", "wf"); got != want {
			t.Errorf("root = %q, want %q", got, want)
		}
	})
	t.Run("falls back to ~/.local/state", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/tmp/home")
		if got, want := For("wf").Root(), filepath.Join("/tmp/home", ".local", "state", "ben", "wf"); got != want {
			t.Errorf("root = %q, want %q", got, want)
		}
	})
}

func TestPrepareIsPrivate(t *testing.T) {
	d := At(filepath.Join(t.TempDir(), "state"))
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, path := range []string{d.Root(), d.Transcripts()} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %o, want 700 — a transcript is the agent's whole raw stream", path, got)
		}
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	d := At(filepath.Join(t.TempDir(), "state"))
	for i := range 2 {
		if err := d.Prepare(); err != nil {
			t.Fatalf("Prepare #%d: %v", i+1, err)
		}
	}
}

func TestAtomicWriteReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := atomicWrite(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := atomicWrite(path, []byte("second")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "second" {
		t.Errorf("contents = %q, want %q", raw, "second")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}

	// The temporary must not survive: a directory accumulating .tmp-* is a
	// state dir an operator cannot read at a glance, and one of them is a
	// half-written state file sitting next to the real one.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "f.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only f.json", names)
	}
}

func TestDaemonStale(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		d     Daemon
		grace time.Duration
		want  bool
	}{
		{
			name: "fresh heartbeat",
			d:    Daemon{HeartbeatMS: 1000, WrittenAt: now.Add(-500 * time.Millisecond)},
			want: false,
		},
		{
			name:  "one missed beat is within grace",
			d:     Daemon{HeartbeatMS: 1000, WrittenAt: now.Add(-3 * time.Second)},
			grace: 5 * time.Second,
			want:  false,
		},
		{
			name:  "silent past the interval and the grace",
			d:     Daemon{HeartbeatMS: 1000, WrittenAt: now.Add(-30 * time.Second)},
			grace: 5 * time.Second,
			want:  true,
		},
		{
			// A writer that declared no interval promised no heartbeat. Calling
			// that stale would invent a claim about a daemon nobody made.
			name: "no declared interval is never stale",
			d:    Daemon{HeartbeatMS: 0, WrittenAt: now.Add(-365 * 24 * time.Hour)},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.Stale(now, tc.grace); got != tc.want {
				t.Errorf("Stale = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadRunsAbsentIsErrNoState(t *testing.T) {
	d := At(t.TempDir())
	_, err := d.ReadRuns()
	if !errors.Is(err, ErrNoState) {
		t.Fatalf("err = %v, want ErrNoState — an unwritten directory is not an idle daemon", err)
	}
}

func TestReadRunsRoundTrip(t *testing.T) {
	d := At(t.TempDir())
	at := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)
	fires := at.Add(2 * time.Minute)
	want := Runs{
		Daemon: Daemon{
			ID: "host/wf", Workflow: "wf", PID: 4242,
			StartedAt: at, WrittenAt: at.Add(time.Second),
			HeartbeatMS: 1000, Draining: true, HeldClaims: 2,
		},
		Records: []Run{{
			Issue: "#7", RunID: "7-a2-t3", State: "backoff", Attempt: 2, Turns: 1,
			FailureReason: "crashed", Branch: "ben/7", UpdatedAt: at,
			NextTimerAt: &fires, NextTimer: "backoff", Continuation: "sess-1",
		}},
	}
	if err := d.WriteRuns(want); err != nil {
		t.Fatalf("WriteRuns: %v", err)
	}
	got, err := d.ReadRuns()
	if err != nil {
		t.Fatalf("ReadRuns: %v", err)
	}
	if got.Daemon != want.Daemon {
		t.Errorf("daemon = %+v, want %+v", got.Daemon, want.Daemon)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	g, w := got.Records[0], want.Records[0]
	if g.NextTimerAt == nil || !g.NextTimerAt.Equal(*w.NextTimerAt) {
		t.Errorf("next_timer_at = %v, want %v", g.NextTimerAt, w.NextTimerAt)
	}
	g.NextTimerAt, w.NextTimerAt = nil, nil
	if g != w {
		t.Errorf("record = %+v, want %+v", g, w)
	}
	if !got.Records[0].Resuming() {
		t.Error("Resuming = false, want true — a record carrying a continuation token resumes")
	}
}

func TestReadRunsTornFileIsAnErrorNotAnIdleDaemon(t *testing.T) {
	d := At(t.TempDir())
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// What a crash between rename and sync leaves: the name is published, the
	// bytes are not all there.
	if err := os.WriteFile(d.RunsPath(), []byte(`{"daemon":{"id":"host/wf","pi`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := d.ReadRuns()
	if err == nil {
		t.Fatalf("ReadRuns = %+v, nil error; a truncated file must not read as a daemon with nothing to do", got)
	}
	if errors.Is(err, ErrNoState) {
		t.Errorf("err = %v, want a parse error — present-but-unreadable is not absent", err)
	}
}

// A reader in another process runs while the daemon writes (SPEC §11). Every
// read must land on one whole revision.
func TestReadRunsNeverSeesAPartialWrite(t *testing.T) {
	d := At(t.TempDir())
	at := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			records := make([]Run, i%40)
			for j := range records {
				records[j] = Run{Issue: "#" + string(rune('a'+j)), State: "running", UpdatedAt: at}
			}
			if err := d.WriteRuns(Runs{Daemon: Daemon{ID: "host/wf", PID: i}, Records: records}); err != nil {
				t.Errorf("WriteRuns: %v", err)
				return
			}
		}
	}()

	var reads int
	for {
		select {
		case <-done:
			if reads == 0 {
				t.Fatal("the reader never got a parseable revision")
			}
			return
		default:
		}
		got, err := d.ReadRuns()
		if errors.Is(err, ErrNoState) {
			continue // the first write has not landed yet
		}
		if err != nil {
			t.Fatalf("read %d: %v", reads, err)
		}
		if len(got.Records) != got.Daemon.PID%40 {
			t.Fatalf("read a mixture: %d records under revision %d", len(got.Records), got.Daemon.PID)
		}
		reads++
	}
}
