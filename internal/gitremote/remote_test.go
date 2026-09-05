package gitremote

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
)

func TestRepositoryIdentity(t *testing.T) {
	tests := []struct {
		name, remote, want string
		wantErr            error
	}{
		{"https", "https://github.test/acme/repo.git", identity("https://github.test/acme/repo.git", "github.test/acme/repo"), nil},
		{"port", "https://github.test:8443/acme/repo.git", identity("https://github.test:8443/acme/repo.git", "github.test:8443/acme/repo"), nil},
		{"scp-like", "git@github.test:acme/repo.git", identity("git@github.test:acme/repo.git", "github.test:acme/repo"), nil},
		{"local path", "/srv/git/acme/repo.git", identity("/srv/git/acme/repo.git", "/srv/git/acme/repo"), nil},
		{"empty", "  ", "", ErrRemoteEmpty},
		{"credential", "https://secret@github.test/acme/repo.git", "", ErrRemoteCredentials},
		{"transport helper", "corp::opaque", "", ErrTransportHelperRemote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RepositoryIdentity(tt.remote)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("RepositoryIdentity(%q) = %q, %v; want %q, %v", tt.remote, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func identity(remote, display string) string {
	return fmt.Sprintf("sha256:%x/%s", sha256.Sum256([]byte(remote)), display)
}

func TestRepositoryIdentityDoesNotCollapseDistinctRemotes(t *testing.T) {
	for _, tt := range []struct {
		name          string
		first, second string
	}{
		{"literal dot-git path", "/srv/git/acme/repo.git", "/srv/git/acme/repo"},
		{"SSH username", "alice@github.test:acme/repo.git", "bob@github.test:acme/repo.git"},
		{"transport", "https://github.test/acme/repo.git", "ssh://git@github.test/acme/repo.git"},
		{"at sign in a relative path", "prefix@segment/repo.git", "segment/repo.git"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first, err := RepositoryIdentity(tt.first)
			if err != nil {
				t.Fatalf("RepositoryIdentity(%q): %v", tt.first, err)
			}
			second, err := RepositoryIdentity(tt.second)
			if err != nil {
				t.Fatalf("RepositoryIdentity(%q): %v", tt.second, err)
			}
			if first == second {
				t.Fatalf("distinct remotes %q and %q collapsed to %q", tt.first, tt.second, first)
			}
		})
	}
}

func TestEmbedsCredential(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   bool
	}{
		{"https password", "https://user:secret@example.test/o/r.git", true},
		{"https bare token", "https://secret@example.test/o/r.git", true},
		{"unparseable https token", "https://secret@example.test:notaport/o/r.git", true},
		{"ssh password", "ssh://git:secret@example.test/o/r.git", true},
		{"unparseable ssh password", "ssh://git:secret@example.test:notaport/o/r.git", true},
		{"ssh public username", "ssh://git@example.test/o/r.git", false},
		{"scp-like username", "git@example.test:o/r.git", false},
		{"scp-like at sign in path", "git@example.test:path@segment/r.git", false},
		{"credential-free https", "https://example.test/o/r.git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EmbedsCredential(tt.remote); got != tt.want {
				t.Fatalf("EmbedsCredential(%q) = %t, want %t", tt.remote, got, tt.want)
			}
		})
	}
}

func TestIsTransportHelper(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{"mock::opaque", true},
		{"1mock::opaque", true},
		{"mock-v1.2+corp::opaque", true},
		{"::opaque", true},
		{"ssh://git@[::1]/repo.git", false},
		{"https://example.test/o/r.git", false},
	}
	for _, tt := range tests {
		if got := IsTransportHelper(tt.remote); got != tt.want {
			t.Errorf("IsTransportHelper(%q) = %t, want %t", tt.remote, got, tt.want)
		}
	}
}
