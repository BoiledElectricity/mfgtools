# Board Flasher (uuu-gui)

A native desktop app (Tauri) around NXP's [uuu (mfgtools)](https://github.com/nxp-imx/mfgtools)
so anyone can provision a board over USB — no terminal, no browser.

Install it, open it, pick a `.wic` image, hit **⚡ Flash Board**, watch the
progress bars.

## What it does

- Bundles the stock `uuu` binary for each OS/architecture and runs it for you
  (`uuu -v -bmap -b emmc_all <image>`).
- Decompresses `.wic.lz4` images automatically (including the LZ4 *legacy*
  frame format Yocto produces) — the decompressed `.wic` is kept next to the
  original and reused on the next flash.
- `.wic.zst`, `.wic.gz`, `.wic.bz2` and plain `.wic` are passed straight to
  uuu, which decompresses them natively while flashing.
- Uses `.bmap` files automatically when they sit next to the image.
- Native file dialog and drag & drop — real file paths, nothing gets copied.
- Live status: connected-board indicator, per-board step/command/percent,
  full uuu log, success/failure banner.

## Installing

Grab the artifact for your machine from the `uuu-gui` GitHub Actions
workflow (or a release):

- **Windows**: run `Board Flasher_<ver>_x64-setup.exe` — installs like any
  app, Start-menu entry included.
- **macOS**: open the `.dmg`, drag Board Flasher to Applications. First
  launch: right-click → *Open* (unsigned). The first flash asks for your
  administrator password (uuu needs root to detach the HID kernel driver);
  later flashes don't ask again.
- **Linux**: install the `.deb` (`sudo apt install ./BoardFlasher*.deb`) or
  run the AppImage. USB access needs root or uuu udev rules (`uuu -udev`
  prints them).

## Flashing a board

1. Open Board Flasher.
2. **Choose image…** (or drag & drop the `.wic.lz4` onto the window).
3. Connect the board's USB port and power it on in serial-download mode —
   the header chip turns green when the board is detected.
4. **⚡ Flash Board** → wait for the green banner.

## Development

```
uuu-gui/
  web/        # plain HTML/CSS/JS frontend (no build step)
  src-tauri/  # Rust app: sidecar runner, uuu output parser, lz4, state events
```

The uuu sidecar must exist as `src-tauri/binaries/uuu-<rust-triple>` before
building. On a Mac:

```sh
./build-uuu-macos.sh                 # portable static-deps uuu sidecar
cd src-tauri
cargo test
cargo tauri build --target aarch64-apple-darwin   # .app + .dmg
```

`UUU_GUI_UUU=/path/to/uuu cargo tauri dev` runs against an external uuu
binary during development.

CI (`.github/workflows/uuu-gui.yaml`) builds the Windows NSIS installer,
macOS dmgs (arm64 + x86_64) and Linux deb/AppImage (x64 + arm64), each with
the matching statically-linked uuu sidecar built from this repo.
