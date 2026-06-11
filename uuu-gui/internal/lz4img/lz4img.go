// Package lz4img decompresses .wic.lz4 images to raw .wic files.
//
// Yocto's CONVERSION_CMD:lz4 uses `lz4 -9 -z -l`, which produces the LZ4
// *legacy* frame format (magic 0x184C2102): a 4-byte magic followed by a
// sequence of [le32 compressed-size][compressed block] entries where each
// block decompresses to at most 8 MiB. The modern frame format
// (magic 0x184D2204) is also handled, via pierrec/lz4's Reader.
package lz4img

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pierrec/lz4/v4"
)

const (
	magicLegacy   = 0x184C2102
	magicFrame    = 0x184D2204
	legacyBlock   = 8 << 20 // 8 MiB decompressed block size used by the lz4 CLI
	maxLegacyComp = legacyBlock + (legacyBlock / 255) + 64
)

// Progress reports compressed bytes consumed out of the total compressed size.
type Progress func(read, total int64)

// IsLZ4Path reports whether the filename looks like an lz4-compressed image.
func IsLZ4Path(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".lz4")
}

// OutputPath returns the decompressed destination for src (strips the .lz4
// suffix), e.g. foo.rootfs.wic.lz4 -> foo.rootfs.wic in the same directory so
// that an existing foo.rootfs.wic.bmap sidecar lines up with the output.
func OutputPath(src string) string {
	if IsLZ4Path(src) {
		return src[:len(src)-len(".lz4")]
	}
	return src + ".wic"
}

// CachedOutput returns the path of an up-to-date previously decompressed wic,
// or "" if it needs to be (re)created.
func CachedOutput(src string) string {
	dst := OutputPath(src)
	si, err := os.Stat(src)
	if err != nil {
		return ""
	}
	di, err := os.Stat(dst)
	if err != nil {
		return ""
	}
	if di.ModTime().Before(si.ModTime()) || di.Size() == 0 {
		return ""
	}
	return dst
}

// DecompressFile decompresses src (legacy or modern lz4 frame) into
// OutputPath(src), writing to a .part file first and renaming on success.
// Returns the destination path.
func DecompressFile(src string, prog Progress) (string, error) {
	dst := OutputPath(src)

	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	total := int64(-1)
	if fi, err := in.Stat(); err == nil {
		total = fi.Size()
	}

	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer func() {
		out.Close()
		os.Remove(tmp) // no-op after successful rename
	}()

	cr := &countingReader{r: bufio.NewReaderSize(in, 1<<20)}
	report := func() {
		if prog != nil {
			prog(cr.n, total)
		}
	}

	bw := bufio.NewWriterSize(out, 1<<20)
	if err := decompress(cr, bw, report); err != nil {
		return "", fmt.Errorf("decompress %s: %w", src, err)
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	report()
	return dst, nil
}

func decompress(r io.Reader, w io.Writer, report func()) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("reading magic: %w", err)
	}

	switch binary.LittleEndian.Uint32(magic[:]) {
	case magicLegacy:
		return decompressLegacy(r, w, report)
	case magicFrame:
		// Reconstruct the stream with the magic prepended for the frame reader.
		zr := lz4.NewReader(io.MultiReader(strings.NewReader(string(magic[:])), r))
		buf := make([]byte, 4<<20)
		for {
			n, err := zr.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return werr
				}
				report()
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("not an lz4 file (magic %02x%02x%02x%02x)",
			magic[0], magic[1], magic[2], magic[3])
	}
}

func decompressLegacy(r io.Reader, w io.Writer, report func()) error {
	comp := make([]byte, maxLegacyComp)
	raw := make([]byte, legacyBlock)
	var hdr [4]byte

	for {
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading block size: %w", err)
		}
		v := binary.LittleEndian.Uint32(hdr[:])

		// lz4 CLI concatenates frames; a new magic may follow the last block.
		if v == magicLegacy {
			continue
		}
		if v == 0 || v > maxLegacyComp {
			return fmt.Errorf("invalid legacy block size %d", v)
		}

		if _, err := io.ReadFull(r, comp[:v]); err != nil {
			return fmt.Errorf("reading block: %w", err)
		}
		n, err := lz4.UncompressBlock(comp[:v], raw)
		if err != nil {
			return fmt.Errorf("block decode: %w", err)
		}
		if _, err := w.Write(raw[:n]); err != nil {
			return err
		}
		report()
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
