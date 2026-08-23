module github.com/srhg-ai-7cef3f93/ben

// Minor-level on purpose (#110), not the patch level `go mod init` wrote: a
// patch-level directive makes every machine below it download a whole toolchain
// under GOTOOLCHAIN=auto and refuse outright under =local, and CI derives its
// install from this line, so it can never see either. A toolchain directive is
// also deliberately absent: pinned setup-go v5 and GOTOOLCHAIN=local ignore it,
// while auto may download it. Raise or add one only for a runtime or compiler
// fix this repo actually needs, and say which — see AGENTS.md, "Toolchain".
// internal/arch fails the build otherwise.
go 1.26

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/google/go-github/v90 v90.0.0
	github.com/osteele/liquid v1.8.1
	golang.org/x/sync v0.18.0
	golang.org/x/sys v0.37.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/osteele/tuesday v1.0.4 // indirect
	golang.org/x/mod v0.29.0 // indirect
	golang.org/x/tools v0.38.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)
