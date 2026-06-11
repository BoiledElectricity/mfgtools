"use strict";

const $ = (id) => document.getElementById(id);
let selected = null; // server-side path of the chosen image
let state = { phase: "idle" };

function fmtSize(n) {
  if (n > 1 << 30) return (n / (1 << 30)).toFixed(2) + " GB";
  if (n > 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  return (n / 1024).toFixed(0) + " KB";
}
const busy = () => ["decompressing", "waiting", "flashing"].includes(state.phase);

// ---- choosing an image ----

$("import-btn").onclick = () => $("file-input").click();
$("file-input").onchange = () => {
  const f = $("file-input").files[0];
  if (f) uploadFile(f);
  $("file-input").value = "";
};
$("clear-btn").onclick = () => {
  if (busy()) return;
  selected = null;
  $("dz-idle").hidden = false;
  $("dz-selected").hidden = true;
  render();
};

const dz = $("dropzone");
["dragenter", "dragover"].forEach((t) =>
  document.addEventListener(t, (e) => {
    e.preventDefault();
    dz.classList.add("drag");
  })
);
["dragleave", "drop"].forEach((t) =>
  document.addEventListener(t, (e) => {
    e.preventDefault();
    if (t === "dragleave" && e.relatedTarget) return;
    dz.classList.remove("drag");
  })
);
document.addEventListener("drop", (e) => {
  const f = e.dataTransfer.files && e.dataTransfer.files[0];
  if (f && !busy()) uploadFile(f);
});

function uploadFile(f) {
  const row = $("upload-row");
  $("dz-idle").hidden = true;
  $("dz-selected").hidden = true;
  row.hidden = false;
  $("upload-label").textContent = "Loading " + f.name;
  const xhr = new XMLHttpRequest();
  xhr.open("POST", "/api/upload?name=" + encodeURIComponent(f.name));
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    const pct = Math.round((e.loaded / e.total) * 100);
    $("upload-bar").style.width = pct + "%";
    $("upload-pct").textContent = pct + "%";
  };
  const fail = (msg) => {
    row.hidden = true;
    $("upload-bar").style.width = "0%";
    $("dz-idle").hidden = false;
    alert(msg);
  };
  xhr.onload = () => {
    if (xhr.status !== 200) return fail("Could not use file: " + xhr.responseText);
    row.hidden = true;
    $("upload-bar").style.width = "0%";
    selected = JSON.parse(xhr.responseText).path;
    $("sel-name").textContent = f.name;
    $("sel-meta").textContent = fmtSize(f.size);
    $("dz-selected").hidden = false;
    render();
  };
  xhr.onerror = () => fail("Upload failed");
  xhr.send(f);
}

// ---- flashing ----

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

// ---- live state ----

const es = new EventSource("/api/events");
es.onmessage = (ev) => {
  $("conn-banner").hidden = true;
  state = JSON.parse(ev.data);
  render();
};
es.onerror = () => {
  $("conn-banner").hidden = false;
};

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
  $("phase-label").textContent = PHASE_LABEL[state.phase] || "";
  $("flash-btn").disabled = !selected || busy();
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
