// uuu-gui — a tiny web GUI around NXP's uuu (mfgtools) so anyone can flash a
// board: run it, the browser opens, pick a .wic/.wic.lz4 image, hit Flash.
//
// The stock uuu binary for the host OS/architecture is embedded and extracted
// at runtime; .wic.lz4 images (LZ4 legacy frame, as produced by Yocto) are
// decompressed automatically; .bmap sidecars are used when present; the flash
// runs uuu's built-in emmc_all script by default.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/popoto/uuu-gui/internal/lz4img"
	"github.com/popoto/uuu-gui/internal/server"
	"github.com/popoto/uuu-gui/internal/uuurun"
)

//go:embed web
var webFS embed.FS

func main() {
	var (
		port      = flag.Int("port", 8642, "port for the web GUI (binds 127.0.0.1)")
		noBrowser = flag.Bool("no-browser", false, "do not open the browser automatically")
		noBmap    = flag.Bool("no-bmap", false, "do not use .bmap files even when present")
		script    = flag.String("script", "emmc_all", "uuu built-in script to run")
		uuuPath   = flag.String("uuu", "", "use this uuu binary instead of the embedded one")
		dirs      = flag.String("dirs", "", "extra image folders to scan (comma separated)")
		list      = flag.Bool("list", false, "list connected boards and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `uuu-gui — web GUI for flashing boards with uuu

Usage:
  uuu-gui                 start the web GUI (opens your browser)
  uuu-gui IMAGE           flash IMAGE (.wic / .wic.lz4 / .wic.zst ...) from the terminal
  uuu-gui -list           show connected boards

Options:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	exe := *uuuPath
	if exe == "" {
		var err error
		exe, err = uuurun.Extract(uuuBin, uuuExeName)
		if err != nil {
			fatal("setting up uuu (%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
		}
	}

	if *list {
		devs, err := uuurun.ListDevices(exe)
		if err != nil {
			fatal("%v", err)
		}
		if len(devs) == 0 {
			fmt.Println("No boards detected. Connect USB and power on in serial-download mode.")
			return
		}
		for _, d := range devs {
			fmt.Printf("%-8s %-8s %-6s %s:%s %s\n", d.Path, d.Chip, d.Pro, d.Vid, d.Pid, d.Serial)
		}
		return
	}

	if img := flag.Arg(0); img != "" {
		cliFlash(exe, img, *script, !*noBmap)
		return
	}

	scanDirs := defaultScanDirs()
	for _, d := range strings.Split(*dirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			scanDirs = append(scanDirs, d)
		}
	}

	sub, _ := fs.Sub(webFS, "web")
	srv := server.New(server.Options{
		UuuPath:  exe,
		ScanDirs: scanDirs,
		Bmap:     !*noBmap,
		Script:   *script,
		WebFS:    sub,
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("cannot listen on %s: %v", addr, err)
	}
	url := "http://" + addr
	fmt.Printf("Board Flasher running at %s  (Ctrl-C to quit)\n", url)
	if !*noBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(url)
		}()
	}
	if err := http.Serve(ln, srv.Handler()); err != nil {
		fatal("%v", err)
	}
}

// cliFlash flashes from the terminal: decompress with a simple progress
// readout, then hand the terminal to uuu for its native console UI.
func cliFlash(exe, image, script string, bmap bool) {
	if _, err := os.Stat(image); err != nil {
		fatal("image not found: %s", image)
	}
	wic := image
	if lz4img.IsLZ4Path(image) {
		if cached := lz4img.CachedOutput(image); cached != "" {
			fmt.Printf("Using cached %s\n", cached)
			wic = cached
		} else {
			fmt.Printf("Decompressing %s\n", filepath.Base(image))
			last := -1
			out, err := lz4img.DecompressFile(image, func(read, total int64) {
				if total <= 0 {
					return
				}
				pct := int(read * 100 / total)
				if pct != last {
					last = pct
					fmt.Printf("\r  %3d%%", pct)
				}
			})
			fmt.Println()
			if err != nil {
				fatal("%v", err)
			}
			wic = out
		}
	}

	args := []string{}
	if bmap {
		args = append(args, "-bmap")
	}
	args = append(args, "-b", script, wic)
	fmt.Printf("Running: uuu %s\n", strings.Join(args, " "))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("uuu failed: %v", err)
	}
}

func defaultScanDirs() []string {
	var dirs []string
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Downloads"))
	}
	return dirs
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "uuu-gui: "+format+"\n", a...)
	os.Exit(1)
}
