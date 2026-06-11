// Package server hosts the local web GUI: static UI, JSON API and an SSE
// event stream with live flash progress.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/popoto/uuu-gui/internal/lz4img"
	"github.com/popoto/uuu-gui/internal/uuurun"
)

// DevState is the live status of one USB device being flashed.
type DevState struct {
	Step    int    `json:"step"`
	Cmd     string `json:"cmd"`
	Percent int    `json:"percent"`
	Done    bool   `json:"done"`
	Failed  bool   `json:"failed"`
	Err     string `json:"err"`
}

// State is the full GUI state, pushed to clients over SSE on every change.
type State struct {
	Phase      string               `json:"phase"` // idle decompressing waiting flashing success error cancelled
	Image      string               `json:"image"`
	Wic        string               `json:"wic"`
	DecompPct  int                  `json:"decompPct"`
	Devices    map[string]*DevState `json:"devices"`
	UsbDevices []uuurun.Device      `json:"usbDevices"`
	Log        []string             `json:"log"`
	Error      string               `json:"error"`
	StartedAt  int64                `json:"startedAt"`
	FinishedAt int64                `json:"finishedAt"`
	UuuVersion string               `json:"uuuVersion"`
	Script     string               `json:"script"`
}

// Options configures the server.
type Options struct {
	UuuPath  string
	ScanDirs []string
	Bmap     bool
	Script   string // built-in uuu script, default emmc_all
	WebFS    fs.FS
}

type Server struct {
	opt    Options
	mu     sync.Mutex
	st     State
	subs   map[chan []byte]struct{}
	cancel context.CancelFunc
}

func New(opt Options) *Server {
	if opt.Script == "" {
		opt.Script = "emmc_all"
	}
	s := &Server{
		opt:  opt,
		subs: map[chan []byte]struct{}{},
		st: State{
			Phase:      "idle",
			Devices:    map[string]*DevState{},
			UuuVersion: uuurun.Version(opt.UuuPath),
			Script:     opt.Script,
		},
	}
	go s.pollDevices()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(s.opt.WebFS))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/images", s.handleImages)
	mux.HandleFunc("POST /api/flash", s.handleFlash)
	mux.HandleFunc("POST /api/cancel", s.handleCancel)
	return mux
}

// ---- state plumbing ----

func (s *Server) snapshot() []byte {
	b, _ := json.Marshal(&s.st)
	return b
}

// update mutates state under lock and broadcasts the new snapshot.
func (s *Server) update(f func(*State)) {
	s.mu.Lock()
	f(&s.st)
	b := s.snapshot()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // slow client: drop, it will catch up on next event
		}
	}
	s.mu.Unlock()
}

func (s *Server) logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	s.update(func(st *State) {
		st.Log = append(st.Log, line)
		if len(st.Log) > 1000 {
			st.Log = st.Log[len(st.Log)-1000:]
		}
	})
}

func (s *Server) phase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Phase
}

// pollDevices refreshes the connected-device list while no flash is running.
func (s *Server) pollDevices() {
	for {
		switch s.phase() {
		case "idle", "success", "error", "cancelled":
			devs, err := uuurun.ListDevices(s.opt.UuuPath)
			if err == nil {
				s.mu.Lock()
				changed := fmt.Sprint(devs) != fmt.Sprint(s.st.UsbDevices)
				s.mu.Unlock()
				if changed {
					s.update(func(st *State) { st.UsbDevices = devs })
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
}

// ---- handlers ----

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	b := s.snapshot()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := make(chan []byte, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	b := s.snapshot()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	fmt.Fprintf(w, "data: %s\n\n", b)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// Image is one flashable file found in the scan directories.
type Image struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Kind    string `json:"kind"` // wic, lz4, zst, gz, bz2
	HasBmap bool   `json:"hasBmap"`
	Cached  bool   `json:"cached"` // lz4 already decompressed and fresh
}

var imageExts = map[string]string{
	".wic": "wic", ".lz4": "lz4", ".zst": "zst", ".gz": "gz", ".bz2": "bz2",
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	dirs := s.opt.ScanDirs
	if d := r.URL.Query().Get("dir"); d != "" {
		dirs = append([]string{d}, dirs...)
	}
	images := scanImages(dirs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

func scanImages(dirs []string) []Image {
	seen := map[string]bool{}
	var out []Image
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			low := strings.ToLower(name)
			kind, ok := imageExts[filepath.Ext(low)]
			if !ok || !strings.Contains(low, ".wic") {
				continue
			}
			path := filepath.Join(dir, name)
			if seen[path] {
				continue
			}
			seen[path] = true
			fi, err := e.Info()
			if err != nil {
				continue
			}
			img := Image{
				Path: path, Name: name, Size: fi.Size(),
				ModTime: fi.ModTime().UnixMilli(), Kind: kind,
			}
			wic := path
			if kind == "lz4" {
				wic = lz4img.OutputPath(path)
				img.Cached = lz4img.CachedOutput(path) != ""
			}
			if _, err := os.Stat(wic + ".bmap"); err == nil {
				img.HasBmap = true
			}
			out = append(out, img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		http.Error(w, "image not found: "+req.Path, http.StatusNotFound)
		return
	}

	s.mu.Lock()
	busy := s.st.Phase == "decompressing" || s.st.Phase == "waiting" || s.st.Phase == "flashing"
	if busy {
		s.mu.Unlock()
		http.Error(w, "a flash is already running", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.st = State{
		Phase: "decompressing", Image: req.Path,
		Devices:    map[string]*DevState{},
		UuuVersion: s.st.UuuVersion,
		UsbDevices: s.st.UsbDevices,
		Script:     s.opt.Script,
		StartedAt:  time.Now().UnixMilli(),
	}
	s.mu.Unlock()
	s.update(func(*State) {})

	go s.runFlash(ctx, req.Path)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCancel(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---- the flash job ----

func (s *Server) runFlash(ctx context.Context, image string) {
	fail := func(err error) {
		phase := "error"
		if ctx.Err() != nil {
			phase = "cancelled"
		}
		s.update(func(st *State) {
			st.Phase = phase
			st.Error = err.Error()
			st.FinishedAt = time.Now().UnixMilli()
		})
	}

	wic := image
	if lz4img.IsLZ4Path(image) {
		if cached := lz4img.CachedOutput(image); cached != "" {
			s.logf("Using cached decompressed image %s", filepath.Base(cached))
			wic = cached
		} else {
			s.logf("Decompressing %s ...", filepath.Base(image))
			last := -1
			out, err := lz4img.DecompressFile(image, func(read, total int64) {
				if ctx.Err() != nil || total <= 0 {
					return
				}
				pct := int(read * 100 / total)
				if pct != last {
					last = pct
					s.update(func(st *State) { st.DecompPct = pct })
				}
			})
			if ctx.Err() != nil {
				fail(fmt.Errorf("cancelled"))
				return
			}
			if err != nil {
				fail(err)
				return
			}
			wic = out
			s.logf("Decompressed to %s", filepath.Base(out))
		}
	}

	s.update(func(st *State) {
		st.Wic = wic
		st.DecompPct = 100
		st.Phase = "waiting"
	})

	args := []string{"-v"}
	if s.opt.Bmap {
		args = append(args, "-bmap")
	}
	args = append(args, "-b", s.opt.Script, wic)
	s.logf("Running: uuu %s", strings.Join(args, " "))

	err := uuurun.Run(ctx, s.opt.UuuPath, args, func(ev uuurun.Event) {
		switch ev.Type {
		case "attach":
			s.update(func(st *State) {
				st.Phase = "flashing"
				if _, ok := st.Devices[ev.Dev]; !ok {
					st.Devices[ev.Dev] = &DevState{}
				}
			})
			s.logf("%s", ev.Text)
		case "cmdstart":
			s.update(func(st *State) {
				d := dev(st, ev.Dev)
				d.Step++
				d.Cmd = ev.Text
				d.Percent = 0
			})
			s.logf("%s> %s", ev.Dev, ev.Text)
		case "cmdok":
			s.update(func(st *State) { dev(st, ev.Dev).Percent = 100 })
		case "cmdfail":
			s.update(func(st *State) {
				d := dev(st, ev.Dev)
				d.Failed = true
				d.Err = ev.Text
			})
			s.logf("%s> FAIL %s", ev.Dev, ev.Text)
		case "progress":
			if ev.Dev != "" {
				s.update(func(st *State) { dev(st, ev.Dev).Percent = ev.Percent })
			}
		case "wait", "info", "line":
			if ev.Text != "" && ev.Type != "wait" {
				s.logf("%s", ev.Text)
			}
		}
	})

	if ctx.Err() != nil {
		fail(fmt.Errorf("cancelled"))
		return
	}
	if err != nil {
		fail(fmt.Errorf("uuu failed: %w", err))
		return
	}
	s.update(func(st *State) {
		st.Phase = "success"
		st.FinishedAt = time.Now().UnixMilli()
		for _, d := range st.Devices {
			d.Done = true
		}
	})
	s.logf("Flash complete")
}

func dev(st *State, path string) *DevState {
	if path == "" {
		path = "?"
	}
	d, ok := st.Devices[path]
	if !ok {
		d = &DevState{}
		st.Devices[path] = d
	}
	return d
}
