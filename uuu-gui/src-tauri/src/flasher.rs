//! Flash job state machine. Holds the State pushed to the webview and runs
//! the decompress -> uuu pipeline on a worker thread.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

use serde::Serialize;
use tauri::{Emitter, Manager};

use crate::{lz4img, uuurun};

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DevState {
    pub step: u32,
    pub cmd: String,
    pub percent: u8,
    pub done: bool,
    pub failed: bool,
    pub err: String,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct State {
    pub phase: String, // idle decompressing waiting flashing success error cancelled
    pub image: String,
    pub wic: String,
    pub decomp_pct: u8,
    pub decomp_read: u64,
    pub decomp_total: u64,
    pub devices: BTreeMap<String, DevState>,
    pub usb_devices: Vec<uuurun::Device>,
    pub log: Vec<String>,
    pub error: String,
    pub started_at: u64,
    pub finished_at: u64,
    pub uuu_version: String,
    pub script: String,
    pub needs_password: bool,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PickedFile {
    pub path: String,
    pub name: String,
    pub size: u64,
}

fn picked(path: &Path) -> Option<PickedFile> {
    Some(PickedFile {
        path: path.to_string_lossy().to_string(),
        name: path.file_name()?.to_string_lossy().to_string(),
        size: path.metadata().ok()?.len(),
    })
}

fn is_bmap_name(low: &str) -> bool {
    low.ends_with(".bmap")
}

fn is_image_name(low: &str) -> bool {
    let ok_ext = [".wic", ".lz4", ".zst", ".gz", ".bz2"]
        .iter()
        .any(|e| low.ends_with(e));
    ok_ext && low.contains(".wic")
}

pub fn picked_image(path: &Path) -> Option<PickedFile> {
    let f = picked(path)?;
    if !is_image_name(&f.name.to_lowercase()) {
        return None;
    }
    Some(f)
}

pub fn picked_bmap(path: &Path) -> Option<PickedFile> {
    let f = picked(path)?;
    if !is_bmap_name(&f.name.to_lowercase()) {
        return None;
    }
    Some(f)
}

pub fn picked_bootloader(path: &Path) -> Option<PickedFile> {
    // Bootloaders come with any or no extension (imx-boot-pmm-emmc.bin-flash_evk,
    // flash.bin); accept any existing file that isn't an image or bmap.
    let f = picked(path)?;
    let low = f.name.to_lowercase();
    if is_image_name(&low) || is_bmap_name(&low) {
        return None;
    }
    Some(f)
}

/// A file dropped onto the window, sorted into the right slot.
#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Dropped {
    pub kind: String, // "image", "bmap" or "bootloader"
    pub file: PickedFile,
}

pub fn classify_dropped(path: &Path) -> Result<Dropped, String> {
    let f = picked(path).ok_or("cannot read dropped file")?;
    let low = f.name.to_lowercase();
    let kind = if is_bmap_name(&low) {
        "bmap"
    } else if is_image_name(&low) {
        "image"
    } else {
        "bootloader"
    };
    Ok(Dropped { kind: kind.into(), file: f })
}

pub struct AppState {
    pub uuu: PathBuf,
    st: Mutex<State>,
    cancelled: AtomicBool,
    child_id: AtomicU32,
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

impl AppState {
    pub fn new(uuu: PathBuf, version: String) -> Self {
        let mut st = State::default();
        st.phase = "idle".into();
        st.uuu_version = version;
        st.script = "emmc_all".into();
        st.needs_password = !uuurun::is_privileged(&uuu);
        AppState {
            uuu,
            st: Mutex::new(st),
            cancelled: AtomicBool::new(false),
            child_id: AtomicU32::new(0),
        }
    }

    pub fn snapshot(&self) -> State {
        self.st.lock().unwrap().clone()
    }

    pub fn cancel(&self) {
        self.cancelled.store(true, Ordering::SeqCst);
        let pid = self.child_id.load(Ordering::SeqCst);
        if pid != 0 {
            uuurun::kill(pid);
        }
    }

    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::SeqCst)
    }
}

/// Mutates state and pushes the new snapshot to the webview.
fn update(app: &tauri::AppHandle, f: impl FnOnce(&mut State)) {
    let s = app.state::<AppState>();
    let snap = {
        let mut st = s.st.lock().unwrap();
        f(&mut st);
        st.clone()
    };
    let _ = app.emit("state", &snap);
}

fn logline(app: &tauri::AppHandle, line: String) {
    update(app, |st| {
        st.log.push(line);
        if st.log.len() > 1000 {
            let cut = st.log.len() - 1000;
            st.log.drain(..cut);
        }
    });
}

pub fn start_flash(
    app: tauri::AppHandle,
    bootloader: String,
    image: String,
    bmap: String,
) -> Result<(), String> {
    if !Path::new(&bootloader).exists() {
        return Err(format!("bootloader not found: {bootloader}"));
    }
    if !Path::new(&image).exists() {
        return Err(format!("image not found: {image}"));
    }
    if !Path::new(&bmap).exists() {
        return Err(format!("bmap not found: {bmap}"));
    }
    {
        let s = app.state::<AppState>();
        let mut st = s.st.lock().unwrap();
        if matches!(st.phase.as_str(), "decompressing" | "waiting" | "flashing") {
            return Err("a flash is already running".into());
        }
        let keep_usb = std::mem::take(&mut st.usb_devices);
        let keep_ver = std::mem::take(&mut st.uuu_version);
        let keep_script = std::mem::take(&mut st.script);
        *st = State::default();
        st.phase = "decompressing".into();
        st.image = image.clone();
        st.usb_devices = keep_usb;
        st.uuu_version = keep_ver;
        st.script = keep_script;
        st.started_at = now_ms();
        st.needs_password = !uuurun::is_privileged(&s.uuu);
        s.cancelled.store(false, Ordering::SeqCst);
        s.child_id.store(0, Ordering::SeqCst);
        let snap = st.clone();
        drop(st);
        let _ = app.emit("state", &snap);
    }

    std::thread::spawn(move || run_flash(app, bootloader, image, bmap));
    Ok(())
}

fn run_flash(app: tauri::AppHandle, bootloader: String, image: String, bmap: String) {
    let s = app.state::<AppState>();
    let uuu = s.uuu.clone();
    let script = s.snapshot().script;

    let fail = |app: &tauri::AppHandle, msg: String, cancelled: bool| {
        update(app, |st| {
            st.phase = if cancelled { "cancelled" } else { "error" }.into();
            st.error = msg;
            st.finished_at = now_ms();
        });
    };

    // 1. Decompress .wic.lz4 (cached next to the source for reuse).
    let src = PathBuf::from(&image);
    let wic = if lz4img::is_lz4_path(&src) {
        if let Some(cached) = lz4img::cached_output(&src) {
            logline(&app, format!("Using cached decompressed image {}", cached.display()));
            cached
        } else {
            logline(&app, format!("Decompressing {} ...", src.display()));
            let appc = app.clone();
            let sc = app.state::<AppState>();
            let mut last = 101u8;
            let res = lz4img::decompress_file(
                &src,
                move |read, total| {
                    if total == 0 {
                        return;
                    }
                    let pct = (read * 100 / total) as u8;
                    if pct != last {
                        last = pct;
                        update(&appc, |st| {
                            st.decomp_pct = pct;
                            st.decomp_read = read;
                            st.decomp_total = total;
                        });
                    }
                },
                move || sc.is_cancelled(),
            );
            match res {
                Ok(p) => p,
                Err(e) => {
                    let cancelled = s.is_cancelled();
                    fail(&app, e, cancelled);
                    return;
                }
            }
        }
    } else {
        src
    };

    update(&app, |st| {
        st.wic = wic.display().to_string();
        st.decomp_pct = 100;
        st.phase = "waiting".into();
    });

    // 2. Privileges (macOS: one-time admin dialog to setuid the sidecar).
    if !uuurun::is_privileged(&uuu) {
        logline(&app, "Requesting administrator access for USB...".into());
    }
    if let Err(e) = uuurun::ensure_privileged(&uuu) {
        fail(&app, e, false);
        return;
    }
    update(&app, |st| st.needs_password = false);

    // 3. uuu -bmap looks for <wic>.bmap next to the image; if the picked
    //    bmap lives elsewhere or is named differently, copy it into place.
    let bmap_target = PathBuf::from(format!("{}.bmap", wic.display()));
    if Path::new(&bmap) != bmap_target.as_path() {
        if let Err(e) = std::fs::copy(&bmap, &bmap_target) {
            fail(&app, format!("placing bmap next to image: {e}"), false);
            return;
        }
        logline(&app, format!("Copied bmap to {}", bmap_target.display()));
    }

    // 4. Run uuu in the classic two-file form: bootloader + image.
    let wic_s = wic.display().to_string();
    let args = vec![
        "-v",
        "-bmap",
        "-b",
        script.as_str(),
        bootloader.as_str(),
        wic_s.as_str(),
    ];
    logline(&app, format!("Running: uuu {}", args.join(" ")));

    let appc = app.clone();
    let sc = app.state::<AppState>();
    let result = uuurun::run(
        &uuu,
        &args,
        move |ev| handle_event(&appc, ev),
        |pid| sc.child_id.store(pid, Ordering::SeqCst),
    );

    let cancelled = s.is_cancelled();
    match result {
        Err(e) => fail(&app, e, cancelled),
        Ok(false) => fail(
            &app,
            if cancelled { "cancelled".into() } else { last_device_error(&app) },
            cancelled,
        ),
        Ok(true) => {
            update(&app, |st| {
                st.phase = "success".into();
                st.finished_at = now_ms();
                for d in st.devices.values_mut() {
                    d.done = true;
                }
            });
            logline(&app, "Flash complete".into());
        }
    }
}

fn last_device_error(app: &tauri::AppHandle) -> String {
    let st = app.state::<AppState>().snapshot();
    for d in st.devices.values() {
        if d.failed && !d.err.is_empty() {
            return with_platform_hint(d.err.clone());
        }
    }
    "uuu failed - see log".into()
}

/// Appends a fix-it hint for well-known platform USB failures.
fn with_platform_hint(err: String) -> String {
    if cfg!(windows) {
        let low = err.to_lowercase();
        if low.contains("open usb device")
            || low.contains("not_found")
            || low.contains("not_supported")
            || low.contains("access")
        {
            return format!(
                "{err} — Windows may be missing the WinUSB driver for the \
                 fastboot device: run Zadig (zadig.akeo.ie), select the board's \
                 fastboot device and install WinUSB, then flash again."
            );
        }
    }
    err
}

fn handle_event(app: &tauri::AppHandle, ev: uuurun::Event) {
    use uuurun::Event::*;
    match ev {
        Attach { dev, text } => {
            update(app, |st| {
                st.phase = "flashing".into();
                st.devices.entry(dev).or_default();
            });
            logline(app, text);
        }
        CmdStart { dev, text } => {
            let logt = format!("{dev}> {text}");
            update(app, |st| {
                let d = st.devices.entry(dev).or_default();
                d.step += 1;
                d.cmd = text;
                d.percent = 0;
            });
            logline(app, logt);
        }
        CmdOk { dev } => update(app, |st| {
            st.devices.entry(dev).or_default().percent = 100;
        }),
        CmdFail { dev, text } => {
            let logt = format!("{dev}> FAIL {text}");
            update(app, |st| {
                let d = st.devices.entry(dev).or_default();
                d.failed = true;
                d.err = text;
            });
            logline(app, logt);
        }
        Progress { dev, percent } => update(app, |st| {
            st.devices.entry(dev).or_default().percent = percent;
        }),
        Wait => {}
        Line(s) => logline(app, s),
    }
}

/// Refreshes the connected-device chip every few seconds while idle.
pub fn spawn_device_poller(app: tauri::AppHandle) {
    std::thread::spawn(move || loop {
        let s = app.state::<AppState>();
        let phase = s.snapshot().phase;
        if matches!(phase.as_str(), "idle" | "success" | "error" | "cancelled") {
            let devs = uuurun::list_devices(&s.uuu);
            let changed = devs != s.snapshot().usb_devices;
            if changed {
                update(&app, |st| st.usb_devices = devs);
            }
        }
        std::thread::sleep(std::time::Duration::from_secs(3));
    });
}
