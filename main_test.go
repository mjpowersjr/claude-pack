package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEncodeProjectPath(t *testing.T) {
	cases := map[string]string{
		"/home/mike/github/personal/claude-pack": "-home-mike-github-personal-claude-pack",
		"/home/mike/my.project_v2":                        "-home-mike-my-project-v2",
		`C:\Users\mike\proj`:                              "C--Users-mike-proj",
		"":                                                "",
	}
	for in, want := range cases {
		if got := encodeProjectPath(in); got != want {
			t.Errorf("encodeProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	if _, err := safeJoin(base, "ok/file.txt"); err != nil {
		t.Errorf("plain relative path rejected: %v", err)
	}
	if _, err := safeJoin(base, "../escape.txt"); err == nil {
		t.Error("path traversal ../escape.txt was not rejected")
	}
	if _, err := safeJoin(base, "a/../../escape.txt"); err == nil {
		t.Error("nested traversal was not rejected")
	}
	if runtime.GOOS != "windows" {
		if _, err := safeJoin(base, "/abs/path"); err == nil {
			t.Error("absolute path was not rejected")
		}
	}
}

func TestPathRewriter(t *testing.T) {
	rw := newPathRewriter("/old/place", "/new/spot", "-old-place", "-new-spot")
	in := []byte(`{"cwd":"/old/place","scratch":"/tmp/x/-old-place/s","note":"/old/place/sub"}`)
	got := string(rw.rewrite(in))
	want := `{"cwd":"/new/spot","scratch":"/tmp/x/-new-spot/s","note":"/new/spot/sub"}`
	if got != want {
		t.Errorf("rewrite = %s, want %s", got, want)
	}
}

func TestPathRewriterWindowsEscaped(t *testing.T) {
	rw := newPathRewriter(`C:\old\proj`, `D:\new\proj`, "C--old-proj", "D--new-proj")
	in := []byte(`{"cwd":"C:\\old\\proj","raw":"C:\old\proj"}`)
	got := string(rw.rewrite(in))
	want := `{"cwd":"D:\\new\\proj","raw":"D:\new\proj"}`
	if got != want {
		t.Errorf("rewrite = %s, want %s", got, want)
	}
}

func TestPathRewriterNoOpWhenSamePath(t *testing.T) {
	rw := newPathRewriter("/same", "/same", "-same", "-same")
	in := []byte(`{"cwd":"/same"}`)
	if got := string(rw.rewrite(in)); got != string(in) {
		t.Errorf("same-path rewrite changed content: %s", got)
	}
}

// setupFixture creates a fake project directory and a fake Claude config dir
// containing one session and one memory file that reference the project path.
func setupFixture(t *testing.T) (projDir, claudeDir string) {
	t.Helper()
	root := t.TempDir()
	projDir = filepath.Join(root, "src", "myproj")
	claudeDir = filepath.Join(root, "claude")

	mustMkdir(t, filepath.Join(projDir, "sub"))
	mustWrite(t, filepath.Join(projDir, "README.md"), "hello\n")
	mustWrite(t, filepath.Join(projDir, "sub", "app.go"), "package app\n")
	mustMkdir(t, filepath.Join(projDir, "node_modules", "junk"))
	mustWrite(t, filepath.Join(projDir, "node_modules", "junk", "big.js"), "x\n")

	enc := encodeProjectPath(projDir)
	cp := filepath.Join(claudeDir, "projects", enc)
	mustMkdir(t, filepath.Join(cp, "memory"))
	mustWrite(t, filepath.Join(cp, "session-abc.jsonl"),
		`{"type":"user","cwd":"`+projDir+`","sessionId":"abc"}`+"\n")
	mustWrite(t, filepath.Join(cp, "memory", "fact.md"),
		"Project lives at "+projDir+"\n")
	return projDir, claudeDir
}

func TestExportImportRoundTrip(t *testing.T) {
	projDir, claudeDir := setupFixture(t)
	archive := filepath.Join(t.TempDir(), "out.tgz")

	if err := doExport(projDir, archive, claudeDir, []string{"node_modules"}, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	m, err := readManifest(archive)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if m.OriginalPath != projDir || len(m.SessionFiles) != 1 || m.MemoryFiles != 1 || m.ProjectFiles != 2 {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	// Import to a different path with a fresh Claude dir.
	destRoot := t.TempDir()
	dest := filepath.Join(destRoot, "elsewhere", "proj")
	newClaude := filepath.Join(destRoot, "claude2")
	if err := doImport(archive, dest, newClaude, false, false, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	if got := mustRead(t, filepath.Join(dest, "README.md")); got != "hello\n" {
		t.Errorf("README content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "node_modules")); !os.IsNotExist(err) {
		t.Error("excluded node_modules was restored")
	}

	newEnc := encodeProjectPath(dest)
	sess := mustRead(t, filepath.Join(newClaude, "projects", newEnc, "session-abc.jsonl"))
	if strings.Contains(sess, projDir) || !strings.Contains(sess, `"cwd":"`+dest+`"`) {
		t.Errorf("session cwd not rewritten: %s", sess)
	}
	mem := mustRead(t, filepath.Join(newClaude, "projects", newEnc, "memory", "fact.md"))
	if !strings.Contains(mem, dest) {
		t.Errorf("memory not rewritten: %s", mem)
	}
}

func TestExportRefusesToOverwriteArchive(t *testing.T) {
	projDir, claudeDir := setupFixture(t)
	archive := filepath.Join(t.TempDir(), "out.tgz")
	mustWrite(t, archive, "existing")
	if err := doExport(projDir, archive, claudeDir, nil, false, false); err == nil {
		t.Fatal("export overwrote an existing archive without --force")
	}
	if got := mustRead(t, archive); got != "existing" {
		t.Error("existing archive was modified")
	}
	if err := doExport(projDir, archive, claudeDir, nil, true, false); err != nil {
		t.Fatalf("export --force: %v", err)
	}
}

func TestExportSkipsOwnArchiveInsideDir(t *testing.T) {
	projDir, claudeDir := setupFixture(t)
	archive := filepath.Join(projDir, "self.tgz")
	if err := doExport(projDir, archive, claudeDir, nil, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	m, err := readManifest(archive)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	for _, s := range m.SessionFiles {
		if s == "self.tgz" {
			t.Error("archive bundled itself")
		}
	}
}

func TestImportRefusesNonEmptyDest(t *testing.T) {
	projDir, claudeDir := setupFixture(t)
	archive := filepath.Join(t.TempDir(), "out.tgz")
	if err := doExport(projDir, archive, claudeDir, nil, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, "precious.txt"), "keep me")
	if err := doImport(archive, dest, filepath.Join(t.TempDir(), "c"), false, false, false); err == nil {
		t.Fatal("import into non-empty destination did not error")
	}
	if got := mustRead(t, filepath.Join(dest, "precious.txt")); got != "keep me" {
		t.Error("existing file was clobbered")
	}
}

func TestImportSkipsExistingSessionsWithoutForce(t *testing.T) {
	projDir, claudeDir := setupFixture(t)
	archive := filepath.Join(t.TempDir(), "out.tgz")
	if err := doExport(projDir, archive, claudeDir, nil, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	destRoot := t.TempDir()
	dest := filepath.Join(destRoot, "proj")
	newClaude := filepath.Join(destRoot, "claude2")

	// Pre-plant a session with the same name at the destination encoding.
	newEnc := encodeProjectPath(dest)
	planted := filepath.Join(newClaude, "projects", newEnc, "session-abc.jsonl")
	mustMkdir(t, filepath.Dir(planted))
	mustWrite(t, planted, "mine\n")

	if err := doImport(archive, dest, newClaude, false, false, false); err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := mustRead(t, planted); got != "mine\n" {
		t.Error("existing session was clobbered without --force")
	}
	if err := doImport(archive, dest, newClaude, true /*skipProject*/, true /*force*/, false); err != nil {
		t.Fatalf("import --force: %v", err)
	}
	if got := mustRead(t, planted); got == "mine\n" {
		t.Error("--force did not overwrite the session")
	}
}

func TestImportRejectsTraversalArchive(t *testing.T) {
	// Hand-craft a malicious archive whose project entry escapes the dest.
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tgz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	manifest := `{"formatVersion":1,"toolVersion":"t","createdAt":"2026-01-01T00:00:00Z","originalPath":"/x","encodedName":"-x"}`
	if err := writeTarBytes(tw, manifestName, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTarBytes(tw, projectPrefix+"../../evil.txt", []byte("pwned"), 0o644); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	dest := filepath.Join(dir, "dest")
	if err := doImport(archive, dest, filepath.Join(dir, "c"), false, false, false); err == nil {
		t.Fatal("traversal entry was not rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(err) {
		t.Error("traversal file was written outside destination")
	}
}

func TestInspectRejectsGarbage(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.tgz")
	mustWrite(t, bad, "garbage")
	if _, err := readManifest(bad); err == nil {
		t.Fatal("garbage file accepted as archive")
	}
}

func TestExportWithNoClaudeData(t *testing.T) {
	// A directory with no sessions/memories should still export (with warning).
	root := t.TempDir()
	projDir := filepath.Join(root, "plain")
	mustMkdir(t, projDir)
	mustWrite(t, filepath.Join(projDir, "f.txt"), "x")
	archive := filepath.Join(root, "out.tgz")
	if err := doExport(projDir, archive, filepath.Join(root, "no-claude"), nil, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	m, err := readManifest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SessionFiles) != 0 || m.ProjectFiles != 1 {
		t.Errorf("unexpected manifest: %+v", m)
	}
}

func TestSymlinkRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	projDir, claudeDir := setupFixture(t)
	if err := os.Symlink("README.md", filepath.Join(projDir, "link.md")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "out.tgz")
	if err := doExport(projDir, archive, claudeDir, nil, false, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "proj")
	if err := doImport(archive, dest, filepath.Join(t.TempDir(), "c"), false, false, false); err != nil {
		t.Fatalf("import: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dest, "link.md"))
	if err != nil || target != "README.md" {
		t.Errorf("symlink not preserved: target=%q err=%v", target, err)
	}
}

// --- helpers ---

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
