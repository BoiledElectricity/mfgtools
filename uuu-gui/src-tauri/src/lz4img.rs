//! Decompression of .wic.lz4 images.
//!
//! Yocto's `lz4 -9 -z -l` produces the LZ4 *legacy* frame format
//! (magic 0x184C2102): 4-byte magic, then [le32 compressed-size][block]
//! entries where each block decompresses to at most 8 MiB. The modern frame
//! format (magic 0x184D2204) is handled by lz4_flex's FrameDecoder.

use std::fs::File;
use std::io::{BufReader, BufWriter, Read, Write};
use std::path::{Path, PathBuf};

const MAGIC_LEGACY: u32 = 0x184C2102;
const MAGIC_FRAME: u32 = 0x184D2204;
const LEGACY_BLOCK: usize = 8 << 20;
const MAX_LEGACY_COMP: usize = LEGACY_BLOCK + (LEGACY_BLOCK / 255) + 64;

pub fn is_lz4_path(path: &Path) -> bool {
    path.extension()
        .map(|e| e.eq_ignore_ascii_case("lz4"))
        .unwrap_or(false)
}

/// foo.rootfs.wic.lz4 -> foo.rootfs.wic (same directory, so a .bmap sidecar
/// next to the source lines up with the output).
pub fn output_path(src: &Path) -> PathBuf {
    if is_lz4_path(src) {
        src.with_extension("")
    } else {
        let mut p = src.as_os_str().to_owned();
        p.push(".wic");
        PathBuf::from(p)
    }
}

/// Returns a previously decompressed, still-fresh output, if any.
pub fn cached_output(src: &Path) -> Option<PathBuf> {
    let dst = output_path(src);
    let (sm, dm) = (src.metadata().ok()?, dst.metadata().ok()?);
    if dm.len() == 0 || dm.modified().ok()? < sm.modified().ok()? {
        return None;
    }
    Some(dst)
}

/// Decompresses src into output_path(src) via a .part temp file.
/// `progress(read, total)` reports compressed bytes consumed.
pub fn decompress_file(
    src: &Path,
    mut progress: impl FnMut(u64, u64),
    cancelled: impl Fn() -> bool,
) -> Result<PathBuf, String> {
    let dst = output_path(src);
    let total = src.metadata().map(|m| m.len()).unwrap_or(0);

    let f = File::open(src).map_err(|e| format!("open {}: {e}", src.display()))?;
    let mut r = CountingReader {
        inner: BufReader::with_capacity(1 << 20, f),
        n: 0,
    };

    let tmp = dst.with_extension("part");
    let out = File::create(&tmp).map_err(|e| format!("create {}: {e}", tmp.display()))?;
    let mut w = BufWriter::with_capacity(1 << 20, out);

    let res = (|| -> Result<(), String> {
        let mut magic = [0u8; 4];
        r.read_exact(&mut magic).map_err(|e| format!("reading magic: {e}"))?;
        match u32::from_le_bytes(magic) {
            MAGIC_LEGACY => decompress_legacy(&mut r, &mut w, &mut |n| progress(n, total), &cancelled),
            MAGIC_FRAME => {
                let chained = ChainedReader { head: Some(magic), inner: &mut r };
                let mut dec = lz4_flex::frame::FrameDecoder::new(chained);
                let mut buf = vec![0u8; 4 << 20];
                loop {
                    if cancelled() {
                        return Err("cancelled".into());
                    }
                    let n = dec.read(&mut buf).map_err(|e| format!("frame decode: {e}"))?;
                    if n == 0 {
                        return Ok(());
                    }
                    w.write_all(&buf[..n]).map_err(|e| e.to_string())?;
                    let read = dec.get_ref().inner.n;
                    progress(read, total);
                }
            }
            _ => Err(format!(
                "not an lz4 file (magic {:02x}{:02x}{:02x}{:02x})",
                magic[0], magic[1], magic[2], magic[3]
            )),
        }
    })();

    if let Err(e) = res {
        let _ = std::fs::remove_file(&tmp);
        return Err(e);
    }
    w.flush().map_err(|e| e.to_string())?;
    drop(w);
    std::fs::rename(&tmp, &dst).map_err(|e| e.to_string())?;
    progress(total, total);
    Ok(dst)
}

fn decompress_legacy(
    r: &mut CountingReader<impl Read>,
    w: &mut impl Write,
    progress: &mut impl FnMut(u64),
    cancelled: &impl Fn() -> bool,
) -> Result<(), String> {
    let mut comp = vec![0u8; MAX_LEGACY_COMP];
    let mut raw = vec![0u8; LEGACY_BLOCK];
    loop {
        if cancelled() {
            return Err("cancelled".into());
        }
        let mut hdr = [0u8; 4];
        match read_exact_or_eof(r, &mut hdr) {
            Ok(false) => return Ok(()), // clean EOF
            Ok(true) => {}
            Err(e) => return Err(format!("reading block size: {e}")),
        }
        let v = u32::from_le_bytes(hdr) as usize;
        // lz4 CLI may concatenate frames; a new magic can follow the last block.
        if v as u32 == MAGIC_LEGACY {
            continue;
        }
        if v == 0 || v > MAX_LEGACY_COMP {
            return Err(format!("invalid legacy block size {v}"));
        }
        r.read_exact(&mut comp[..v]).map_err(|e| format!("reading block: {e}"))?;
        let n = lz4_flex::block::decompress_into(&comp[..v], &mut raw)
            .map_err(|e| format!("block decode: {e}"))?;
        w.write_all(&raw[..n]).map_err(|e| e.to_string())?;
        progress(r.n);
    }
}

fn read_exact_or_eof(r: &mut impl Read, buf: &mut [u8]) -> std::io::Result<bool> {
    let mut got = 0;
    while got < buf.len() {
        let n = r.read(&mut buf[got..])?;
        if n == 0 {
            if got == 0 {
                return Ok(false);
            }
            return Err(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "truncated block header",
            ));
        }
        got += n;
    }
    Ok(true)
}

pub struct CountingReader<R> {
    inner: R,
    pub n: u64,
}

impl<R: Read> Read for CountingReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = self.inner.read(buf)?;
        self.n += n as u64;
        Ok(n)
    }
}

/// Replays the 4 magic bytes already consumed, then continues with the rest.
struct ChainedReader<'a, R> {
    head: Option<[u8; 4]>,
    inner: &'a mut R,
}

impl<R: Read> Read for ChainedReader<'_, R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if let Some(h) = self.head.take() {
            buf[..4].copy_from_slice(&h);
            return Ok(4);
        }
        self.inner.read(buf)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn legacy_round_trip() {
        // Build a legacy stream by hand: magic + two compressed blocks.
        let block1: Vec<u8> = b"yocto-wic-block".repeat(700000); // >8MiB source
        let (a, b) = block1.split_at(LEGACY_BLOCK);
        let ca = lz4_flex::block::compress(a);
        let cb = lz4_flex::block::compress(b);
        let mut stream = MAGIC_LEGACY.to_le_bytes().to_vec();
        stream.extend((ca.len() as u32).to_le_bytes());
        stream.extend(&ca);
        stream.extend((cb.len() as u32).to_le_bytes());
        stream.extend(&cb);

        let dir = std::env::temp_dir().join("uuu-gui-test-legacy");
        let _ = std::fs::create_dir_all(&dir);
        let src = dir.join("img.wic.lz4");
        std::fs::write(&src, &stream).unwrap();
        let out = decompress_file(&src, |_, _| {}, || false).unwrap();
        assert_eq!(out, dir.join("img.wic"));
        assert_eq!(std::fs::read(&out).unwrap(), block1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn output_path_strips_lz4() {
        assert_eq!(
            output_path(Path::new("/a/b.rootfs.wic.lz4")),
            PathBuf::from("/a/b.rootfs.wic")
        );
    }
}
