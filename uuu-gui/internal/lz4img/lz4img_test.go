package lz4img

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testInput builds data that resembles a disk image: compressible runs,
// zero regions and some incompressible noise, > one 8MiB legacy block.
func testInput(t *testing.T) []byte {
	t.Helper()
	rnd := rand.New(rand.NewSource(1))
	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte("yocto-wic-block"), 1<<16)) // ~1MB compressible
	buf.Write(make([]byte, 6<<20))                            // 6MB zeros
	noise := make([]byte, 4<<20)                              // 4MB noise crosses block boundary
	rnd.Read(noise)
	buf.Write(noise)
	buf.Write(bytes.Repeat([]byte{0xAB}, 9<<20)) // 9MB run
	return buf.Bytes()
}

func roundTrip(t *testing.T, lz4Args []string) {
	t.Helper()
	if _, err := exec.LookPath("lz4"); err != nil {
		t.Skip("lz4 CLI not installed")
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "img.wic")
	comp := filepath.Join(dir, "img.wic.lz4")
	want := testInput(t)
	if err := os.WriteFile(raw, want, 0o644); err != nil {
		t.Fatal(err)
	}
	args := append(lz4Args, raw, comp)
	if out, err := exec.Command("lz4", args...).CombinedOutput(); err != nil {
		t.Fatalf("lz4 %v: %v (%s)", args, err, out)
	}
	if err := os.Remove(raw); err != nil {
		t.Fatal(err)
	}

	var calls int
	got, err := DecompressFile(comp, func(read, total int64) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("output path = %q, want %q", got, raw)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("decompressed data mismatch: got %d bytes, want %d", len(data), len(want))
	}
	if calls == 0 {
		t.Error("progress callback never called")
	}
}

func TestLegacyFormat(t *testing.T) { roundTrip(t, []string{"-q", "-z", "-l"}) } // yocto style
func TestFrameFormat(t *testing.T)  { roundTrip(t, []string{"-q", "-z"}) }

func TestOutputPath(t *testing.T) {
	if got := OutputPath("/a/b.rootfs.wic.lz4"); got != "/a/b.rootfs.wic" {
		t.Fatalf("got %q", got)
	}
}

func TestNotLZ4(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "x.wic.lz4")
	os.WriteFile(bad, []byte("this is not lz4 data"), 0o644)
	if _, err := DecompressFile(bad, nil); err == nil {
		t.Fatal("expected error for non-lz4 input")
	}
}
