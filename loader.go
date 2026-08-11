package migratekit

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// LoadFromFS loads migrations from an embedded filesystem and verifies the
// parent-link chain with default options (headerless migrations tolerated with
// a warning). It is Load(fsys, dir) — use Load directly for
// RequireParentLinks or a custom warning sink.
// If dir is empty, defaults to "." (root of the filesystem).
func LoadFromFS(fsys fs.FS, dir ...string) ([]Migration, error) {
	directory := "."
	if len(dir) > 0 && dir[0] != "" {
		directory = dir[0]
	}
	return Load(fsys, directory)
}

// loadFiles reads all .up.sql files, ordered by numeric prefix (so unpadded
// names like 2_x and 10_x apply in numeric order), falling back to filename
// order for non-numeric names. Returns an error if two files normalize to the
// same Prefix() — tracking is prefix-keyed, so a duplicate prefix would
// silently skip the second file as "already applied". It does NOT verify the
// parent-link chain; that is Load's job.
func loadFiles(fsys fs.FS, directory string) ([]Migration, error) {
	if directory == "" {
		directory = "."
	}
	entries, err := fs.ReadDir(fsys, directory)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", directory, err)
	}

	var migrations []Migration
	seen := map[string]string{} // normalized prefix -> filename
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}

		prefix := Prefix(entry.Name())
		if prior, dup := seen[prefix]; dup {
			return nil, fmt.Errorf("duplicate migration prefix %q: %s and %s (tracking is prefix-keyed; the second file would be silently skipped)", prefix, prior, entry.Name())
		}
		seen[prefix] = entry.Name()

		// Construct path correctly for embed.FS
		// embed.FS doesn't accept "./" prefix, so handle "." specially
		var path string
		if directory == "." {
			path = entry.Name()
		} else {
			path = directory + "/" + entry.Name()
		}

		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		// Store raw content; template substitution is applied at execution time.
		migrations = append(migrations, Migration{
			Name:    entry.Name(),
			Content: string(content),
		})
	}

	// Numeric prefixes sort numerically (1, 2, 10 — not 1, 10, 2);
	// non-numeric names sort after numeric ones, lexically.
	sort.Slice(migrations, func(i, j int) bool {
		ni, iNum := numericPrefix(migrations[i].Name)
		nj, jNum := numericPrefix(migrations[j].Name)
		switch {
		case iNum && jNum:
			if ni != nj {
				return ni < nj
			}
			return migrations[i].Name < migrations[j].Name
		case iNum:
			return true
		case jNum:
			return false
		default:
			return migrations[i].Name < migrations[j].Name
		}
	})

	return migrations, nil
}

// numericPrefix parses the normalized Prefix() of a migration filename as an
// integer. ok is false for names without a numeric prefix.
func numericPrefix(name string) (n int64, ok bool) {
	n, err := strconv.ParseInt(Prefix(name), 10, 64)
	return n, err == nil
}
