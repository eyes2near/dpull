package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func timePast(days int) time.Time { return time.Now().AddDate(0, 0, -days) }

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func writeChunk(t *testing.T, dir string, idx int, data []byte) {
	t.Helper()
	p := PartPath(dir, idx)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(data)
	if err := CommitPart(dir, idx, int64(len(data)), hex.EncodeToString(s[:])); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleAndVerify(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full := make([]byte, 1000)
	rand.Read(full)
	dg := digestOf(full)
	meta := Meta{Digest: dg, Size: int64(len(full)), ChunkSize: 400, Count: 3}

	dir, done, err := s.PreparePartsDir(dg, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Fatalf("fresh download reports %d completed chunks", len(done))
	}
	for i := 0; i < meta.Count; i++ {
		start := int64(i) * meta.ChunkSize
		end := start + meta.ChunkSize
		if end > int64(len(full)) {
			end = int64(len(full))
		}
		writeChunk(t, dir, i, full[start:end])
	}
	path, err := s.Assemble(dg, meta.Count, meta.Size)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatal("assembled content differs")
	}
	if !s.HasBlob(dg, int64(len(full)), true) {
		t.Error("HasBlob should be true after assembly")
	}
	// parts are cleaned up by default
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("parts should be removed after assembly")
	}
}

func TestResumeOnlyCountsVerifiedChunks(t *testing.T) {
	s, _ := New(t.TempDir())
	full := make([]byte, 900)
	rand.Read(full)
	dg := digestOf(full)
	meta := Meta{Digest: dg, Size: 900, ChunkSize: 300, Count: 3}
	dir, _, _ := s.PreparePartsDir(dg, meta)
	writeChunk(t, dir, 0, full[0:300])
	writeChunk(t, dir, 2, full[600:900])
	// a truncated part with a stale marker must be ignored
	if err := os.WriteFile(PartPath(dir, 1), full[300:400], 0o644); err != nil {
		t.Fatal(err)
	}

	dir2, done, err := s.PreparePartsDir(dg, meta)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 != dir {
		t.Fatal("parts dir should be stable")
	}
	if !done[0] || !done[2] {
		t.Errorf("completed chunks lost: %v", done)
	}
	if done[1] {
		t.Error("truncated chunk was accepted")
	}
}

func TestPlanChangeInvalidatesParts(t *testing.T) {
	s, _ := New(t.TempDir())
	full := make([]byte, 600)
	rand.Read(full)
	dg := digestOf(full)
	old := Meta{Digest: dg, Size: 600, ChunkSize: 300, Count: 2}
	dir, _, _ := s.PreparePartsDir(dg, old)
	writeChunk(t, dir, 0, full[0:300])

	fresh := Meta{Digest: dg, Size: 600, ChunkSize: 200, Count: 3}
	_, done, err := s.PreparePartsDir(dg, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Error("changing --chunk must invalidate existing parts")
	}
}

func TestAssembleRejectsCorruptData(t *testing.T) {
	s, _ := New(t.TempDir())
	full := make([]byte, 600)
	rand.Read(full)
	dg := digestOf(full)
	meta := Meta{Digest: dg, Size: 600, ChunkSize: 300, Count: 2}
	dir, _, _ := s.PreparePartsDir(dg, meta)
	writeChunk(t, dir, 0, full[0:300])
	writeChunk(t, dir, 1, bytes.Repeat([]byte("x"), 300))

	if _, err := s.Assemble(dg, meta.Count, meta.Size); err == nil {
		t.Fatal("corrupted parts must not assemble")
	}
	if s.HasBlob(dg, 600, false) {
		t.Error("no blob file should exist after a failed assembly")
	}
}

func TestAssembleRejectsShortData(t *testing.T) {
	s, _ := New(t.TempDir())
	part := make([]byte, 100)
	rand.Read(part)
	dg := digestOf(part)
	meta := Meta{Digest: dg, Size: 500, ChunkSize: 500, Count: 1}
	dir, _, _ := s.PreparePartsDir(dg, meta)
	writeChunk(t, dir, 0, part)
	if _, err := s.Assemble(dg, 1, 500); err == nil {
		t.Fatal("short data must be rejected")
	}
}

func TestPurgeParts(t *testing.T) {
	s, _ := New(t.TempDir())
	dg := digestOf([]byte("hello"))
	meta := Meta{Digest: dg, Size: 10, ChunkSize: 10, Count: 1}
	dir, _, _ := s.PreparePartsDir(dg, meta)
	writeChunk(t, dir, 0, []byte("0123456789"))
	dir2, err := s.PurgeParts(dg)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 != dir {
		t.Fatalf("PurgeParts moved the dir: %q vs %q", dir2, dir)
	}
	_, done, _ := s.PreparePartsDir(dg, meta)
	if len(done) != 0 {
		t.Error("purge left chunk state behind")
	}
}

func TestDropBlobAndPurgeForForce(t *testing.T) {
	s, _ := New(t.TempDir())
	full := make([]byte, 600)
	rand.Read(full)
	dg := digestOf(full)
	meta := Meta{Digest: dg, Size: 600, ChunkSize: 600, Count: 1}
	dir, _, _ := s.PreparePartsDir(dg, meta)
	writeChunk(t, dir, 0, full)
	if _, err := s.Assemble(dg, 1, 600); err != nil {
		t.Fatal(err)
	}
	if !s.HasBlob(dg, 600, false) {
		t.Fatal("blob should exist")
	}
	if _, err := s.PurgeParts(dg); err != nil {
		t.Fatal(err)
	}
	if err := s.DropBlob(dg); err != nil {
		t.Fatal(err)
	}
	if s.HasBlob(dg, 600, false) {
		t.Error("--force should have removed the blob")
	}
	if err := s.DropBlob(dg); err != nil {
		t.Errorf("dropping a missing blob must be silent: %v", err)
	}
}

func TestPruneRemovesOldBlobsOnly(t *testing.T) {
	s, _ := New(t.TempDir())
	dg := digestOf([]byte("old"))
	path := s.BlobPath(dg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, timePast(30), timePast(30)); err != nil {
		t.Fatal(err)
	}
	freed, err := s.Prune(7)
	if err != nil {
		t.Fatal(err)
	}
	if freed != 3 {
		t.Errorf("freed = %d", freed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("old blob not removed")
	}
	if n, err := s.Prune(0); n != 0 || err != nil {
		t.Error("Prune(0) must be a no-op")
	}
}
