package ticketprep

import "testing"

func TestFreshnessUsesSectionBindings(t *testing.T) {
	packet := validPacket(t)

	t.Run("unchanged", func(t *testing.T) {
		report, err := Freshness(packet, packet.Capture)
		if err != nil {
			t.Fatal(err)
		}
		for _, section := range report.Sections {
			if section.Status != FreshnessMatchesCapture {
				t.Errorf("%s = %s, want matches_capture", section.Section, section.Status)
			}
		}
	})

	t.Run("repository moved", func(t *testing.T) {
		current := packet.Capture
		current.Repository.Commit = repeat("c", 40)
		current.Repository.Tree = repeat("d", 40)
		report, err := Freshness(packet, current)
		if err != nil {
			t.Fatal(err)
		}
		if !report.SubjectMatches || report.RepositoryMatches {
			t.Fatalf("matches = subject %v repository %v", report.SubjectMatches, report.RepositoryMatches)
		}
		for _, section := range report.Sections {
			want := FreshnessStale
			if section.Binding == BindingSubject {
				want = FreshnessMatchesCapture
			}
			if section.Status != want {
				t.Errorf("%s = %s, want %s", section.Section, section.Status, want)
			}
		}
	})

	t.Run("subject moved", func(t *testing.T) {
		current := packet.Capture
		current.Subject.Body += "\nchanged"
		current.Subject.ContentDigest, _ = IssueDigest(current.Subject.Title, current.Subject.Body)
		report, err := Freshness(packet, current)
		if err != nil {
			t.Fatal(err)
		}
		for _, section := range report.Sections {
			if section.Status != FreshnessStale {
				t.Errorf("%s = %s, want stale", section.Section, section.Status)
			}
		}
	})
}

func TestFreshnessCoversEveryRenderedSection(t *testing.T) {
	packet := validPacket(t)
	report, err := Freshness(packet, packet.Capture)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sections) != len(sectionDefinitions) {
		t.Fatalf("got %d sections, want %d", len(report.Sections), len(sectionDefinitions))
	}
	seen := map[string]bool{}
	for _, section := range report.Sections {
		if seen[section.Section] {
			t.Fatalf("duplicate freshness section %q", section.Section)
		}
		seen[section.Section] = true
	}
	for _, suggestion := range reportItems(packet.Advice) {
		if !seen[suggestion.Section] {
			t.Errorf("suggestion %s has no freshness section %q", suggestion.ID, suggestion.Section)
		}
	}
}
