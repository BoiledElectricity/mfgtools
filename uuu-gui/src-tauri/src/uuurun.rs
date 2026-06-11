//! Locating and running the bundled uuu sidecar, and parsing its output.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::OnceLock;

use regex::Regex;

/// One parsed line of `uuu -v` output.
#[derive(Debug, Clone, PartialEq)]
pub enum Event {
    Attach { dev: String, text: String },
    CmdStart { dev: String, text: String },
    CmdOk { dev: String },
    CmdFail { dev: String, text: String },
    Progress { dev: String, percent: u8 },
    Wait,
    Line(String),
}

#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct Device {
    pub path: String,
    pub chip: String,
    pub pro: String,
    pub vid: String,
    pub pid: String,
    pub serial: String,
}

/// The uuu sidecar sits next to the app executable (Tauri externalBin).
/// UUU_GUI_UUU overrides it for development.
pub fn sidecar_path(_app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn std::error::Error>> {
    if let Ok(p) = std::env::var("UUU_GUI_UUU") {
        return Ok(PathBuf::from(p));
    }
    let exe = std::env::current_exe()?;
    let dir = exe.parent().ok_or("no executable directory")?;
    let name = if cfg!(windows) { "uuu.exe" } else { "uuu" };
    let p = dir.join(name);
    if !p.exists() {
        return Err(format!("bundled uuu not found at {}", p.display()).into());
    }
    Ok(p)
}

/// Command builder that never flashes a console window on Windows
/// (the device poller would otherwise strobe one every few seconds).
fn quiet_command(exe: &Path) -> Command {
    #[allow(unused_mut)]
    let mut cmd = Command::new(exe);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        cmd.creation_flags(0x08000000); // CREATE_NO_WINDOW
    }
    cmd
}

pub fn version(exe: &Path) -> String {
    let out = quiet_command(exe).output();
    match out {
        Ok(o) => String::from_utf8_lossy(&o.stdout)
            .lines()
            .next()
            .unwrap_or("")
            .trim()
            .to_string(),
        Err(_) => String::new(),
    }
}

pub fn list_devices(exe: &Path) -> Vec<Device> {
    let out = match quiet_command(exe).arg("-lsusb").output() {
        Ok(o) => String::from_utf8_lossy(&o.stdout).to_string(),
        Err(_) => return vec![],
    };
    parse_lsusb(&out)
}

pub fn parse_lsusb(out: &str) -> Vec<Device> {
    let mut devs = vec![];
    let mut saw_sep = false;
    for line in strip_ansi(out).lines() {
        let line = line.trim();
        if line.starts_with("====") {
            saw_sep = true;
            continue;
        }
        if !saw_sep || line.is_empty() {
            continue;
        }
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() < 5 || !f[3].starts_with("0x") {
            continue;
        }
        devs.push(Device {
            path: f[0].into(),
            chip: f[1].into(),
            pro: f[2].trim_end_matches(':').into(),
            vid: f[3].into(),
            pid: f[4].into(),
            serial: f.get(6).map(|s| s.to_string()).unwrap_or_default(),
        });
    }
    devs
}

fn spawn(exe: &Path, args: &[&str]) -> std::io::Result<Child> {
    let mut cmd = quiet_command(exe);
    cmd.args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd.spawn()
}

/// Runs uuu, streaming parsed events live as output arrives. The child's pid
/// is reported through `set_child_id` so the caller can kill it to cancel.
pub fn run(
    exe: &Path,
    args: &[&str],
    mut on_event: impl FnMut(Event),
    set_child_id: impl FnOnce(u32),
) -> Result<bool, String> {
    let mut child = spawn(exe, args).map_err(|e| format!("starting uuu: {e}"))?;
    set_child_id(child.id());

    let stdout = child.stdout.take().expect("stdout piped");
    let stderr = child.stderr.take().expect("stderr piped");

    // Forward both pipes into one channel of raw chunks.
    let (tx, rx) = std::sync::mpsc::channel::<Vec<u8>>();
    for mut pipe in [
        Box::new(stdout) as Box<dyn Read + Send>,
        Box::new(stderr) as Box<dyn Read + Send>,
    ] {
        let tx = tx.clone();
        std::thread::spawn(move || {
            let mut tmp = [0u8; 8192];
            while let Ok(n) = pipe.read(&mut tmp) {
                if n == 0 || tx.send(tmp[..n].to_vec()).is_err() {
                    break;
                }
            }
        });
    }
    drop(tx);

    let mut buf = Vec::new();
    let mut parser = Parser::default();
    let mut events = Vec::new();
    for chunk in rx {
        buf.extend_from_slice(&chunk);
        drain_lines(&mut buf, &mut parser, &mut events);
        for e in events.drain(..) {
            on_event(e);
        }
    }
    buf.push(b'\n');
    drain_lines(&mut buf, &mut parser, &mut events);
    for e in events.drain(..) {
        on_event(e);
    }

    let status = child.wait().map_err(|e| e.to_string())?;
    Ok(status.success())
}

fn drain_lines(buf: &mut Vec<u8>, p: &mut Parser, out: &mut Vec<Event>) {
    while let Some(i) = buf.iter().position(|&b| b == b'\n' || b == b'\r') {
        let line: Vec<u8> = buf.drain(..=i).collect();
        let s = String::from_utf8_lossy(&line[..line.len() - 1]).to_string();
        if let Some(e) = p.line(&s) {
            out.push(e);
        }
    }
}

fn strip_ansi(s: &str) -> String {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new("\x1b\\[[0-9;?]*[A-Za-z]").unwrap())
        .replace_all(s, "")
        .into_owned()
}

#[derive(Default)]
pub struct Parser {
    last_dev: String,
}

impl Parser {
    pub fn line(&mut self, raw: &str) -> Option<Event> {
        static ATTACH: OnceLock<Regex> = OnceLock::new();
        static START: OnceLock<Regex> = OnceLock::new();
        static OK: OnceLock<Regex> = OnceLock::new();
        static FAIL: OnceLock<Regex> = OnceLock::new();
        static PCT: OnceLock<Regex> = OnceLock::new();

        let s = strip_ansi(raw);
        let s = s.trim();
        if s.is_empty() {
            return None;
        }

        let attach = ATTACH.get_or_init(|| Regex::new(r"^New USB Device Attached at (\S+)").unwrap());
        let start = START.get_or_init(|| Regex::new(r"^(\S+)>Start Cmd:\s*(.*)$").unwrap());
        let ok = OK.get_or_init(|| Regex::new(r"^(\S+)>Okay \([0-9.]+s\)").unwrap());
        let fail = FAIL.get_or_init(|| Regex::new(r"^(\S+)>Fail (.*)$").unwrap());
        let pct = PCT.get_or_init(|| Regex::new(r"^([0-9]{1,3})%$").unwrap());

        if let Some(m) = attach.captures(s) {
            self.last_dev = m[1].to_string();
            return Some(Event::Attach { dev: m[1].into(), text: s.into() });
        }
        if let Some(m) = start.captures(s) {
            self.last_dev = m[1].to_string();
            return Some(Event::CmdStart { dev: m[1].into(), text: m[2].into() });
        }
        if let Some(m) = ok.captures(s) {
            return Some(Event::CmdOk { dev: m[1].into() });
        }
        if let Some(m) = fail.captures(s) {
            return Some(Event::CmdFail { dev: m[1].into(), text: m[2].into() });
        }
        if let Some(m) = pct.captures(s) {
            if let Ok(n) = m[1].parse::<u8>() {
                if n <= 100 && !self.last_dev.is_empty() {
                    return Some(Event::Progress { dev: self.last_dev.clone(), percent: n });
                }
            }
            return None;
        }
        if s.starts_with("Wait for ") {
            return Some(Event::Wait);
        }
        Some(Event::Line(s.into()))
    }
}

/// Kill a uuu child by pid (used for cancel; works even though uuu may be
/// setuid-root on macOS because the real uid still matches).
pub fn kill(pid: u32) {
    #[cfg(unix)]
    unsafe {
        libc_kill(pid as i32, 9);
    }
    #[cfg(windows)]
    {
        let _ = quiet_command(Path::new("taskkill"))
            .args(["/PID", &pid.to_string(), "/T", "/F"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status();
    }
}

#[cfg(unix)]
extern "C" {
    #[link_name = "kill"]
    fn libc_kill(pid: i32, sig: i32) -> i32;
}

/// On macOS uuu needs root to detach the HID kernel driver. Make the sidecar
/// setuid-root once via the native admin-password dialog; later flashes run
/// without prompting. On Linux require root or udev rules up front.
pub fn ensure_privileged(exe: &Path) -> Result<(), String> {
    #[cfg(target_os = "macos")]
    {
        if is_setuid_root(exe) || unsafe { libc_geteuid() } == 0 {
            return Ok(());
        }
        let p = exe.display().to_string();
        let script = format!(
            "do shell script \"/usr/sbin/chown root:wheel '{p}' && /bin/chmod 4755 '{p}'\" \
             with administrator privileges \
             with prompt \"Board Flasher needs administrator access to talk to the board over USB.\""
        );
        let out = Command::new("/usr/bin/osascript")
            .args(["-e", &script])
            .output()
            .map_err(|e| e.to_string())?;
        if !out.status.success() {
            return Err(format!(
                "administrator access not granted: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        if !is_setuid_root(exe) {
            return Err("could not make uuu privileged".into());
        }
        return Ok(());
    }
    #[cfg(target_os = "linux")]
    {
        if unsafe { libc_geteuid() } != 0 && !is_setuid_root(exe) {
            return Err("USB access needs root: run with sudo, or install uuu udev rules".into());
        }
        return Ok(());
    }
    #[allow(unreachable_code)]
    {
        let _ = exe;
        Ok(())
    }
}

/// True when no privilege prompt will be needed before flashing.
pub fn is_privileged(exe: &Path) -> bool {
    #[cfg(any(target_os = "macos", target_os = "linux"))]
    {
        return is_setuid_root(exe) || unsafe { libc_geteuid() } == 0;
    }
    #[allow(unreachable_code)]
    {
        let _ = exe;
        true
    }
}

#[cfg(unix)]
fn is_setuid_root(exe: &Path) -> bool {
    use std::os::unix::fs::MetadataExt;
    match exe.metadata() {
        Ok(m) => m.uid() == 0 && (m.mode() & 0o4000) != 0,
        Err(_) => false,
    }
}

#[cfg(unix)]
extern "C" {
    #[link_name = "geteuid"]
    fn libc_geteuid() -> u32;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_verbose_output() {
        let mut p = Parser::default();
        assert_eq!(
            p.line("New USB Device Attached at 1:16"),
            Some(Event::Attach { dev: "1:16".into(), text: "New USB Device Attached at 1:16".into() })
        );
        assert_eq!(
            p.line("1:16>Start Cmd:SDPS: boot -f x.wic"),
            Some(Event::CmdStart { dev: "1:16".into(), text: "SDPS: boot -f x.wic".into() })
        );
        assert_eq!(p.line("\x1b[33m42%\x1b[0m"), Some(Event::Progress { dev: "1:16".into(), percent: 42 }));
        assert_eq!(p.line("\x1b[32m1:16>Okay (0.53s)\x1b[0m"), Some(Event::CmdOk { dev: "1:16".into() }));
        assert_eq!(
            p.line("1:16>Fail Bulk(W):LIBUSB_ERROR_IO(2.1s)"),
            Some(Event::CmdFail { dev: "1:16".into(), text: "Bulk(W):LIBUSB_ERROR_IO(2.1s)".into() })
        );
    }

    #[test]
    fn parses_lsusb() {
        let out = "uuu (Universal Update Utility)\n\nConnected Known USB Devices\n\
\tPath\t Chip\t Pro\t Vid\t Pid\t BcdVersion\t Serial No\n\
\t====================================\n\
\t1:16\t MX8MN\t SDPS:\t 0x1FC9\t0x0132\t 0x0001\t XXXX\n";
        let d = parse_lsusb(out);
        assert_eq!(d.len(), 1);
        assert_eq!(d[0].path, "1:16");
        assert_eq!(d[0].chip, "MX8MN");
        assert_eq!(d[0].pro, "SDPS");
    }
}
