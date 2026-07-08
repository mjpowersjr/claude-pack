// claude-pack: bundle a project directory together with its Claude Code
// sessions and memories into a single archive, and restore it on the same
// or another machine — even when the project lands at a different path.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	toolVersion   = "1.0.0"
	formatVersion = 1
	manifestName  = "manifest.json"
	projectPrefix = "project/"
	sessionPrefix = "claude/sessions/"
	memoryPrefix  = "claude/memory/"
)

// Manifest describes the contents of an archive.
type Manifest struct {
	FormatVersion int       `json:"formatVersion"`
	ToolVersion   string    `json:"toolVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	Hostname      string    `json:"hostname"`
	OriginalPath  string    `json:"originalPath"`
	EncodedName   string    `json:"encodedName"`
	SessionFiles  []string  `json:"sessionFiles"`
	MemoryFiles   int       `json:"memoryFiles"`
	ProjectFiles  int       `json:"projectFiles"`
	SessionsOnly  bool      `json:"sessionsOnly,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		if isTTY() {
			return runInteractive()
		}
		printRootUsage()
		return errors.New("no command given (run with no arguments in a terminal for interactive mode)")
	}
	switch args[0] {
	case "export", "pack":
		return cmdExport(args[1:])
	case "import", "unpack":
		return cmdImport(args[1:])
	case "inspect", "info":
		return cmdInspect(args[1:])
	case "interactive", "-i", "--interactive":
		return runInteractive()
	case "version", "--version", "-v":
		fmt.Println("claude-pack", toolVersion)
		return nil
	case "help", "--help", "-h":
		printRootUsage()
		return nil
	default:
		printRootUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printRootUsage() {
	fmt.Print(`claude-pack — bundle a directory plus its Claude Code sessions & memories

Usage:
  claude-pack <command> [options]
  claude-pack                          (no args in a terminal: interactive mode)

Commands:
  export     Bundle a directory + its Claude sessions/memories into an archive
  import     Restore an archive (project dir + sessions + memories)
  inspect    Show what's inside an archive without extracting anything
  version    Print version
  help       Show this help

Run "claude-pack <command> --help" for command-specific options.

Examples:
  claude-pack export --dir ~/code/myproj --output myproj.claude.tgz
  claude-pack import --archive myproj.claude.tgz --dest ~/elsewhere/myproj
  claude-pack inspect --archive myproj.claude.tgz
`)
}

// ---------------------------------------------------------------------------
// export

func cmdExport(args []string) error {
	fl := newFlagSet("export", `Bundle a directory and its Claude Code sessions/memories into a .tgz archive
(or, with --sessions-only, just the Claude data for that directory).

Usage: claude-pack export [options]

Options:
  --dir PATH         Project directory to bundle (default: current directory)
  --output PATH      Archive to write (default: <dirname>-<date>.claude.tgz)
  --claude-dir PATH  Claude config dir (default: $CLAUDE_CONFIG_DIR or ~/.claude)
  --exclude PATTERN  Glob of path segments to skip, repeatable (e.g. node_modules)
  --sessions-only    Bundle only the Claude sessions/memories, not the directory
  --force            Overwrite the output archive if it already exists
`)
	var opts struct {
		dir, output, claudeDir string
		excludes               multiFlag
		sessionsOnly, force    bool
	}
	fl.StringVar(&opts.dir, "dir", ".", "")
	fl.StringVar(&opts.output, "output", "", "")
	fl.StringVar(&opts.claudeDir, "claude-dir", "", "")
	fl.Var(&opts.excludes, "exclude", "")
	fl.BoolVar(&opts.sessionsOnly, "sessions-only", false, "")
	fl.BoolVar(&opts.force, "force", false, "")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (did you mean --dir?)", fl.Arg(0))
	}
	return doExport(opts.dir, opts.output, opts.claudeDir, opts.excludes, opts.sessionsOnly, opts.force, false)
}

func doExport(dir, output, claudeDir string, excludes []string, sessionsOnly, force, interactive bool) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absDir); err != nil {
		return fmt.Errorf("project directory: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", absDir)
	}

	cDir, err := resolveClaudeDir(claudeDir)
	if err != nil {
		return err
	}
	encoded := encodeProjectPath(absDir)
	claudeProjDir := filepath.Join(cDir, "projects", encoded)

	sessions, memFiles, err := collectClaudeData(claudeProjDir)
	if err != nil {
		return err
	}
	if len(sessions) == 0 && len(memFiles) == 0 {
		if sessionsOnly {
			return fmt.Errorf("nothing to export: no Claude sessions or memories found for %s (looked in %s)", absDir, claudeProjDir)
		}
		fmt.Fprintf(os.Stderr, "warning: no Claude sessions or memories found for %s (looked in %s)\n", absDir, claudeProjDir)
		if interactive && !promptYesNo("Continue and bundle just the directory?", true) {
			return errors.New("aborted")
		}
	}

	if output == "" {
		output = fmt.Sprintf("%s-%s.claude.tgz", filepath.Base(absDir), time.Now().Format("20060102-150405"))
	}
	absOutput, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absOutput); err == nil {
		if interactive && !force {
			if !promptYesNo(fmt.Sprintf("%s already exists. Overwrite?", absOutput), false) {
				return errors.New("aborted; will not overwrite existing archive")
			}
		} else if !force {
			return fmt.Errorf("output %s already exists (use --force to overwrite)", absOutput)
		}
	}

	// Write to a temp file in the destination directory, rename on success,
	// so a failed export never leaves a truncated archive behind.
	tmp, err := os.CreateTemp(filepath.Dir(absOutput), ".claude-pack-*.tgz")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	projCount, err := writeArchive(tmp, absDir, absOutput, encoded, sessions, memFiles, claudeProjDir, excludes, sessionsOnly)
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmpName, absOutput); err != nil {
		return err
	}

	fmt.Printf("Exported %s\n", absDir)
	if sessionsOnly {
		fmt.Printf("  project files : none (--sessions-only)\n")
	} else {
		fmt.Printf("  project files : %d\n", projCount)
	}
	fmt.Printf("  sessions      : %d\n", len(sessions))
	fmt.Printf("  memory files  : %d\n", len(memFiles))
	fmt.Printf("  archive       : %s\n", absOutput)
	return nil
}

func collectClaudeData(claudeProjDir string) (sessions, memFiles []string, err error) {
	entries, err := os.ReadDir(claudeProjDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			sessions = append(sessions, filepath.Join(claudeProjDir, e.Name()))
		}
	}
	memDir := filepath.Join(claudeProjDir, "memory")
	_ = filepath.WalkDir(memDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		memFiles = append(memFiles, p)
		return nil
	})
	sort.Strings(sessions)
	sort.Strings(memFiles)
	return sessions, memFiles, nil
}

func writeArchive(w io.Writer, absDir, absOutput, encoded string, sessions, memFiles []string, claudeProjDir string, excludes []string, sessionsOnly bool) (projCount int, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	sessionNames := make([]string, len(sessions))
	for i, s := range sessions {
		sessionNames[i] = filepath.Base(s)
	}
	host, _ := os.Hostname()
	manifest := Manifest{
		FormatVersion: formatVersion,
		ToolVersion:   toolVersion,
		CreatedAt:     time.Now().UTC(),
		Hostname:      host,
		OriginalPath:  absDir,
		EncodedName:   encoded,
		SessionFiles:  sessionNames,
		MemoryFiles:   len(memFiles),
		SessionsOnly:  sessionsOnly,
	}

	// Project files. Walk first so the manifest can carry the count; buffer
	// entries would cost memory, so instead write manifest with count filled
	// in after a pre-count pass (cheap: metadata only).
	var projFiles []string
	if !sessionsOnly {
		err = filepath.WalkDir(absDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == absDir {
				return nil
			}
			rel, _ := filepath.Rel(absDir, p)
			if p == absOutput { // don't bundle the archive into itself
				return nil
			}
			for _, pat := range excludes {
				for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
					if ok, _ := path.Match(pat, seg); ok {
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
				}
			}
			projFiles = append(projFiles, p)
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("scanning project directory: %w", err)
		}
	}

	countFiles := 0
	for _, p := range projFiles {
		if info, err := os.Lstat(p); err == nil && !info.IsDir() {
			countFiles++
		}
	}
	manifest.ProjectFiles = countFiles

	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeTarBytes(tw, manifestName, mb, 0o644); err != nil {
		return 0, err
	}

	for _, p := range projFiles {
		rel, _ := filepath.Rel(absDir, p)
		if err := addPathToTar(tw, p, projectPrefix+filepath.ToSlash(rel)); err != nil {
			return 0, fmt.Errorf("adding %s: %w", rel, err)
		}
	}
	for _, s := range sessions {
		if err := addPathToTar(tw, s, sessionPrefix+filepath.Base(s)); err != nil {
			return 0, fmt.Errorf("adding session %s: %w", filepath.Base(s), err)
		}
	}
	memRoot := filepath.Join(claudeProjDir, "memory")
	for _, m := range memFiles {
		rel, _ := filepath.Rel(memRoot, m)
		if err := addPathToTar(tw, m, memoryPrefix+filepath.ToSlash(rel)); err != nil {
			return 0, fmt.Errorf("adding memory %s: %w", rel, err)
		}
	}

	if err := tw.Close(); err != nil {
		return 0, err
	}
	return countFiles, gz.Close()
}

func addPathToTar(tw *tar.Writer, src, name string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(src); err != nil {
			return err
		}
	}
	switch {
	case info.Mode().IsRegular(), info.IsDir(), info.Mode()&os.ModeSymlink != 0:
	default:
		fmt.Fprintf(os.Stderr, "warning: skipping %s (unsupported file type)\n", src)
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Format = tar.FormatPAX
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
	}
	return nil
}

func writeTarBytes(tw *tar.Writer, name string, data []byte, mode int64) error {
	hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Now(), Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// ---------------------------------------------------------------------------
// import

func cmdImport(args []string) error {
	fl := newFlagSet("import", `Restore an archive: extract the project directory and install its Claude
sessions/memories, rewriting old paths to the new location.

Usage: claude-pack import --archive PATH [options]

Options:
  --archive PATH     Archive to restore (required)
  --dest PATH        Where to place the project directory
                     (default: ./<original directory name>)
  --claude-dir PATH  Claude config dir (default: $CLAUDE_CONFIG_DIR or ~/.claude)
  --sessions-only    Only restore sessions/memories, not the directory itself
                     (--skip-project is an alias; --dest still names the
                     project directory the sessions should attach to)
  --force            Overwrite existing project files, sessions, and memories
`)
	var opts struct {
		archive, dest, claudeDir string
		skipProject, force       bool
	}
	fl.StringVar(&opts.archive, "archive", "", "")
	fl.StringVar(&opts.dest, "dest", "", "")
	fl.StringVar(&opts.claudeDir, "claude-dir", "", "")
	fl.BoolVar(&opts.skipProject, "skip-project", false, "")
	fl.BoolVar(&opts.skipProject, "sessions-only", false, "")
	fl.BoolVar(&opts.force, "force", false, "")
	// Allow: claude-pack import foo.tgz [--flags] (flag parsing stops at the
	// first positional, so pull a leading archive path out before parsing).
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.archive = args[0]
		args = args[1:]
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if opts.archive == "" && fl.NArg() == 1 {
		opts.archive = fl.Arg(0)
	} else if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	if opts.archive == "" {
		return errors.New("--archive is required (see claude-pack import --help)")
	}
	return doImport(opts.archive, opts.dest, opts.claudeDir, opts.skipProject, opts.force, false)
}

func doImport(archive, dest, claudeDir string, skipProject, force, interactive bool) error {
	manifest, err := readManifest(archive)
	if err != nil {
		return err
	}
	if manifest.FormatVersion > formatVersion {
		return fmt.Errorf("archive format v%d is newer than this tool understands (v%d); upgrade claude-pack", manifest.FormatVersion, formatVersion)
	}
	if manifest.SessionsOnly {
		// The archive has no project files; behave as --sessions-only so a
		// non-empty destination (the project usually already exists) is fine.
		skipProject = true
	}

	if dest == "" {
		def := filepath.Base(filepath.FromSlash(strings.ReplaceAll(manifest.OriginalPath, `\`, `/`)))
		if interactive {
			dest = promptString(fmt.Sprintf("Destination directory for the project"), "./"+def)
		} else {
			dest = "./" + def
		}
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	cDir, err := resolveClaudeDir(claudeDir)
	if err != nil {
		return err
	}

	// Safety: never silently merge into a non-empty destination.
	if !skipProject {
		if entries, err := os.ReadDir(absDest); err == nil && len(entries) > 0 && !force {
			if interactive {
				if !promptYesNo(fmt.Sprintf("%s exists and is not empty. Extract into it anyway (existing files with the same names are NOT overwritten)?", absDest), false) {
					return errors.New("aborted")
				}
			} else {
				return fmt.Errorf("destination %s exists and is not empty (use --force to overwrite, or pick another --dest)", absDest)
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	newEncoded := encodeProjectPath(absDest)
	claudeProjDir := filepath.Join(cDir, "projects", newEncoded)
	rw := newPathRewriter(manifest.OriginalPath, absDest, manifest.EncodedName, newEncoded)

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s does not look like a claude-pack archive: %w", archive, err)
	}
	tr := tar.NewReader(gz)

	var stats struct{ proj, sess, mem, skipped int }
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		// Prefix checks run on the raw name so traversal attempts like
		// "project/../../x" reach safeJoin and are rejected, not skipped.
		name := hdr.Name
		if path.Clean(name) == manifestName {
			continue
		}
		switch {
		case strings.HasPrefix(name, projectPrefix):
			if skipProject {
				continue
			}
			rel := strings.TrimPrefix(name, projectPrefix)
			target, err := safeJoin(absDest, rel)
			if err != nil {
				return err
			}
			wrote, err := extractEntry(tr, hdr, target, force)
			if err != nil {
				return fmt.Errorf("extracting %s: %w", rel, err)
			}
			if wrote {
				if !hdr.FileInfo().IsDir() {
					stats.proj++
				}
			} else {
				stats.skipped++
				fmt.Fprintf(os.Stderr, "warning: kept existing %s (use --force to overwrite)\n", target)
			}
		case strings.HasPrefix(name, sessionPrefix):
			base := path.Base(strings.TrimPrefix(name, sessionPrefix))
			target := filepath.Join(claudeProjDir, base)
			wrote, err := writeRewritten(tr, target, rw, force, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return fmt.Errorf("restoring session %s: %w", base, err)
			}
			if wrote {
				stats.sess++
			} else {
				stats.skipped++
				fmt.Fprintf(os.Stderr, "warning: session %s already exists, kept the existing one (use --force to overwrite)\n", target)
			}
		case strings.HasPrefix(name, memoryPrefix):
			rel := strings.TrimPrefix(name, memoryPrefix)
			target, err := safeJoin(filepath.Join(claudeProjDir, "memory"), rel)
			if err != nil {
				return err
			}
			if hdr.FileInfo().IsDir() {
				if err := os.MkdirAll(target, 0o755); err != nil {
					return err
				}
				continue
			}
			wrote, err := writeRewritten(tr, target, rw, force, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return fmt.Errorf("restoring memory %s: %w", rel, err)
			}
			if wrote {
				stats.mem++
			} else {
				stats.skipped++
				fmt.Fprintf(os.Stderr, "warning: memory file %s already exists, kept the existing one (use --force to overwrite)\n", target)
			}
		default:
			fmt.Fprintf(os.Stderr, "warning: ignoring unknown archive entry %q\n", hdr.Name)
		}
	}

	fmt.Printf("Imported archive from %s (created %s on %s)\n", manifest.OriginalPath, manifest.CreatedAt.Format("2006-01-02"), manifest.Hostname)
	if !skipProject {
		fmt.Printf("  project       : %s (%d files)\n", absDest, stats.proj)
	}
	fmt.Printf("  sessions      : %d restored -> %s\n", stats.sess, claudeProjDir)
	fmt.Printf("  memory files  : %d restored\n", stats.mem)
	if stats.skipped > 0 {
		fmt.Printf("  skipped       : %d existing files kept (re-run with --force to overwrite)\n", stats.skipped)
	}
	if manifest.OriginalPath != absDest {
		fmt.Printf("  paths rewritten: %s -> %s\n", manifest.OriginalPath, absDest)
	}
	fmt.Printf("\nNext: cd %s && claude --resume\n", absDest)
	return nil
}

func readManifest(archive string) (*Manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s does not look like a claude-pack archive (not gzip): %w", archive, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s — not a claude-pack archive", archive, manifestName)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(hdr.Name) == manifestName {
			var m Manifest
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
				return nil, fmt.Errorf("corrupt manifest: %w", err)
			}
			if m.OriginalPath == "" || m.EncodedName == "" {
				return nil, errors.New("manifest is missing required fields")
			}
			return &m, nil
		}
	}
}

// extractEntry writes a project entry to target. Returns false if the target
// already existed and force is off (file kept, not an error).
func extractEntry(r io.Reader, hdr *tar.Header, target string, force bool) (bool, error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return true, os.MkdirAll(target, hdr.FileInfo().Mode().Perm()|0o200)
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if _, err := os.Lstat(target); err == nil {
			if !force {
				return false, nil
			}
			os.Remove(target)
		}
		return true, os.Symlink(hdr.Linkname, target)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		out, err := os.OpenFile(target, flags, hdr.FileInfo().Mode().Perm()|0o200)
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if _, err := io.Copy(out, r); err != nil {
			out.Close()
			return false, err
		}
		return true, out.Close()
	default:
		fmt.Fprintf(os.Stderr, "warning: skipping unsupported entry type for %s\n", hdr.Name)
		return true, nil
	}
}

// ---------------------------------------------------------------------------
// path rewriting

type pathRewriter struct{ pairs [][2][]byte }

// newPathRewriter builds the old->new replacements applied to session and
// memory files: the raw path, its JSON-escaped form (matters for Windows
// backslashes), and the encoded ~/.claude/projects directory name.
func newPathRewriter(oldPath, newPath, oldEncoded, newEncoded string) *pathRewriter {
	rw := &pathRewriter{}
	add := func(o, n string) {
		if o != "" && o != n {
			rw.pairs = append(rw.pairs, [2][]byte{[]byte(o), []byte(n)})
		}
	}
	jsonEsc := func(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
	add(jsonEsc(oldPath), jsonEsc(newPath)) // JSON-escaped first (superset match)
	add(oldPath, newPath)
	add(oldEncoded, newEncoded)
	return rw
}

func (rw *pathRewriter) rewrite(data []byte) []byte {
	for _, p := range rw.pairs {
		data = bytes.ReplaceAll(data, p[0], p[1])
	}
	return data
}

// writeRewritten reads all of r, applies path rewriting, and writes target,
// refusing to clobber an existing file unless force. Returns whether written.
func writeRewritten(r io.Reader, target string, rw *pathRewriter, force bool, perm fs.FileMode) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	if _, err := os.Lstat(target); err == nil && !force {
		return false, nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	if perm == 0 {
		perm = 0o644
	}
	return true, os.WriteFile(target, rw.rewrite(data), perm)
}

// ---------------------------------------------------------------------------
// inspect

func cmdInspect(args []string) error {
	fl := newFlagSet("inspect", `Show archive metadata and contents without extracting anything.

Usage: claude-pack inspect --archive PATH
       claude-pack inspect PATH

Options:
  --archive PATH   Archive to inspect
`)
	var archive string
	fl.StringVar(&archive, "archive", "", "")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		archive = args[0]
		args = args[1:]
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if archive == "" && fl.NArg() == 1 {
		archive = fl.Arg(0)
	}
	if archive == "" {
		return errors.New("--archive is required")
	}
	m, err := readManifest(archive)
	if err != nil {
		return err
	}
	fmt.Printf("Archive        : %s\n", archive)
	fmt.Printf("Created        : %s on %q\n", m.CreatedAt.Format(time.RFC1123), m.Hostname)
	fmt.Printf("Tool / format  : claude-pack %s / v%d\n", m.ToolVersion, m.FormatVersion)
	fmt.Printf("Original path  : %s\n", m.OriginalPath)
	if m.SessionsOnly {
		fmt.Printf("Project files  : none (sessions-only archive)\n")
	} else {
		fmt.Printf("Project files  : %d\n", m.ProjectFiles)
	}
	fmt.Printf("Memory files   : %d\n", m.MemoryFiles)
	fmt.Printf("Sessions       : %d\n", len(m.SessionFiles))
	for _, s := range m.SessionFiles {
		fmt.Printf("  - %s\n", s)
	}
	return nil
}

// ---------------------------------------------------------------------------
// interactive mode

func runInteractive() error {
	fmt.Println("claude-pack", toolVersion, "— interactive mode")
	fmt.Println()
	mode := promptChoice("What would you like to do?", []string{"export (bundle a directory + Claude data)", "import (restore an archive)", "inspect (peek inside an archive)"})
	switch mode {
	case 0:
		cwd, _ := os.Getwd()
		dir := promptString("Directory to bundle", cwd)
		output := promptString("Archive to write (blank = auto-named in current dir)", "")
		sessionsOnly := !promptYesNo("Include the directory contents? (No = Claude sessions/memories only)", true)
		var excludes []string
		if !sessionsOnly {
			excl := promptString("Exclude patterns, comma-separated (blank = none, e.g. node_modules,.git)", "")
			for _, e := range strings.Split(excl, ",") {
				if e = strings.TrimSpace(e); e != "" {
					excludes = append(excludes, e)
				}
			}
		}
		return doExport(dir, output, "", excludes, sessionsOnly, false, true)
	case 1:
		archive := promptString("Archive to import", "")
		if archive == "" {
			return errors.New("an archive path is required")
		}
		m, err := readManifest(archive)
		if err != nil {
			return err
		}
		fmt.Printf("  (original location: %s, %d sessions, %d memory files)\n", m.OriginalPath, len(m.SessionFiles), m.MemoryFiles)
		return doImport(archive, "", "", false, false, true)
	default:
		archive := promptString("Archive to inspect", "")
		return cmdInspect([]string{"--archive", archive})
	}
}

var stdinReader = bufio.NewReader(os.Stdin)

func promptString(question, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", question, def)
	} else {
		fmt.Printf("%s: ", question)
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func promptYesNo(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	ans := strings.ToLower(promptString(fmt.Sprintf("%s (%s)", question, hint), ""))
	switch ans {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

func promptChoice(question string, options []string) int {
	fmt.Println(question)
	for i, o := range options {
		fmt.Printf("  %d) %s\n", i+1, o)
	}
	for {
		ans := promptString("Choice", "1")
		var n int
		if _, err := fmt.Sscanf(ans, "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		fmt.Printf("Please enter a number between 1 and %d.\n", len(options))
	}
}

// ---------------------------------------------------------------------------
// helpers

// encodeProjectPath mirrors Claude Code's project-directory naming:
// every non-alphanumeric character of the absolute path becomes '-'.
func encodeProjectPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func resolveClaudeDir(flagVal string) (string, error) {
	if flagVal != "" {
		return filepath.Abs(flagVal)
	}
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w (use --claude-dir)", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func newFlagSet(name, usage string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(os.Stderr)
	fl.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	return fl
}

// safeJoin joins rel onto base, rejecting entries that would escape base
// (protection against malicious archives).
func safeJoin(base, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("archive entry has absolute path %q", rel)
	}
	target := filepath.Join(base, rel)
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes destination directory", rel)
	}
	return target, nil
}

func isTTY() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }
