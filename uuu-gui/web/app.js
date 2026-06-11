"use strict";

const $ = (id) => document.getElementById(id);
let selected = null;
let state = { phase: "idle" };

function fmtSize(n) {
  if (n > 1 << 30) return (n / (1 << 30)).toFixed(2) + " GB";
  if (n > 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  return (n / 1024).toFixed(0) + " KB";
}
function fmtWhen(ms) {
  const d = new Date(ms);
  return d.toLocaleDateString() + " " + d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
const busy = () => ["decompressing", "waiting", "flashing"].includes(state.phase);

// ---- image list ----

async function loadImages() {
  const res = await fetch("/api/images");
  const data = await res.json() || {};
  const images = data.images || [];
  $("scan-hint").textContent =
    "Drag & drop an image anywhere on this page, or pick one found in: " +
    (data.dirs || []).join("  ·  ");
  const list = $("image-list");
  list.innerHTML = "";
  if (!images.length) {
    list.innerHTML = '<div class="empty">No .wic images found.<br>Drag &amp; drop a .wic / .wic.lz4 file anywhere on this page.</div>';
    return;
  }
  if (!selected) selected = images[0].path; // preselect newest
  for (const img of images) {
    const item = document.createElement("div");
    item.className = "image-item" + (selected === img.path ? " selected" : "");
    item.innerHTML = `
      <div>
        <div class="image-name">${img.name}</div>
        <div class="image-meta">${fmtSize(img.size)} · ${fmtWhen(img.modTime)} · ${img.path.slice(0, img.path.length - img.name.length - 1)}</div>
      </div>
      <div class="badges">
        ${img.kind !== "wic" ? `<span class="badge lz4">${img.kind.toUpperCase()}</span>` : ""}
        ${img.cached ? '<span class="badge cached">CACHED</span>' : ""}
        ${img.hasBmap ? '<span class="badge bmap">BMAP</span>' : ""}
      </div>`;
    item.onclick = () => {
      selected = img.path;
      document.querySelectorAll(".image-item").forEach((el) => el.classList.remove("selected"));
      item.classList.add("selected");
      render();
    };
    list.appendChild(item);
  }
  render();
}

// ---- actions ----

$("refresh-btn").onclick = loadImages;

$("import-btn").onclick = () => $("file-input").click();
$("file-input").onchange = () => {
  const f = $("file-input").files[0];
  if (f) uploadFile(f);
  $("file-input").value = "";
};

$("flash-btn").onclick = async () => {
  if (!selected || busy()) return;
  const res = await fetch("/api/flash", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path: selected }),
  });
  if (!res.ok) alert(await res.text());
};

$("cancel-btn").onclick = () => fetch("/api/cancel", { method: "POST" });

// ---- drag & drop ----

let dragDepth = 0;
document.addEventListener("dragenter", (e) => {
  e.preventDefault();
  if (++dragDepth === 1) $("drop-overlay").hidden = false;
});
document.addEventListener("dragleave", (e) => {
  e.preventDefault();
  if (--dragDepth <= 0) { dragDepth = 0; $("drop-overlay").hidden = true; }
});
document.addEventListener("dragover", (e) => e.preventDefault());
document.addEventListener("drop", (e) => {
  e.preventDefault();
  dragDepth = 0;
  $("drop-overlay").hidden = true;
  const f = e.dataTransfer.files && e.dataTransfer.files[0];
  if (f) uploadFile(f);
});

function uploadFile(f) {
  const row = $("upload-row");
  row.hidden = false;
  $("upload-label").textContent = "Receiving " + f.name;
  const xhr = new XMLHttpRequest();
  xhr.open("POST", "/api/upload?name=" + encodeURIComponent(f.name));
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    const pct = Math.round((e.loaded / e.total) * 100);
    $("upload-bar").style.width = pct + "%";
    $("upload-pct").textContent = pct + "%";
  };
  xhr.onload = async () => {
    row.hidden = true;
    $("upload-bar").style.width = "0%";
    if (xhr.status !== 200) {
      alert("Upload failed: " + xhr.responseText);
      return;
    }
    selected = JSON.parse(xhr.responseText).path;
    await loadImages();
  };
  xhr.onerror = () => { row.hidden = true; alert("Upload failed"); };
  xhr.send(f);
}

// ---- live state ----

const es = new EventSource("/api/events");
es.onmessage = (ev) => {
  state = JSON.parse(ev.data);
  render();
};

const PHASE_LABEL = {
  idle: "",
  decompressing: "Decompressing image…",
  waiting: "Waiting for board — connect USB and power on in serial-download mode",
  flashing: "Flashing…",
  success: "",
  error: "",
  cancelled: "",
};

function render() {
  // USB chip
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
  $("phase-label").textContent = PHASE_LABEL[state.phase] || "";
  $("flash-btn").disabled = !selected || busy();
  $("cancel-btn").hidden = !busy();

  // progress
  const area = $("progress-area");
  area.hidden = state.phase === "idle";
  const decompRow = $("decomp-row");
  const isLz4 = (state.image || "").toLowerCase().endsWith(".lz4");
  decompRow.hidden = !isLz4 || state.phase === "idle";
  if (!decompRow.hidden) {
    $("decomp-bar").style.width = (state.decompPct || 0) + "%";
    $("decomp-bar").className = "fill" + (state.decompPct >= 100 ? " ok" : "");
    $("decomp-pct").textContent = (state.decompPct || 0) + "%";
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

  // result banner
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

  // log
  const log = $("log");
  const txt = (state.log || []).join("\n");
  if (log.textContent !== txt) {
    log.textContent = txt;
    log.scrollTop = log.scrollHeight;
  }
}

loadImages();
