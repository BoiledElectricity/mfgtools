"use strict";

const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;

const $ = (id) => document.getElementById(id);
// The three files needed for a flash: imx-boot, .wic(.lz4) image, .wic.bmap
const files = { bootloader: null, image: null, bmap: null };
let state = { phase: "idle" };

function fmtSize(n) {
  if (n > 1 << 30) return (n / (1 << 30)).toFixed(2) + " GB";
  if (n > 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  return (n / 1024).toFixed(0) + " KB";
}
const busy = () => ["decompressing", "waiting", "flashing"].includes(state.phase);

// ---- choosing the files ----

const SLOT_PLACEHOLDER = {
  bootloader: "imx-boot file",
  image: ".wic / .wic.lz4 file",
  bmap: ".wic.bmap file",
};
const SLOT_IDS = { bootloader: "boot", image: "image", bmap: "bmap" };

function setSlot(kind, f) {
  files[kind] = f;
  const id = SLOT_IDS[kind];
  const nameEl = $(id + "-name");
  if (f) {
    nameEl.textContent = f.name;
    nameEl.className = "filerow-name";
    $(id + "-meta").textContent = fmtSize(f.size);
  } else {
    nameEl.textContent = SLOT_PLACEHOLDER[kind];
    nameEl.className = "filerow-name empty-name";
    $(id + "-meta").textContent = "";
  }
  render();
}

$("pick-boot-btn").onclick = async () => {
  const f = await invoke("pick_bootloader");
  if (f) setSlot("bootloader", f);
};
$("pick-image-btn").onclick = async () => {
  const f = await invoke("pick_image");
  if (f) setSlot("image", f);
};
$("pick-bmap-btn").onclick = async () => {
  const f = await invoke("pick_bmap");
  if (f) setSlot("bmap", f);
};

// Native drag & drop: Tauri delivers real file paths; each dropped file is
// sorted into its slot by name (bmap, image, otherwise bootloader).
listen("tauri://drag-enter", () => $("dropzone").classList.add("drag"));
listen("tauri://drag-leave", () => $("dropzone").classList.remove("drag"));
listen("tauri://drag-drop", async (ev) => {
  $("dropzone").classList.remove("drag");
  if (busy()) return;
  for (const path of ev.payload.paths || []) {
    try {
      const d = await invoke("classify_dropped", { path });
      setSlot(d.kind, d.file);
    } catch (e) {
      alert(e);
    }
  }
});

// ---- flashing ----

const ready = () => files.bootloader && files.image && files.bmap;

$("flash-btn").onclick = async () => {
  if (!ready() || busy()) return;
  try {
    await invoke("start_flash", {
      bootloader: files.bootloader.path,
      image: files.image.path,
      bmap: files.bmap.path,
    });
  } catch (e) {
    alert(e);
  }
};

$("cancel-btn").onclick = () => invoke("cancel_flash");

// ---- live state ----

listen("state", (ev) => {
  state = ev.payload;
  render();
});
invoke("get_state").then((s) => {
  state = s;
  render();
});

const PHASE_LABEL = {
  decompressing: "Decompressing image…",
  waiting: "Waiting for board — connect USB and power on in serial-download mode",
  flashing: "Flashing…",
};

function render() {
  const chip = $("usb-status");
  const devs = state.usbDevices || [];
  if (devs.length) {
    chip.className = "chip chip-green";
    chip.textContent = devs.map((d) => `● ${d.chip} (${d.pro}) @ ${d.path}`).join("  ");
  } else if (busy()) {
    chip.className = "chip chip-gray";
    chip.textContent = "flashing…";
  } else {
    chip.className = "chip chip-gray";
    chip.textContent = "no board detected";
  }

  $("uuu-version").textContent = state.uuuVersion || "";
  let phaseText = PHASE_LABEL[state.phase] || "";
  if (state.phase === "waiting" && state.needsPassword) {
    phaseText = "Enter your password in the dialog to allow USB access…";
  }
  $("phase-label").textContent = phaseText;
  $("flash-btn").disabled = !ready() || busy();
  $("cancel-btn").hidden = !busy();

  const area = $("progress-area");
  area.hidden = state.phase === "idle";
  const decompRow = $("decomp-row");
  const isLz4 = (state.image || "").toLowerCase().endsWith(".lz4");
  decompRow.hidden = !isLz4 || state.phase === "idle";
  if (!decompRow.hidden) {
    $("decomp-bar").style.width = (state.decompPct || 0) + "%";
    $("decomp-bar").className = "fill" + (state.decompPct >= 100 ? " ok" : "");
    $("decomp-pct").textContent = (state.decompPct || 0) + "%";
    $("decomp-detail").textContent = state.decompTotal > 0
      ? `${fmtSize(state.decompRead)} of ${fmtSize(state.decompTotal)}`
      : "";
  }

  const rows = $("device-rows");
  rows.innerHTML = "";
  for (const [path, d] of Object.entries(state.devices || {})) {
    const card = document.createElement("div");
    card.className = "dev-card";
    const fillClass = d.failed ? "fill bad" : d.done ? "fill ok" : "fill";
    card.innerHTML = `
      <div class="dev-head">
        <span class="dev-path">Board ${path}</span>
        <span class="dev-cmd">${d.failed ? d.err : d.cmd || ""}</span>
        <span class="dev-step">step ${d.step}</span>
      </div>
      <div class="prog-row">
        <div class="bar"><div class="${fillClass}" style="width:${d.done ? 100 : d.percent}%"></div></div>
        <span class="prog-pct">${d.done ? 100 : d.percent}%</span>
      </div>`;
    rows.appendChild(card);
  }

  const banner = $("result-banner");
  if (state.phase === "success") {
    const secs = Math.round((state.finishedAt - state.startedAt) / 1000);
    banner.hidden = false;
    banner.className = "ok";
    banner.textContent = `✅ Flash complete (${Math.floor(secs / 60)}m ${secs % 60}s) — board can be rebooted`;
  } else if (state.phase === "error") {
    banner.hidden = false;
    banner.className = "bad";
    banner.textContent = "❌ Flash failed: " + (state.error || "unknown error");
  } else if (state.phase === "cancelled") {
    banner.hidden = false;
    banner.className = "bad";
    banner.textContent = "⏹ Cancelled";
  } else {
    banner.hidden = true;
  }

  const log = $("log");
  const txt = (state.log || []).join("\n");
  if (log.textContent !== txt) {
    log.textContent = txt;
    log.scrollTop = log.scrollHeight;
  }
}
