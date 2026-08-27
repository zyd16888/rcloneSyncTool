package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitSingleFileSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	parent, base, ok := splitSingleFileSource(file)
	if !ok || parent != dir || base != "movie.mkv" {
		t.Fatalf("a regular file must split into parent and name: %q %q %v", parent, base, ok)
	}

	if _, _, ok := splitSingleFileSource(dir); ok {
		t.Fatal("a directory must keep directory semantics")
	}
	if _, _, ok := splitSingleFileSource(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing path must keep directory semantics")
	}
}

func TestTransferSpecRoundTrip(t *testing.T) {
	spec := TransferSpec{
		Operation:          "move",
		SourceSubpath:      "Movie.Name",
		DestinationSubpath: "incoming/run-1",
	}
	encoded, err := EncodeTransferSpec(spec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeStoredTransferSpec(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Operation != "move" || decoded.SourceSubpath != "Movie.Name" {
		t.Fatalf("round trip lost data: %+v", decoded)
	}
	if _, err := DecodeStoredTransferSpec(`{"operation":"sync"}`); err == nil {
		t.Fatal("an unsupported operation must not survive a restart")
	}
}
