// Package store implements the on-disk blob cache: chunked part files that can
// survive interrupted runs, plus assembly and sha256 verification.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrMismatch means the assembled bytes do not match the expected digest.
var ErrMismatch = errors.New("digest mismatch")

// Store is a blob cache rooted at a directory.
type Store struct {
	Root      string
	KeepParts bool // keep part files after assembly (saves re-download at the cost of disk)
	Verbose   bool
}

// Meta describes an in-progress chunked download.
type Meta struct {
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	ChunkSize int64     `json:"chunk_size"`
	Count     int       `json:"count"`
	Unknown   bool      `json:"unknown_size,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// New prepares a cache directory.
func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s := &Store{Root: abs}
	for _, d := range []string{"blobs", "parts"} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SplitDigest splits "sha256:abcd" into its algorithm and hex parts.
func SplitDigest(d string) (algo, hexStr string, err error) {
	i := strings.Index(d, ":")
	if i <= 0 {
		return "", "", fmt.Errorf("malformed digest %q", d)
	}
	return d[:i], d[i+1:], nil
}

// BlobPath returns the final location of a verified blob.
func (s *Store) BlobPath(digest string) string {
	algo, hexStr, err := SplitDigest(digest)
	if err != nil {
		return filepath.Join(s.Root, "blobs", "invalid")
	}
	return filepath.Join(s.Root, "blobs", algo, hexStr)
}

// PartsDir returns the directory holding part files of a digest.
func (s *Store) PartsDir(digest string) string {
	algo, hexStr, err := SplitDigest(digest)
	if err != nil {
		return filepath.Join(s.Root, "parts", "invalid")
	}
	return filepath.Join(s.Root, "parts", algo+"-"+hexStr[:min(len(hexStr), 24)])
}

// HasBlob reports whether a verified blob is present. When verify is true the
// content hash is checked, which costs a full read.
func (s *Store) HasBlob(digest string, size int64, verify bool) bool {
	p := s.BlobPath(digest)
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		return false
	}
	if size >= 0 && st.Size() != size {
		return false
	}
	if !verify {
		return true
	}
	return s.Verify(digest) == nil
}

// DropBlob removes a stored blob (used by --force).
func (s *Store) DropBlob(digest string) error {
	err := os.Remove(s.BlobPath(digest))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PurgeParts deletes all part files of a digest and returns the (empty) dir.
func (s *Store) PurgeParts(digest string) (string, error) {
	dir := s.PartsDir(digest)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Verify checks a stored blob against its digest.
func (s *Store) Verify(digest string) error {
	algo, want, err := SplitDigest(digest)
	if err != nil {
		return err
	}
	if algo != "sha256" {
		return fmt.Errorf("unsupported digest algorithm %q", algo)
	}
	f, err := os.Open(s.BlobPath(digest))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrMismatch
	}
	return nil
}

// writeMeta persists the plan of a chunked download.
func (s *Store) writeMeta(dir string, m Meta) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

// LoadMeta reads the plan of an in-progress download.
func (s *Store) LoadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// PreparePartsDir creates (or validates) the part directory for a download and
// reports which chunks are already complete.
func (s *Store) PreparePartsDir(digest string, want Meta) (dir string, done map[int]bool, err error) {
	dir = s.PartsDir(digest)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	done = map[int]bool{}
	if existing, err := s.LoadMeta(dir); err == nil {
		if existing.Digest != want.Digest || existing.Count != want.Count ||
			existing.ChunkSize != want.ChunkSize || existing.Size != want.Size {
			// plan changed: parts are not reusable
			if err := s.dropParts(dir); err != nil {
				return "", nil, err
			}
			done = map[int]bool{}
		}
	}
	if err := s.writeMeta(dir, want); err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".ok") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSuffix(name, ".ok"))
		if err != nil || idx < 0 || idx >= want.Count {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		rec := strings.Fields(string(b))
		if len(rec) != 2 {
			continue
		}
		var size int64
		if _, err := fmt.Sscanf(rec[1], "%d", &size); err != nil {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, partName(idx)))
		if err != nil || st.Size() != size {
			continue
		}
		if rec[0] != "sha256:"+sha256File(filepath.Join(dir, partName(idx))) {
			continue
		}
		done[idx] = true
	}
	return dir, done, nil
}

func (s *Store) dropParts(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

func partName(i int) string { return fmt.Sprintf("%06d.part", i) }

func okName(i int) string { return fmt.Sprintf("%06d.ok", i) }

// PartPath returns the data file path of a chunk.
func PartPath(dir string, i int) string { return filepath.Join(dir, partName(i)) }

// CommitPart stores the verification marker for a finished chunk.
func CommitPart(dir string, i int, size int64, shaHex string) error {
	tmp := filepath.Join(dir, okName(i)+".tmp")
	if err := os.WriteFile(tmp, []byte("sha256:"+shaHex+" "+strconv.FormatInt(size, 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, okName(i)))
}

// Assemble concatenates the part files into the final blob, verifies the
// digest and (unless KeepParts) removes the part files.
func (s *Store) Assemble(digest string, count int, size int64) (string, error) {
	dir := s.PartsDir(digest)
	final := s.BlobPath(digest)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}
	tmp := final + ".assembling"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var written int64
	buf := make([]byte, 1<<20)
	for i := 0; i < count; i++ {
		p := filepath.Join(dir, partName(i))
		f, err := os.Open(p)
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return "", fmt.Errorf("missing part %d of %s: %w", i, digest, err)
		}
		n, err := io.CopyBuffer(io.MultiWriter(out, h), f, buf)
		f.Close()
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return "", err
		}
		written += n
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if size >= 0 && written != size {
		os.Remove(tmp)
		return "", fmt.Errorf("%s: got %d bytes, manifest says %d", digest, written, size)
	}
	algo, want, err := SplitDigest(digest)
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if algo != "sha256" {
		os.Remove(tmp)
		return "", fmt.Errorf("unsupported digest algorithm %q", algo)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("%w: %s (parts produced sha256:%s)", ErrMismatch, digest, got)
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	if !s.KeepParts {
		os.RemoveAll(dir)
	}
	return final, nil
}

// OpenBlob opens a verified blob for reading.
func (s *Store) OpenBlob(digest string) (*os.File, error) { return os.Open(s.BlobPath(digest)) }

// DiskUsage returns the number of bytes used by blobs and parts.
func (s *Store) DiskUsage() (blobs, parts int64) {
	filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		switch {
		case strings.Contains(path, string(os.PathSeparator)+"parts"+string(os.PathSeparator)):
			parts += info.Size()
		case strings.Contains(path, string(os.PathSeparator)+"blobs"+string(os.PathSeparator)):
			blobs += info.Size()
		}
		return nil
	})
	return blobs, parts
}

// Prune deletes verified blobs older than keep-days (0 disables).
func (s *Store) Prune(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	var freed int64
	err := filepath.Walk(filepath.Join(s.Root, "blobs"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				freed += info.Size()
			}
		}
		return nil
	})
	return freed, err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return strings.Repeat("0", 64)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return strings.Repeat("0", 64)
	}
	return hex.EncodeToString(h.Sum(nil))
}
