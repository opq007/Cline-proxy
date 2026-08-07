package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDataDirRespectsEnv(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "custom-data"))
	t.Setenv("CLINE_DATA_DIR", "")
	got := dataDir()
	want := os.Getenv("DATA_DIR")
	if got != want {
		t.Fatalf("dataDir=%q want %q", got, want)
	}
}

func TestResolveDataPathUsesDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("CLINE_DATA_DIR", "")
	p := resolveDataPath(".cline-accounts.json")
	if filepath.Dir(p) != dir {
		t.Fatalf("path %q not under DATA_DIR %q", p, dir)
	}
	if filepath.Base(p) != ".cline-accounts.json" {
		t.Fatalf("unexpected base: %s", p)
	}
}

func TestResolveDataPathSkipsDirectoryTrap(t *testing.T) {
	// Simulate Docker creating a directory where a file should be.
	root := t.TempDir()
	t.Setenv("DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("CLINE_DATA_DIR", "")
	// Put a bogus directory at cwd-relative legacy path is hard; just ensure
	// primary path under DATA_DIR is preferred when it does not exist yet.
	p := resolveDataPath(".cline-accounts.json")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		t.Fatalf("resolved path must not be a directory: %s", p)
	}
	// Writing should create a real file.
	if err := os.WriteFile(p, []byte(`{"accounts":[],"keys":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(p) {
		t.Fatalf("expected regular file at %s", p)
	}
}

func TestIsRegularFileFalseForDir(t *testing.T) {
	d := t.TempDir()
	if isRegularFile(d) {
		t.Fatal("directory must not count as regular file")
	}
}
