# uuu-gui — Board Flasher

A one-file web GUI around NXP's [uuu (mfgtools)](https://github.com/nxp-imx/mfgtools)
so anyone can provision a board over USB — no terminal required.

Run it, your browser opens, pick a `.wic` image, hit **⚡ Flash Board**, watch
the progress bars. That's it.

## What it does

- Embeds the stock `uuu` binary for your OS/architecture and runs it for you
  (`uuu -v -bmap -b emmc_all <image>` by default).
- Decompresses `.wic.lz4` images automatically (including the LZ4 *legacy*
  frame format Yocto produces) — the decompressed `.wic` is kept next to the
  original and reused on the next flash.
- `.wic.zst`, `.wic.gz`, `.wic.bz2` and plain `.wic` are passed straight to
  uuu, which decompresses them natively while flashing.
- Uses `.bmap` files automatically when they sit next to the image.
- Pick the image with a file chooser or drag & drop; picking a file that is
  already in the image folder (e.g. straight from Downloads) copies nothing.
- Live status: connected-board indicator, per-board step/command/percent,
  full uuu log, success/failure banner.

## Flashing a board (operator instructions)

1. Download the `uuu-gui` build for your machine (Mac / Windows / Linux).
2. Start uuu-gui:
   - **macOS**: first time only — right-click → *Open* (unsigned binary).
   - **Windows**: double-click `uuu-gui.exe`.
   - **Linux**: `sudo ./uuu-gui` (sudo gives USB access; or install udev
     rules, see below).
3. The browser opens at `http://127.0.0.1:8642`.
4. Click **Choose image…** and pick the `.wic.lz4` (or drag & drop it onto
   the page).
5. Connect the board's USB port and power it on in serial-download mode.
   The header chip turns green when the board is detected.
6. Click **⚡ Flash Board**, wait for the green banner.

## CLI usage

```
uuu-gui                  # start the web GUI (opens browser)
uuu-gui IMAGE.wic.lz4    # flash from the terminal, uuu's native console UI
uuu-gui -list            # show connected boards
uuu-gui -script sd_all   # use a different uuu built-in script
uuu-gui -no-bmap         # ignore .bmap sidecars
uuu-gui -uuu /path/uuu   # use an external uuu binary instead of the embedded one
uuu-gui -dirs /imgs,/srv # extra folders to scan for images
```

## USB notes per OS

- **macOS** — the first flash asks for your administrator password (uuu needs
  root to detach the HID kernel driver); later flashes don't ask again.
- **Linux** — uuu needs permission to claim the USB device: run with `sudo`,
  or install udev rules once (`uuu -udev` prints them; the embedded uuu is
  extracted to `~/.cache/uuu-gui/` if you want to run it directly).
- **Windows** — the serial-download (HID) stage works out of the box. If the
  fastboot stage can't be found, install the WinUSB driver for the device
  once with [Zadig](https://zadig.akeo.ie/).

## Building

The Go app embeds a per-platform stock `uuu` from `bins/<os>_<arch>/`.
CI (`.github/workflows/uuu-gui.yaml`) builds all platforms; locally on a Mac:

```sh
./build-uuu-macos.sh      # builds portable uuu -> bins/darwin_<arch>/uuu
go build -o uuu-gui .
go test ./...
```

The macOS uuu is linked against static homebrew libraries (libusb, zstd,
zlib, openssl, vendored tinyxml2), so the result runs on a clean Mac.
Linux uses the repo's `STATIC=1` build; Windows uses the static MSVC solution.
