// Board Flasher — native Tauri app around NXP's uuu.
//
// The stock uuu binary ships as a Tauri sidecar next to the app executable.
// Flashing runs `uuu -v -bmap -b emmc_all <image>`; uuu's verbose output is
// parsed into a State struct that is pushed to the webview as a "state"
// event after every change. Yocto .wic.lz4 images (LZ4 legacy frames) are
// decompressed in-process first, cached next to the source file.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod flasher;
mod lz4img;
mod uuurun;

use flasher::{AppState, State};
use tauri::Manager;

#[tauri::command]
fn get_state(app: tauri::AppHandle) -> State {
    app.state::<AppState>().snapshot()
}

#[tauri::command]
async fn pick_image(app: tauri::AppHandle) -> Option<flasher::PickedFile> {
    use tauri_plugin_dialog::DialogExt;
    let path = app
        .dialog()
        .file()
        .add_filter("Board images", &["wic", "lz4", "zst", "gz", "bz2"])
        .blocking_pick_file()?;
    let path = path.into_path().ok()?;
    flasher::picked_image(&path)
}

#[tauri::command]
async fn pick_bootloader(app: tauri::AppHandle) -> Option<flasher::PickedFile> {
    use tauri_plugin_dialog::DialogExt;
    // No extension filter: bootloaders are usually named just "imx-boot".
    let path = app.dialog().file().blocking_pick_file()?;
    let path = path.into_path().ok()?;
    flasher::picked_bootloader(&path)
}

#[tauri::command]
async fn pick_bmap(app: tauri::AppHandle) -> Option<flasher::PickedFile> {
    use tauri_plugin_dialog::DialogExt;
    let path = app
        .dialog()
        .file()
        .add_filter("Block map", &["bmap"])
        .blocking_pick_file()?;
    let path = path.into_path().ok()?;
    flasher::picked_bmap(&path)
}

#[tauri::command]
fn classify_dropped(path: String) -> Result<flasher::Dropped, String> {
    flasher::classify_dropped(std::path::Path::new(&path))
}

#[tauri::command]
fn start_flash(
    app: tauri::AppHandle,
    bootloader: String,
    image: String,
    bmap: String,
) -> Result<(), String> {
    flasher::start_flash(app, bootloader, image, bmap)
}

#[tauri::command]
fn cancel_flash(app: tauri::AppHandle) {
    app.state::<AppState>().cancel();
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .setup(|app| {
            let exe = uuurun::sidecar_path(app.handle())?;
            let version = uuurun::version(&exe);
            app.manage(AppState::new(exe, version));
            flasher::spawn_device_poller(app.handle().clone());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_state,
            pick_image,
            pick_bootloader,
            pick_bmap,
            classify_dropped,
            start_flash,
            cancel_flash
        ])
        .on_window_event(|window, event| {
            // Closing the window cancels any running uuu so it doesn't keep
            // the USB device claimed.
            if let tauri::WindowEvent::CloseRequested { .. } = event {
                window.app_handle().state::<AppState>().cancel();
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running Board Flasher");
}
