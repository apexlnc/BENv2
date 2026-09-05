package ticketprep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srhg-ai-7cef3f93/ben/internal/gitcmd"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

const maxGitOutputBytes = 64 << 20

var (
	lineReferencePattern = regexp.MustCompile(`^(.+?)(?::[0-9]+(?:,[0-9]+)*)?$`)
	goIdentifierPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pathExtensionPattern = regexp.MustCompile(`(?i)\.(?:go|md|json|ya?ml|toml|sh)(?::[0-9]+(?:,[0-9]+)*)?$`)
	makeTargetPattern    = regexp.MustCompile(`^([A-Za-z0-9_.-]+):(?:\s|$)`)
)

type treeEntry struct {
	path string
	blob string
	kind string
}

type repositoryReader struct {
	root      string
	commit    string
	tree      string
	entries   map[string]treeEntry
	basenames map[string][]treeEntry
}

// CaptureIssue records the issue snapshot and repository observations without
// consulting the working-tree files. Tracked edits and untracked files can
// therefore neither enter facts nor be attributed to the recorded commit.
func CaptureIssue(ctx context.Context, repoPath string, issue IssueInput) (Capture, error) {
	if err := issue.Validate(); err != nil {
		return Capture{}, err
	}
	repo, observed, fingerprint, err := openRepository(ctx, repoPath)
	if err != nil {
		return Capture{}, err
	}
	fromIssue, err := repositoryFromIssueURL(issue.URL)
	if err != nil {
		return Capture{}, err
	}
	if observed != fromIssue {
		return Capture{}, fmt.Errorf("%w: issue repository %q != origin repository %q", ErrBindingMismatch, fromIssue, observed)
	}
	digest, err := IssueDigest(issue.Title, issue.Body)
	if err != nil {
		return Capture{}, err
	}

	facts, err := repo.observe(ctx, issue.Title, issue.Body)
	if err != nil {
		return Capture{}, err
	}
	capture := Capture{
		SchemaVersion: SchemaVersion,
		KernelVersion: KernelVersion,
		Subject: Subject{
			RepositoryIdentity: observed,
			IssueNumber:        issue.Number,
			IssueURL:           issue.URL,
			Title:              issue.Title,
			Body:               issue.Body,
			ContentDigest:      digest,
		},
		Repository: Repository{
			Remote:            "origin",
			Identity:          observed,
			RemoteFingerprint: fingerprint,
			Commit:            repo.commit,
			Tree:              repo.tree,
		},
		Facts: facts,
		Sources: FactSources{
			Issue:      "declared_issue_snapshot",
			Repository: "git_object_database",
			Remote:     "git_config:remote.origin.url",
		},
	}
	if err := capture.Validate(); err != nil {
		return Capture{}, fmt.Errorf("ticketprep: internally produced invalid capture: %w", err)
	}
	return capture, nil
}

func openRepository(ctx context.Context, repoPath string) (*repositoryReader, string, string, error) {
	rootBytes, err := runGit(ctx, repoPath, maxURLBytes, false, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, "", "", err
	}
	root := strings.TrimSuffix(string(rootBytes), "\n")
	if root == "" || strings.ContainsRune(root, '\x00') {
		return nil, "", "", fmt.Errorf("%w: git returned an invalid repository root", ErrRepository)
	}
	commitBytes, err := runGit(ctx, root, 256, false, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, "", "", err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if !objectIDPattern.MatchString(commit) {
		return nil, "", "", fmt.Errorf("%w: HEAD is not a full object ID", ErrRepository)
	}
	treeBytes, err := runGit(ctx, root, 256, false, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return nil, "", "", err
	}
	tree := strings.TrimSpace(string(treeBytes))
	if !objectIDPattern.MatchString(tree) {
		return nil, "", "", fmt.Errorf("%w: commit tree is not a full object ID", ErrRepository)
	}
	remoteBytes, err := runGit(ctx, root, maxURLBytes, false, "config", "--get", "remote.origin.url")
	if err != nil {
		return nil, "", "", err
	}
	remote := strings.TrimSuffix(string(remoteBytes), "\n")
	if _, err := gitremote.RepositoryIdentity(remote); err != nil {
		return nil, "", "", fmt.Errorf("%w: origin identity: %v", ErrRepository, err)
	}
	identity, err := normalizeRemote(remote)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256([]byte(remote))
	fingerprint := "sha256:" + hex.EncodeToString(sum[:])

	lsTree, err := runGit(ctx, root, maxGitOutputBytes, false, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, "", "", err
	}
	entries, err := parseTree(lsTree)
	if err != nil {
		return nil, "", "", err
	}
	return newRepositoryReader(root, commit, tree, entries), identity, fingerprint, nil
}

func newRepositoryReader(root, commit, tree string, entries map[string]treeEntry) *repositoryReader {
	basenames := make(map[string][]treeEntry)
	for _, entry := range entries {
		base := path.Base(entry.path)
		basenames[base] = append(basenames[base], entry)
	}
	for base := range basenames {
		sort.Slice(basenames[base], func(i, j int) bool {
			return basenames[base][i].path < basenames[base][j].path
		})
	}
	return &repositoryReader{root: root, commit: commit, tree: tree, entries: entries, basenames: basenames}
}

func parseTree(data []byte) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	for _, raw := range bytes.Split(data, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if len(entries) >= maxTreeEntries {
			return nil, fmt.Errorf("%w: repository tree has more than %d entries", ErrBoundExceeded, maxTreeEntries)
		}
		metadata, name, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok || !utf8.Valid(name) {
			return nil, fmt.Errorf("%w: tree contains an unrepresentable path", ErrRepository)
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 || !objectIDPattern.MatchString(fields[2]) {
			continue
		}
		nameString := string(name)
		entries[nameString] = treeEntry{path: nameString, blob: fields[2], kind: fields[1]}
	}
	return entries, nil
}

func (r *repositoryReader) observe(ctx context.Context, title, body string) (Facts, error) {
	literals := codeLiterals(title + "\n" + body)
	var symbolReferences []string
	facts := Facts{
		Paths:              []PathFact{},
		Symbols:            []SymbolFact{},
		InstructionFiles:   []InstructionFact{},
		ValidationCommands: []CommandFact{},
		Unknown:            []UnknownFact{},
	}
	for _, literal := range literals {
		switch {
		case looksLikePath(literal) || r.namesCommittedPath(literal):
			fact, ok := r.pathFact(literal)
			if ok {
				facts.Paths = append(facts.Paths, fact)
				if err := factCollectionBound("paths", len(facts.Paths)); err != nil {
					return Facts{}, err
				}
			} else {
				facts.Unknown = append(facts.Unknown, UnknownFact{Reference: literal, Reason: "path syntax is unsupported or unsafe"})
				if err := factCollectionBound("unknown", len(facts.Unknown)); err != nil {
					return Facts{}, err
				}
			}
		case looksLikeGoSymbol(literal):
			symbolReferences = append(symbolReferences, literal)
			if err := factCollectionBound("symbols", len(symbolReferences)); err != nil {
				return Facts{}, err
			}
		case looksLikeUnsupportedReference(literal):
			facts.Unknown = append(facts.Unknown, UnknownFact{Reference: literal, Reason: "literal is not an unambiguous v0 path or Go symbol reference"})
			if err := factCollectionBound("unknown", len(facts.Unknown)); err != nil {
				return Facts{}, err
			}
		}
	}
	// Classify the entire bounded frontier before invoking Git once per symbol.
	// A rejected issue therefore cannot turn a large advisory body into an
	// unbounded sequence of subprocesses.
	for _, literal := range symbolReferences {
		fact, err := r.symbolFact(ctx, literal)
		if err != nil {
			return Facts{}, err
		}
		facts.Symbols = append(facts.Symbols, fact)
	}

	sort.Slice(facts.Paths, func(i, j int) bool { return facts.Paths[i].Reference < facts.Paths[j].Reference })
	sort.Slice(facts.Symbols, func(i, j int) bool { return facts.Symbols[i].Reference < facts.Symbols[j].Reference })
	sort.Slice(facts.Unknown, func(i, j int) bool { return facts.Unknown[i].Reference < facts.Unknown[j].Reference })
	facts.Paths = dedupePaths(facts.Paths)
	facts.Symbols = dedupeSymbols(facts.Symbols)
	facts.Unknown = dedupeUnknown(facts.Unknown)

	facts.InstructionFiles = r.applicableInstructions(facts.Paths)
	commands, err := r.validationCommands(ctx, facts.InstructionFiles)
	if err != nil {
		return Facts{}, err
	}
	facts.ValidationCommands = commands
	return facts, nil
}

func factCollectionBound(name string, count int) error {
	if count > maxFactCount {
		return fmt.Errorf("%w: capture.facts.%s has %d values, max %d", ErrBoundExceeded, name, count, maxFactCount)
	}
	return nil
}

func (r *repositoryReader) namesCommittedPath(reference string) bool {
	match := lineReferencePattern.FindStringSubmatch(reference)
	if match == nil || !validRepositoryPath(match[1]) {
		return false
	}
	wanted := match[1]
	if strings.Contains(wanted, "/") {
		_, ok := r.entries[wanted]
		return ok
	}
	return len(r.basenames[wanted]) > 0
}

func (r *repositoryReader) pathFact(reference string) (PathFact, bool) {
	match := lineReferencePattern.FindStringSubmatch(reference)
	if match == nil {
		return PathFact{}, false
	}
	wanted := match[1]
	if !validRepositoryPath(wanted) {
		return PathFact{}, false
	}
	evidence := "git_tree:" + r.tree
	if strings.Contains(wanted, "/") {
		entry, ok := r.entries[wanted]
		if !ok {
			return PathFact{Reference: reference, Status: FactAbsent, Evidence: evidence, Reason: "literal path is absent from the recorded tree"}, true
		}
		if entry.kind != "blob" {
			return PathFact{Reference: reference, Status: FactUnknown, Evidence: evidence, Reason: "literal path names a non-blob tree entry"}, true
		}
		return PathFact{Reference: reference, Status: FactExists, ResolvedPath: wanted, Blob: entry.blob, Evidence: evidence}, true
	}
	matches := r.basenames[wanted]
	switch len(matches) {
	case 0:
		return PathFact{Reference: reference, Status: FactAbsent, Evidence: evidence, Reason: "literal basename is absent from the recorded tree"}, true
	case 1:
		if matches[0].kind != "blob" {
			return PathFact{Reference: reference, Status: FactUnknown, Evidence: evidence, Reason: "literal basename names a non-blob tree entry"}, true
		}
		return PathFact{Reference: reference, Status: FactExists, ResolvedPath: matches[0].path, Blob: matches[0].blob, Evidence: evidence}, true
	default:
		return PathFact{Reference: reference, Status: FactUnknown, Evidence: evidence, Reason: fmt.Sprintf("basename is ambiguous across %d committed paths", len(matches))}, true
	}
}

type declaration struct {
	path string
	line int
	blob string
}

func (r *repositoryReader) symbolFact(ctx context.Context, name string) (SymbolFact, error) {
	evidence := "go_syntax_at_commit:" + r.commit
	grep, err := runGit(ctx, r.root, maxGitOutputBytes, true,
		"grep", "-z", "-l", "-w", "-e", name, r.commit, "--", "*.go")
	if err != nil {
		return SymbolFact{}, err
	}
	var declarations []declaration
	unparsableCandidates := 0
	for _, raw := range bytes.Split(grep, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		prefix := r.commit + ":"
		filename := strings.TrimPrefix(string(raw), prefix)
		entry, ok := r.entries[filename]
		if !ok || filename == string(raw) {
			return SymbolFact{}, fmt.Errorf("%w: git grep returned an unbound path", ErrRepository)
		}
		content, err := r.blob(ctx, entry.blob)
		if err != nil {
			return SymbolFact{}, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filename, content, parser.SkipObjectResolution)
		if err != nil {
			unparsableCandidates++
			continue
		}
		for _, ident := range declaredIdentifiers(file) {
			if ident.Name == name {
				declarations = append(declarations, declaration{path: filename, line: fset.Position(ident.Pos()).Line, blob: entry.blob})
			}
		}
	}
	sort.Slice(declarations, func(i, j int) bool {
		if declarations[i].path != declarations[j].path {
			return declarations[i].path < declarations[j].path
		}
		return declarations[i].line < declarations[j].line
	})
	if unparsableCandidates > 0 {
		return SymbolFact{
			Reference: name,
			Name:      name,
			Status:    FactUnknown,
			Evidence:  evidence,
			Reason:    fmt.Sprintf("%d committed Go candidate files containing the identifier could not be parsed", unparsableCandidates),
		}, nil
	}
	switch len(declarations) {
	case 0:
		return SymbolFact{Reference: name, Name: name, Status: FactAbsent, Evidence: evidence, Reason: "no committed Go declaration has this identifier"}, nil
	case 1:
		got := declarations[0]
		return SymbolFact{Reference: name, Name: name, Status: FactExists, Path: got.path, Line: got.line, Blob: got.blob, Evidence: evidence}, nil
	default:
		return SymbolFact{Reference: name, Name: name, Status: FactUnknown, Evidence: evidence, Reason: fmt.Sprintf("identifier is ambiguous across %d committed declarations", len(declarations))}, nil
	}
}

func declaredIdentifiers(file *ast.File) []*ast.Ident {
	var identifiers []*ast.Ident
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			identifiers = append(identifiers, declaration.Name)
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					identifiers = append(identifiers, spec.Name)
				case *ast.ValueSpec:
					identifiers = append(identifiers, spec.Names...)
				}
			}
		}
	}
	return identifiers
}

func (r *repositoryReader) applicableInstructions(paths []PathFact) []InstructionFact {
	wanted := map[string]bool{"AGENTS.md": true}
	for _, fact := range paths {
		if fact.Status != FactExists {
			continue
		}
		dir := path.Dir(fact.ResolvedPath)
		for dir != "." && dir != "/" {
			wanted[path.Join(dir, "AGENTS.md")] = true
			dir = path.Dir(dir)
		}
	}
	var instructions []InstructionFact
	for filename := range wanted {
		if entry, ok := r.entries[filename]; ok {
			instructions = append(instructions, InstructionFact{Path: filename, Blob: entry.blob})
		}
	}
	sort.Slice(instructions, func(i, j int) bool { return instructions[i].Path < instructions[j].Path })
	return instructions
}

func (r *repositoryReader) validationCommands(ctx context.Context, instructions []InstructionFact) ([]CommandFact, error) {
	seen := map[string]bool{}
	var commands []CommandFact
	for _, instruction := range instructions {
		content, err := r.blob(ctx, instruction.Blob)
		if err != nil {
			return nil, err
		}
		for _, command := range commandsFromInstructions(string(content), instruction.Path, instruction.Blob) {
			if !seen[command.Command] {
				seen[command.Command] = true
				commands = append(commands, command)
			}
		}
	}
	if makefile, ok := r.entries["Makefile"]; ok {
		content, err := r.blob(ctx, makefile.blob)
		if err != nil {
			return nil, err
		}
		for _, command := range commandsFromMakefile(string(content), makefile.blob) {
			if !seen[command.Command] {
				seen[command.Command] = true
				commands = append(commands, command)
			}
		}
	}
	sort.Slice(commands, func(i, j int) bool {
		if commands[i].Source != commands[j].Source {
			return commands[i].Source < commands[j].Source
		}
		if commands[i].Line != commands[j].Line {
			return commands[i].Line < commands[j].Line
		}
		return commands[i].Command < commands[j].Command
	})
	return commands, nil
}

func commandsFromInstructions(content, source, blob string) []CommandFact {
	lines := strings.Split(content, "\n")
	inSection, inFence := false, false
	var commands []CommandFact
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inSection = trimmed == "## Canonical commands"
			inFence = false
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if marker := strings.Index(trimmed, " #"); marker >= 0 {
			trimmed = strings.TrimSpace(trimmed[:marker])
		}
		if trimmed != "" && isValidationCommand(trimmed) {
			commands = append(commands, CommandFact{Command: trimmed, Source: source, Line: i + 1, Blob: blob})
		}
	}
	return commands
}

func commandsFromMakefile(content, blob string) []CommandFact {
	allowed := map[string]bool{
		"check": true, "test": true, "race": true, "vet": true, "lint": true,
		"fmt-check": true, "workflow-check": true, "worktree-check": true,
	}
	var commands []CommandFact
	for i, line := range strings.Split(content, "\n") {
		match := makeTargetPattern.FindStringSubmatch(line)
		if match != nil && allowed[match[1]] {
			commands = append(commands, CommandFact{Command: "make " + match[1], Source: "Makefile", Line: i + 1, Blob: blob})
		}
	}
	return commands
}

func (r *repositoryReader) blob(ctx context.Context, oid string) ([]byte, error) {
	sizeBytes, err := runGit(ctx, r.root, 128, false, "cat-file", "-s", oid)
	if err != nil {
		return nil, err
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(sizeBytes)))
	if err != nil || size < 0 || size > maxBlobBytes {
		return nil, fmt.Errorf("%w: blob %s has unsupported size", ErrBoundExceeded, oid)
	}
	return runGit(ctx, r.root, maxBlobBytes, false, "cat-file", "blob", oid)
}

func runGit(ctx context.Context, repoPath string, limit int, allowNoMatch bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", gitcmd.Argv(argv)...)
	cmd.Env = ticketprepGitEnv()
	var stdout limitedBuffer
	stdout.limit = limit
	var stderr limitedBuffer
	stderr.limit = 32 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(stdout.err, errOutputLimit) || errors.Is(stderr.err, errOutputLimit) {
		return nil, fmt.Errorf("%w: git %s exceeded its output limit", ErrBoundExceeded, args[0])
	}
	if err != nil {
		var exit *exec.ExitError
		if allowNoMatch && errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: git %s: %s", ErrRepository, args[0], strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func ticketprepGitEnv() []string {
	base := gitcmd.Env()
	env := make([]string, 0, len(base)+2)
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if key != "GIT_NO_LAZY_FETCH" && key != "GIT_NO_REPLACE_OBJECTS" {
			env = append(env, value)
		}
	}
	// Object reads are the ticketprep trust boundary. Lazy fetch would make an
	// offline read contact a promisor remote, while replacement refs could make
	// the recorded object ID describe different bytes than the bytes inspected.
	return append(env, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1")
}

var errOutputLimit = errors.New("output limit reached")

type limitedBuffer struct {
	bytes.Buffer
	limit int
	err   error
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		b.err = errOutputLimit
		return remaining, b.err
	}
	return b.Buffer.Write(p)
}

func normalizeRemote(remote string) (string, error) {
	raw := strings.TrimSpace(remote)
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return normalizeRepositoryParts(parsed.Host, parsed.EscapedPath())
	}
	colon := strings.IndexByte(raw, ':')
	if colon > 0 && !strings.Contains(raw[:colon], "/") {
		host := raw[:colon]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		return normalizeRepositoryParts(host, raw[colon+1:])
	}
	return "", fmt.Errorf("%w: origin is not a host/owner/repository remote", ErrRepository)
}

func repositoryFromIssueURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: cannot parse issue repository", ErrInvalidValue)
	}
	parts := splitPath(parsed.EscapedPath())
	if len(parts) != 4 || parts[2] != "issues" {
		return "", fmt.Errorf("%w: issue URL has no repository identity", ErrInvalidValue)
	}
	return normalizeRepositoryParts(parsed.Host, strings.Join(parts[:2], "/"))
}

func normalizeRepositoryParts(host, escapedPath string) (string, error) {
	decoded, err := url.PathUnescape(strings.Trim(escapedPath, "/"))
	if err != nil {
		return "", fmt.Errorf("%w: repository path escaping is invalid", ErrInvalidValue)
	}
	decoded = strings.TrimSuffix(decoded, ".git")
	parts := splitPath(decoded)
	if host == "" || len(parts) != 2 || parts[0] == "." || parts[1] == "." || parts[0] == ".." || parts[1] == ".." {
		return "", fmt.Errorf("%w: repository identity must be host/owner/repository", ErrInvalidValue)
	}
	identity := strings.ToLower(host) + "/" + strings.Join(parts, "/")
	if strings.EqualFold(host, "github.com") {
		identity = strings.ToLower(identity)
	}
	return identity, nil
}

func codeLiterals(text string) []string {
	seen := map[string]bool{}
	var literals []string
	for i := 0; i < len(text); {
		start := strings.IndexByte(text[i:], '`')
		if start < 0 {
			break
		}
		start += i
		if (start > 0 && text[start-1] == '`') || (start+1 < len(text) && text[start+1] == '`') {
			i = start + 1
			continue
		}
		end := strings.IndexByte(text[start+1:], '`')
		if end < 0 {
			break
		}
		end += start + 1
		literal := strings.TrimSpace(text[start+1 : end])
		if literal != "" && !strings.ContainsAny(literal, "\r\n") && !seen[literal] {
			seen[literal] = true
			literals = append(literals, literal)
		}
		i = end + 1
	}
	sort.Strings(literals)
	return literals
}

func looksLikePath(literal string) bool {
	return pathExtensionPattern.MatchString(literal) || literal == "Makefile"
}

func validRepositoryPath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\x00<>") || strings.Contains(value, "://") || path.IsAbs(value) {
		return false
	}
	return path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func looksLikeUnsupportedReference(literal string) bool {
	return strings.Contains(literal, ".") && !strings.Contains(literal, " ") || strings.ContainsAny(literal, "/<>")
}

func looksLikeGoSymbol(literal string) bool {
	if !goIdentifierPattern.MatchString(literal) {
		return false
	}
	for i, r := range literal {
		if i == 0 && r >= 'A' && r <= 'Z' || i > 0 && r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func isValidationCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return false
	}
	if fields[0] == "make" {
		return map[string]bool{
			"check": true, "test": true, "race": true, "fmt-check": true,
			"vet": true, "lint": true, "workflow-check": true, "worktree-check": true,
		}[fields[1]]
	}
	if fields[0] == "go" && (fields[1] == "test" || fields[1] == "vet") {
		return true
	}
	return len(fields) >= 5 && fields[0] == "go" && fields[1] == "run" &&
		fields[2] == "./cmd/ben" && fields[3] == "config" && fields[4] == "effective"
}

func dedupePaths(values []PathFact) []PathFact {
	return dedupeBy(values, func(value PathFact) string { return value.Reference })
}

func dedupeSymbols(values []SymbolFact) []SymbolFact {
	return dedupeBy(values, func(value SymbolFact) string { return value.Reference })
}

func dedupeUnknown(values []UnknownFact) []UnknownFact {
	return dedupeBy(values, func(value UnknownFact) string { return value.Reference })
}

func dedupeBy[T any](values []T, key func(T) string) []T {
	if len(values) < 2 {
		return values
	}
	out := values[:0]
	last := ""
	for i, value := range values {
		got := key(value)
		if i == 0 || got != last {
			out = append(out, value)
			last = got
		}
	}
	return out
}

var _ io.Writer = (*limitedBuffer)(nil)
