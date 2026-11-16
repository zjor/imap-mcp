// Package archtest holds repository-wide architecture guards.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// eachImport calls fn for every import of every .go file under root.
func eachImport(t *testing.T, root string, fn func(file, path string)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == ".superpowers" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			fn(filepath.ToSlash(rel), path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoSMTP(t *testing.T) {
	root := repoRoot(t)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(mod)), "smtp") {
		t.Error("go.mod mentions smtp")
	}
	eachImport(t, root, func(file, path string) {
		if strings.Contains(strings.ToLower(path), "smtp") {
			t.Errorf("%s imports %s", file, path)
		}
	})
}

func TestOnlyImapsafeImportsIMAPClient(t *testing.T) {
	root := repoRoot(t)
	eachImport(t, root, func(file, path string) {
		if path != "github.com/emersion/go-imap/v2/imapclient" {
			return
		}
		// Integration tests under test/ seed mailboxes with a raw client.
		if !strings.HasPrefix(file, "internal/imapsafe/") && !strings.HasPrefix(file, "test/") {
			t.Errorf("%s imports imapclient; only internal/imapsafe may", file)
		}
	})
}
