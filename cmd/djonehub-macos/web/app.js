const $ = (selector) => document.querySelector(selector);
let lastSMSCount = null;
let selectedSMSSender = null;
let esimHealthPollTimer = null;
let esimHealthInFlight = false;
let networkTrafficTimer = null;
let networkTrafficPrevious = null;
let networkTrafficInFlight = false;
let callPollInFlight = false;
let lastActiveCallID = null;
let cellularPolicyBusy = false;
let gpsRefreshTimer = null;
let gpsRefreshInFlight = false;
let platformCapabilities = {
  os: "darwin",
  call_audio: true,
  direct_usb_at: true,
  esim_full: true,
  network_policy_native: true,
};

function setThemePreference(theme) {
  if (theme === "light" || theme === "dark") {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("djonehub-theme", theme);
    localStorage.removeItem("vohive-theme");
  } else {
    delete document.documentElement.dataset.theme;
    localStorage.removeItem("djonehub-theme");
    localStorage.removeItem("vohive-theme");
  }
  document.querySelectorAll("[data-theme-option]").forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.themeOption === theme));
  });
}

const savedTheme = localStorage.getItem("djonehub-theme") || localStorage.getItem("vohive-theme");
setThemePreference(savedTheme === "light" || savedTheme === "dark" ? savedTheme : "auto");
document.querySelectorAll("[data-theme-option]").forEach((button) => {
  button.addEventListener("click", () => setThemePreference(button.dataset.themeOption));
});

const operatorNames = new Map([
  ["CHN-UNICOM", "中国联通"],
  ["CHINA UNICOM", "中国联通"],
  ["UNICOM", "中国联通"],
  ["46001", "中国联通"],
  ["46006", "中国联通"],
  ["46009", "中国联通"],
  ["CHINA MOBILE", "中国移动"],
  ["CMCC", "中国移动"],
  ["CHN-CMCC", "中国移动"],
  ["46000", "中国移动"],
  ["46002", "中国移动"],
  ["46004", "中国移动"],
  ["46007", "中国移动"],
  ["46008", "中国移动"],
  ["CHINA TELECOM", "中国电信"],
  ["CHN-CT", "中国电信"],
  ["CTCC", "中国电信"],
  ["46003", "中国电信"],
  ["46005", "中国电信"],
  ["46011", "中国电信"],
  ["CBN", "中国广电"],
  ["CHN-CBN", "中国广电"],
  ["CHINA BROADNET", "中国广电"],
  ["46015", "中国广电"],
]);

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}

async function loadPlatform() {
  try {
    platformCapabilities = await api("/api/platform");
  } catch (_) {
    return;
  }
  const platformName = { windows: "Windows", linux: "Linux" }[platformCapabilities.os];
  if (!platformName) return;

  document.title = `DJOneHub for ${platformName}`;
  const platformLabel = document.querySelector(".app-header h1 span");
  if (platformLabel) platformLabel.textContent = platformName;

  if (!platformCapabilities.call_audio) {
    const audioRow = document.querySelector(".audio-row");
    if (audioRow) audioRow.hidden = true;
  }
  if (!platformCapabilities.esim_full) {
    const esimNav = document.querySelector('[data-view="esim"]');
    const esimView = document.querySelector("#esim");
    if (esimNav) esimNav.hidden = true;
    if (esimView) esimView.hidden = true;
  }
  if (!platformCapabilities.network_policy_native) {
    const policy = document.querySelector("#cellular-policy-toggle")?.closest(".network-policy-control");
    if (policy) policy.hidden = true;
  }
}

function renderCellularPolicy(state) {
  const button = $("#cellular-policy-toggle");
  const forceOff = Boolean(state?.force_off);
  const enabled = !forceOff;
  button.setAttribute("aria-checked", String(enabled));
  setNetworkTrafficPolling(enabled);
  button.title = forceOff
    ? "开启 Wi‑Fi 优先、4G 自动备用"
    : "关闭后强制禁止 Mac 使用 4G；短信和来电监控不受影响";
}

async function loadCellularPolicy() {
  try {
    renderCellularPolicy(await api("/api/network/cellular-policy"));
  } catch (error) {
    $("#cellular-policy-toggle").title = error.message;
  }
}

$("#cellular-policy-toggle").addEventListener("click", async () => {
  if (cellularPolicyBusy) return;
  cellularPolicyBusy = true;
  const button = $("#cellular-policy-toggle");
  button.disabled = true;
  const forceOff = button.getAttribute("aria-checked") === "true";
  try {
    const state = await api("/api/network/cellular-policy", {
      method: "POST",
      body: JSON.stringify({ force_off: forceOff }),
    });
    renderCellularPolicy(state);
    notice(forceOff ? "已强制关闭 4G，短信和来电监控保持运行" : "已恢复 Wi‑Fi 优先、4G 自动备用");
  } catch (error) {
    notice(error.message);
    await loadCellularPolicy();
  } finally {
    button.disabled = false;
    cellularPolicyBusy = false;
  }
});

function notice(message) {
  const el = $("#notice");
  el.textContent = message;
  el.classList.add("show");
  clearTimeout(notice.timer);
  notice.timer = setTimeout(() => el.classList.remove("show"), 2600);
}

let modalResolve = null;

function closeModal(result = null) {
  const modal = $("#app-modal");
  modal.hidden = true;
  document.body.classList.remove("modal-open");
  if (modalResolve) {
    const resolve = modalResolve;
    modalResolve = null;
    resolve(result);
  }
}

function showModal({ title, message = "", fields = [], confirmLabel = "确定", danger = false }) {
  if (modalResolve) closeModal(null);
  const modal = $("#app-modal");
  const messageElement = $("#modal-message");
  const fieldsElement = $("#modal-fields");
  const confirmButton = $("#modal-confirm");
  $("#modal-title").textContent = title;
  messageElement.textContent = message;
  messageElement.hidden = !message;
  fieldsElement.replaceChildren(...fields.map((field) => {
    const label = document.createElement("label");
    label.className = "modal-field";
    const caption = document.createElement("span");
    caption.textContent = field.label;
    const input = document.createElement("input");
    input.name = field.name;
    input.value = field.value || "";
    input.placeholder = field.placeholder || "";
    input.autocomplete = "off";
    if (field.required) input.required = true;
    label.append(caption, input);
    return label;
  }));
  confirmButton.textContent = confirmLabel;
  confirmButton.className = danger ? "danger modal-danger" : "";
  modal.hidden = false;
  document.body.classList.add("modal-open");
  const firstInput = fieldsElement.querySelector("input");
  setTimeout(() => (firstInput || confirmButton).focus(), 0);
  return new Promise((resolve) => { modalResolve = resolve; });
}

$("#modal-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const values = {};
  event.currentTarget.querySelectorAll(".modal-fields input").forEach((input) => {
    values[input.name] = input.value.trim();
  });
  closeModal(values);
});
$("#modal-cancel").addEventListener("click", () => closeModal(null));
$("#modal-close").addEventListener("click", () => closeModal(null));
$("#app-modal").addEventListener("click", (event) => {
  if (event.target === event.currentTarget) closeModal(null);
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !$("#app-modal").hidden) closeModal(null);
});

async function copySMSCode(code) {
  try {
    await navigator.clipboard.writeText(code);
    notice(`验证码 ${code} 已复制`);
  } catch (error) {
    notice("复制失败，请手动复制验证码");
  }
}

function renderHardwareDetails(status) {
  const panel = $("#hardware-details");
  const device = status.usb_device;
  if (!device && !status.discovery_error) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }

  const title = document.createElement("strong");
  title.textContent = device ? "已检测到大疆 USB 设备" : "未检测到可用硬件";

  const detail = document.createElement("p");
  if (device) {
    const interfaceText = Array.isArray(device.interfaces)
      ? `${device.interfaces.length} 个 USB interface`
      : "interface 未知";
    detail.textContent = [
      `${device.vendor || "DJI"} ${device.product || ""}`.trim(),
      `${device.vendor_id}:${device.product_id}`,
      device.mode,
      interfaceText,
    ].filter(Boolean).join(" · ");
  } else {
    detail.textContent = status.discovery_error || "设备未枚举";
  }

  const hint = document.createElement("small");
  hint.textContent = status.discovery_error
    ? `当前限制：${status.discovery_error}`
    : "AT 串口可用后，短信和 eSIM/卡片操作会自动启用。";

  panel.hidden = false;
  panel.replaceChildren(title, detail, hint);
}

function setValue(id, text, tone = "") {
  const el = $(id);
  el.textContent = text || "--";
  el.className = tone;
}

function displayOperatorName(value) {
  const raw = String(value || "").trim();
  if (!raw) return "--";
  return operatorNames.get(raw.toUpperCase()) || raw;
}

function displayWorkMode(value) {
	if (value === null || value === undefined || value === "") {
	  return { label: "待读取", tone: "muted" };
	}
  switch (Number(value)) {
    case 0: return { label: "传统短信模式", tone: "muted" };
    case 1: return { label: "短信常驻 · 4G 备用", tone: "info" };
    case 2: return { label: "实验模式 2", tone: "warn" };
    case 3: return { label: "实验模式 3", tone: "warn" };
    default: return { label: "待读取", tone: "muted" };
  }
}

function signalTone(dbm) {
  const value = Number(dbm);
  if (!Number.isFinite(value) || value === 0) return "muted";
  if (value >= -65) return "good";
  if (value >= -75) return "signal-fair";
  if (value >= -85) return "warn";
  if (value >= -95) return "orange";
  return "bad";
}

async function loadStatus() {
  try {
    const status = await api("/api/status");
    setValue("#operator", displayOperatorName(status.operator), status.operator ? "info" : "muted");
    setValue("#signal", status.signal_dbm ? `${status.signal_dbm} dBm` : "--", signalTone(status.signal_dbm));
    setValue("#network-mode", status.network_mode || status.reg_status_text || "--", status.network_mode ? "info" : "muted");
    setValue(
      "#sim",
      status.sim_inserted ? "已插入" : (status.usb_device ? "待读取" : "未检测到"),
      status.sim_inserted ? "good" : (status.usb_device ? "warn" : "bad"),
    );
    const workMode = Object.prototype.hasOwnProperty.call(status, "usbnet_mode")
      ? displayWorkMode(status.usbnet_mode)
      : displayWorkMode(null);
    setValue("#work-mode", workMode.label, workMode.tone);
    $("#device-summary").textContent =
      status.hardware_status || [status.imei, status.firmware].filter(Boolean).join(" · ") || "模块初始化中";
    renderHardwareDetails(status);
  } catch (error) {
    $("#device-summary").textContent = error.message;
  }
}

function smsShortTime(timestamp) {
  const date = new Date(timestamp);
  const now = new Date();
  if (date.toDateString() === now.toDateString()) {
    return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
  }
  return date.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
}

function groupSMSCConversations(messages) {
  const groups = new Map();
  for (const message of messages) {
    const key = message.sender || "未知号码";
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(message);
  }
  return [...groups.entries()].map(([sender, items]) => ({
    sender,
    items,
    last: items[0],
  }));
}

function renderSMSThread(messages, sender) {
  const thread = $("#sms-thread");
  thread.replaceChildren();
  const head = document.createElement("div");
  head.className = "thread-head";
  const name = document.createElement("strong");
  name.textContent = sender;
  const meta = document.createElement("small");
  meta.textContent = `${messages.length} 条短信`;
  head.append(name, meta);

  const list = document.createElement("div");
  list.className = "thread-list";
  [...messages].reverse().forEach((message) => {
    const bubble = document.createElement("div");
    bubble.className = "thread-msg received";
    const content = document.createElement("span");
    content.textContent = message.content;
    bubble.append(content);
    if (message.code) {
      const actions = document.createElement("span");
      actions.className = "sms-actions";
      const badge = document.createElement("span");
      badge.className = "code-badge";
      badge.textContent = `验证码 ${message.code}`;
      const copy = document.createElement("button");
      copy.className = "secondary compact";
      copy.type = "button";
      copy.textContent = "复制";
      copy.addEventListener("click", () => copySMSCode(message.code));
      actions.append(badge, copy);
      bubble.append(actions);
    }
    const time = document.createElement("time");
    time.textContent = new Date(message.timestamp).toLocaleString("zh-CN");
    bubble.append(time);
    list.append(bubble);
  });

  const reply = document.createElement("form");
  reply.className = "thread-reply";
  const input = document.createElement("input");
  input.type = "text";
  input.placeholder = "回复短信…";
  input.maxLength = 1000;
  const button = document.createElement("button");
  button.type = "submit";
  button.textContent = "发送";
  reply.append(input, button);
  reply.addEventListener("submit", async (event) => {
    event.preventDefault();
    const content = input.value.trim();
    if (!content) return;
    button.disabled = true;
    try {
      const result = await api("/api/sms/send", {
        method: "POST",
        body: JSON.stringify({ phone: sender, message: content }),
      });
      notice(`已发送（${result.segments || 1} 条）`);
      input.value = "";
      await loadSMS();
    } catch (error) {
      notice(`发送失败：${error.message}`);
    } finally {
      button.disabled = false;
    }
  });

  thread.append(head, list, reply);
}

function renderSMSCConversations(conversations) {
  const container = $("#sms-conversations");
  if (!conversations.length) {
    container.className = "conversation-list empty";
    container.textContent = "暂无短信";
    $("#sms-thread").replaceChildren();
    return;
  }
  const selected = selectedSMSSender && conversations.some((c) => c.sender === selectedSMSSender)
    ? selectedSMSSender
    : conversations[0].sender;
  selectedSMSSender = selected;
  container.className = "conversation-list";
  container.replaceChildren(...conversations.map((conv) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `conversation-item${conv.sender === selected ? " active" : ""}`;
    const name = document.createElement("strong");
    name.textContent = conv.sender;
    const time = document.createElement("time");
    time.className = "conversation-time";
    time.textContent = smsShortTime(conv.last.timestamp);
    const snippet = document.createElement("span");
    snippet.className = "conversation-snippet";
    snippet.textContent = conv.last.content;
    button.append(name, time, snippet);
    button.addEventListener("click", () => {
      selectedSMSSender = conv.sender;
      renderSMSCConversations(conversations);
      renderSMSThread(conv.items, conv.sender);
    });
    return button;
  }));
  const current = conversations.find((c) => c.sender === selected);
  if (current) renderSMSThread(current.items, current.sender);
}

async function loadSMS() {
  try {
    const [messages, status] = await Promise.all([
      api("/api/sms"),
      api("/api/sms/status"),
    ]);
    const pollText = status.polling
      ? `自动轮询 ${status.poll_interval_s || 8}s`
      : "自动轮询未启用";
    const cleanupText = status.auto_cleanup_me ? "自动清理 ME 已开启" : "自动清理 ME 未开启";
    const errorText = status.last_poll_error ? ` · 最近错误：${status.last_poll_error}` : "";
    $("#sms-status").textContent = `当前缓存 ${messages.length} 条短信 · ${pollText} · ${cleanupText}${errorText}`;
    if (lastSMSCount !== null && messages.length > lastSMSCount) {
      notice(`收到 ${messages.length - lastSMSCount} 条新短信`);
    }
    lastSMSCount = messages.length;
    renderSMSCConversations(groupSMSCConversations(messages));
  } catch (error) {
    $("#sms-status").textContent = `读取列表失败：${error.message}`;
    notice(error.message);
  }
}

function callStateLabel(call) {
  switch (call?.state) {
    case "incoming": return "正在来电";
    case "waiting": return "来电等待";
    case "active": return "通话已接通";
    case "dialing": return "正在拨号";
    case "alerting": return "等待接听";
    case "held": return "通话保持";
    default: return "通话状态";
  }
}

function formatCallDuration(call) {
  if (!call.ended_at) return "进行中";
  const seconds = Math.max(0, Math.round((new Date(call.ended_at) - new Date(call.started_at)) / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes}:${String(seconds % 60).padStart(2, "0")}`;
}

function renderCallHistory(history) {
  const list = $("#call-history");
  const rows = Array.isArray(history) ? history : [];
  $("#call-history-count").textContent = `${rows.length} 条`;
  if (!rows.length) {
    list.className = "list empty";
    list.textContent = "暂无记录";
    return;
  }
  list.className = "list";
  list.replaceChildren(...rows.map((call) => {
    const row = document.createElement("article");
    row.className = "item call-history-item";
    const nameWrap = document.createElement("div");
    nameWrap.style.display = "flex";
    nameWrap.style.alignItems = "center";
    nameWrap.style.gap = "8px";
    const direction = document.createElement("span");
    direction.className = "call-dir";
    direction.textContent = call.missed ? "!" : (call.direction === "outgoing" ? "↑" : "↓");
    direction.title = call.missed ? "未接来电" : (call.direction === "outgoing" ? "已拨出" : "已接听");
    const number = document.createElement("strong");
    number.textContent = call.number || "未知号码";
    nameWrap.append(direction, number);
    const state = document.createElement("p");
    state.textContent = call.missed ? "未接来电" : (call.direction === "outgoing" ? "已拨出" : "已接听");
    const right = document.createElement("div");
    right.className = "sms-actions";
    right.style.flexDirection = "column";
    right.style.alignItems = "flex-end";
    const meta = document.createElement("time");
    meta.textContent = `${new Date(call.started_at).toLocaleString()} · ${formatCallDuration(call)}`;
    const callBack = document.createElement("button");
    callBack.className = "secondary compact";
    callBack.type = "button";
    callBack.textContent = "回拨";
    callBack.addEventListener("click", () => {
      if (call.number) {
        $("#dial-number").value = call.number;
        $("#dial-form").requestSubmit();
      }
    });
    right.append(meta, callBack);
    row.append(nameWrap, state, right);
    return row;
  }));
}

async function loadCalls() {
  if (callPollInFlight) return;
  callPollInFlight = true;
  try {
    const status = await api("/api/calls/status");
    const active = status.active;
    const panel = $("#active-call");
    const pollText = status.polling
      ? `每 ${status.poll_interval_s || 3} 秒检查`
      : "演示模式";
    $("#call-monitor-status").textContent = status.last_poll_error
      ? `${pollText} · ${status.last_poll_error}`
      : `${pollText} · 监听正常`;

    if (active) {
      panel.hidden = false;
      $("#active-call-label").textContent = callStateLabel(active);
      $("#active-call-number").textContent = active.number || "未知号码";
      $("#active-call-time").textContent = new Date(active.started_at).toLocaleString();
      const ringing = ["incoming", "waiting"].includes(active.state);
      const inCall = ["active", "dialing", "alerting", "held"].includes(active.state);
      $("#answer-call").hidden = !ringing;
      $("#reject-call").hidden = !ringing;
      $("#hangup-call").hidden = !(ringing || inCall);
      if (active.id !== lastActiveCallID &&
          active.direction === "incoming" &&
          ["incoming", "waiting"].includes(active.state)) {
        notice(`来电：${active.number || "未知号码"}`);
        startRinging(active.number);
      }
      lastActiveCallID = active.id;
      // DTMF is only meaningful once the far end can hear it.
      setDTMFMode(["active", "held"].includes(active.state));
      if (ringing) startRinging(active.number);
      else if (!ringingStates.includes(active.state)) stopRinging();
    } else {
      panel.hidden = true;
      lastActiveCallID = null;
      setDTMFMode(false);
      stopRinging();
    }
    const audio = status.audio || {};
    $("#call-audio-status").textContent = audio.running
      ? "通话音频：运行中"
      : "通话音频：未运行";
    $("#call-audio-toggle").textContent = audio.running ? "关闭" : "开启";
    $("#call-audio-mute").disabled = !audio.running;
    $("#call-audio-levels").textContent = audio.running
      ? `模块音量 ${Math.round((audio.far_peak || 0) * 100)}% · 麦克风 ${Math.round((audio.near_peak || 0) * 100)}%`
      : "";
    if (audio.error) {
      $("#call-audio-status").textContent += `（${audio.error}）`;
    }
    const source = audio.devices?.near_source;
    if (source && $("#call-audio-source").value !== source) {
      $("#call-audio-source").value = source;
      applySourceVisibility(source);
    }
    renderCallHistory(status.history);
  } catch (error) {
    $("#call-monitor-status").textContent = `监听异常：${error.message}`;
  } finally {
    callPollInFlight = false;
  }
}

function profileRows(value) {
  const groups = Array.isArray(value) ? value : value?.profiles || [];
  return groups.flatMap((group) =>
    (group.profiles || []).map((profile) => ({ ...profile, aid: group.aid_hex || "" })),
  );
}

function profileDisplayName(profile) {
  return profile?.name || profile?.service_provider_name || profile?.iccid || "未命名 Profile";
}

function activeProfile(profiles) {
  return profiles.find((profile) => profile.state === 1) || null;
}

function maskIdentifier(value, keep = 4) {
  const text = String(value || "");
  if (text.length <= keep * 2) return text;
  return `${text.slice(0, keep)} ${"•".repeat(Math.max(4, text.length - keep * 2))} ${text.slice(-keep)}`;
}

function maskPhoneNumber(value) {
  const text = String(value || "").trim();
  const digitCount = [...text].filter((char) => /\d/.test(char)).length;
  if (digitCount <= 8) return text;
  let digitIndex = 0;
  return [...text].map((char) => {
    if (!/\d/.test(char)) return char;
    digitIndex += 1;
    return digitIndex > 4 && digitIndex <= digitCount - 4 ? "*" : char;
  }).join("");
}

async function copyIdentifier(value, label) {
  try {
    await navigator.clipboard.writeText(value);
    notice(`${label} 已复制`);
  } catch (error) {
    notice(`复制 ${label} 失败，请手动复制`);
  }
}

async function editProfileNote(profile, note) {
  const values = await showModal({
    title: "编辑模块资料",
    message: "这些资料保存在大疆模块中，并按 ICCID 与当前 Profile 关联。",
    confirmLabel: "保存",
    fields: [
      { name: "label", label: "模块内名称", value: note.label || "", placeholder: "可选" },
      { name: "phone", label: "模块号码", value: note.phone || "", placeholder: "可选" },
      { name: "tags", label: "用途标签", value: note.tags || "", placeholder: "例如：英国验证码" },
    ],
  });
  if (!values) return;
  try {
    await api("/api/esim/module-notes", {
      method: "PUT",
      body: JSON.stringify({ iccid: profile.iccid, label: values.label, phone: values.phone, tags: values.tags }),
    });
    notice("模块资料已保存");
    await loadESIM();
  } catch (error) {
    notice(error.message);
  }
}

function phonebookCheck(label, ok, detail) {
  const card = document.createElement("div");
  card.className = `phonebook-check ${ok ? "ok" : ""}`;
  const title = document.createElement("strong");
  title.textContent = label;
  const text = document.createElement("small");
  text.textContent = detail;
  card.append(title, text);
  return card;
}

async function probeESIMPhonebook() {
  const button = $("#probe-esim-phonebook");
  const status = $("#esim-phonebook-status");
  const resultPanel = $("#esim-phonebook-result");
  button.disabled = true;
  status.textContent = "正在检测卡内通讯录能力，不会写入联系人...";
  resultPanel.hidden = true;
  try {
    const result = await api("/api/esim/phonebook/probe", { method: "POST" });
    const supported = result.storage_supported && result.storage_selected;
    const portable = supported && result.read_supported && result.write_supported;
    status.textContent = portable
      ? "已确认当前 Profile 支持卡内通讯录读写；尚未写入任何联系人。"
      : "当前 Profile 未完整确认卡内通讯录读写能力；不会进行写入。";
    resultPanel.replaceChildren(
      phonebookCheck("SIM 通讯录", result.storage_supported, result.storage_supported ? "支持 SM 卡内存储" : "未发现 SM 卡内存储"),
      phonebookCheck("当前卡片", result.storage_selected, result.storage_selected ? "已安全选中 SM 存储" : "无法选中 SM 存储"),
      phonebookCheck("读取能力", result.read_supported, result.read_supported ? "模块支持读取卡内联系人" : "模块未确认读取命令"),
      phonebookCheck("写入接口", result.write_supported, result.write_supported ? "模块声明支持写入接口" : "模块未确认写入命令"),
      phonebookCheck("当前状态", supported, result.storage_status || "未返回容量信息"),
    );
    resultPanel.hidden = false;
  } catch (error) {
    status.textContent = `通讯录检测失败：${error.message}`;
  } finally {
    button.disabled = false;
  }
}

function esimEIDRows(value) {
  const eids = value?.chip_info?.eids;
  return Array.isArray(eids) ? eids : [];
}

function renderESIMChip(overview) {
  const panel = $("#esim-chip");
  const chip = overview?.chip_info || {};
  const eids = esimEIDRows(overview);
  if (!chip.sku_name && !chip.serial_number && !chip.firmware && !eids.length) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }
  panel.hidden = false;
  panel.replaceChildren(
    diagnosticCard("卡类型", chip.sku_name || "eUICC/eSIM 卡片"),
    diagnosticCard("固件", chip.firmware || "--", chip.serial_number ? `序列号 ${chip.serial_number}` : ""),
    diagnosticCard("EID", eids.map((item) => item.eid).filter(Boolean).join(" · ") || "--"),
  );
}

function renderESIMEIDList(overview) {
  const eids = esimEIDRows(overview);
  if (!eids.length) return [];
  return eids.map((item) => {
    const row = document.createElement("article");
    row.className = "item esim-info-row";
    const name = document.createElement("strong");
    name.textContent = "已识别 eUICC";
    const detail = document.createElement("p");
    detail.textContent = [
      item.eid ? `EID ${item.eid}` : "",
      item.aid ? `AID ${item.aid}` : "",
      item.free_nvram ? `可用空间 ${item.free_nvram}` : "",
      item.firmware ? `固件 ${item.firmware}` : "",
    ].filter(Boolean).join("\n");
    const status = document.createElement("small");
    status.textContent = item.spec || item.spec_guess || "eSIM";
    row.append(name, detail, status);
    return row;
  });
}

function renderESIMEIDPanel(rows) {
  if (!rows.length) return null;
  const panel = document.createElement("details");
  panel.className = "esim-euicc-panel";
  const heading = document.createElement("summary");
  heading.className = "esim-euicc-heading";
  const title = document.createElement("strong");
  title.textContent = "已识别 eUICC";
  const hint = document.createElement("small");
  hint.textContent = rows.length > 1 ? `${rows.length} 张 eSIM 卡片` : "卡片信息";
  heading.append(title, hint);
  panel.append(heading, ...rows);
  return panel;
}

async function loadESIMHealth() {
  if (esimHealthInFlight) return;
  esimHealthInFlight = true;
  const section = $("#esim-runtime-section");
  const panel = $("#esim-runtime");
  section.hidden = false;
  panel.replaceChildren(diagnosticCard("Profile 检查", "正在检测"));
  try {
    const health = await api("/api/esim/health");
    if (health.card_type === "physical_sim") {
      section.hidden = true;
      return;
    }
    if (!health.active_profile) {
      panel.replaceChildren(diagnosticCard("Profile 检查", health.message || "未发现已启用 Profile"));
      return;
    }
    const profile = health.active_profile;
    const signal = Number.isFinite(health.signal_dbm) ? `${health.signal_dbm} dBm` : "--";
    panel.replaceChildren(
      diagnosticCard("当前启用", profileDisplayName(profile), profile.iccid ? `ICCID ${maskIdentifier(profile.iccid)}` : ""),
      diagnosticCard("模块实际卡", health.module_iccid ? maskIdentifier(health.module_iccid) : "--", health.imsi ? `IMSI ${health.imsi}` : ""),
      diagnosticCard("蜂窝注册", health.registration || "未注册", [displayOperatorName(health.operator), health.network_mode].filter(Boolean).join(" · ")),
      diagnosticCard("信号", signal, health.registered ? "模块已接管当前 Profile" : "等待网络注册"),
    );
  } catch (error) {
    panel.replaceChildren(diagnosticCard("Profile 检查", "暂时无法读取", error.message));
  } finally {
    esimHealthInFlight = false;
  }
}

function setESIMHealthPolling(enabled) {
  clearInterval(esimHealthPollTimer);
  esimHealthPollTimer = null;
  if (!enabled) return;
  esimHealthPollTimer = setInterval(() => {
    if ($("#esim").classList.contains("active")) void loadESIMHealth();
  }, 30000);
}

function diagnosticCard(label, value, detail = "") {
  const card = document.createElement("div");
  card.className = "diagnostic-card";
  const span = document.createElement("span");
  span.textContent = label;
  const strong = document.createElement("strong");
  strong.textContent = value || "--";
  card.append(span, strong);
  if (detail) {
    const small = document.createElement("small");
    small.textContent = detail;
    card.append(small);
  }
  return card;
}

function renderNetworkCheck(label, result) {
  const list = $("#network-checks");
  list.className = "list";
  const row = document.createElement("article");
  row.className = `item check-item ${result.ok ? "ok" : "bad"}`;
  const name = document.createElement("strong");
  name.textContent = label;
  const detail = document.createElement("p");
  detail.textContent = result.detail || result.summary || "";
  const status = document.createElement("small");
  status.textContent = result.ok ? "通过" : "未通过";
  row.append(name, detail, status);
  const existing = [...list.querySelectorAll(".item")].filter((item) => item.dataset.label !== label);
  row.dataset.label = label;
  list.replaceChildren(row, ...existing);
}

async function runNetworkCheck(label, path, button) {
  button.disabled = true;
  try {
    const result = await api(path, { method: "POST" });
    renderNetworkCheck(label, result);
    notice(result.summary || "检测完成");
  } catch (error) {
    renderNetworkCheck(label, { ok: false, summary: "检测失败", detail: error.message });
    notice(error.message);
  } finally {
    button.disabled = false;
  }
}

function renderGPS(state) {
  const grid = $("#gps-result");
  const status = $("#gps-status");
  const toggle = $("#gps-toggle");
  const headerToggle = $("#gps-header-toggle");
  const enabled = Boolean(state?.enabled);
  toggle.textContent = enabled ? "停止定位" : "启动定位";
  toggle.classList.toggle("danger", enabled);
  headerToggle.setAttribute("aria-checked", String(enabled));
  headerToggle.title = enabled ? "GPS 定位已开启，点击停止" : "默认关闭；开启后仅在本机读取定位信息";
  $("#gps-refresh").disabled = !enabled;
  if (!enabled) {
    status.textContent = "定位服务未启动。坐标只会保留在本机当前进程中。";
    grid.hidden = true;
    grid.replaceChildren();
    return;
  }
  const fix = state?.last_fix;
  if (!fix) {
    status.textContent = state?.last_error || "正在搜索卫星，请移至窗边或室外后等待。";
    grid.hidden = true;
    grid.replaceChildren();
    return;
  }
  status.textContent = `定位已更新；后台每 ${state.poll_interval_s || 15} 秒刷新一次。`;
  grid.hidden = false;
  grid.replaceChildren(
    diagnosticCard("纬度", fix.latitude || "未知", "本机显示，不上传"),
    diagnosticCard("经度", fix.longitude || "未知", "本机显示，不上传"),
    diagnosticCard("卫星数", fix.satellites || "未知", "模块本次定位使用的卫星数"),
    diagnosticCard("精度 HDOP", fix.hdop || "未知", "数值越小通常越稳定"),
    diagnosticCard("海拔", fix.altitude || "未知", "模块返回值"),
    diagnosticCard("定位时间", fix.utc || "未知", "模块 UTC 时间"),
  );
}

async function toggleGPS() {
  const pageButton = $("#gps-toggle");
  const headerButton = $("#gps-header-toggle");
  if (pageButton.disabled || headerButton.disabled) return;
  pageButton.disabled = true;
  headerButton.disabled = true;
  try {
    const status = await api("/api/gps");
    if (status.enabled) {
      await api("/api/gps/stop", { method: "POST" });
      notice("定位已停止");
    } else {
      await api("/api/gps/start", { method: "POST" });
      notice("定位已启动，请移至窗边或室外等待");
    }
    await loadGPS();
  } catch (error) {
    notice(error.message);
    await loadGPS();
  } finally {
    pageButton.disabled = false;
    headerButton.disabled = false;
  }
}

async function loadGPS() {
  if (gpsRefreshInFlight) return;
  gpsRefreshInFlight = true;
  try {
    renderGPS(await api("/api/gps"));
  } catch (error) {
    $("#gps-status").textContent = `定位服务不可用：${error.message}`;
  } finally {
    gpsRefreshInFlight = false;
  }
}

function setGPSPolling(enabled) {
  if (gpsRefreshTimer) {
    clearInterval(gpsRefreshTimer);
    gpsRefreshTimer = null;
  }
  if (enabled) gpsRefreshTimer = setInterval(loadGPS, 5000);
}

async function loadNetwork() {
  const grid = $("#network-grid");
  const ifaceList = $("#network-interfaces");
  $("#network-status").textContent = "正在读取网络诊断...";
  try {
    const diag = await api("/api/network");
    const active = Array.isArray(diag.active_contexts) ? diag.active_contexts.join(", ") : "";
    const apns = Array.isArray(diag.pdp_contexts)
      ? diag.pdp_contexts.map((ctx) => `${ctx.id}:${ctx.apn}`).join(" · ")
      : "";
    const addresses = Array.isArray(diag.pdp_addresses) ? diag.pdp_addresses.join(" · ") : "";
    const usb = diag.usb_device
      ? `${diag.usb_device.vendor || ""} ${diag.usb_device.product || ""} (${diag.usb_device.vendor_id}:${diag.usb_device.product_id})`
      : "未检测到";
    const route = diag.default_route || {};
    const routeText = route.interface
      ? `${route.interface}${route.gateway ? ` -> ${route.gateway}` : ""}`
      : "未知";
    grid.replaceChildren(
      diagnosticCard("USB 网卡", diag.usb_network_present ? "已识别" : "未识别", "macOS 是否出现可用 USB 网络接口"),
      diagnosticCard("默认出口", routeText, "当前 macOS 实际优先使用的网卡和网关"),
      diagnosticCard("usbnet", diag.usbnet_mode || "未知", "模块当前 USB 网络模式"),
      diagnosticCard("蜂窝数据", active ? `已激活 ${active}` : "未激活", "PDP context 激活状态"),
      diagnosticCard("蜂窝 IP", addresses || "无", "模块侧拿到的数据网络地址"),
      diagnosticCard("APN", apns || "无", "当前可见 PDP 配置"),
      diagnosticCard("USB 枚举", usb, diag.usb_device?.mode || ""),
    );

    const errorText = diag.errors ? ` · 错误：${Object.values(diag.errors).join("；")}` : "";
    $("#network-status").textContent = diag.usb_network_present
      ? `macOS 已识别 USB 网络接口${errorText}`
      : `蜂窝侧可能已通，但 macOS 尚未识别 USB 网卡${errorText}`;

    const interfaces = Array.isArray(diag.mac_interfaces) ? diag.mac_interfaces : [];
    if (!interfaces.length) {
      ifaceList.className = "list empty";
      ifaceList.textContent = "未读取到网络接口";
      return;
    }
    ifaceList.className = "list";
    ifaceList.replaceChildren(...interfaces.map((item) => {
      const row = document.createElement("article");
      row.className = "item";
      const name = document.createElement("strong");
      name.textContent = item.name;
      const detail = document.createElement("p");
      detail.textContent = [item.kind, item.status, item.ipv4].filter(Boolean).join(" · ");
      const status = document.createElement("small");
      status.textContent = item.status === "active" ? "active" : "inactive";
      row.append(name, detail, status);
      return row;
    }));
  } catch (error) {
    $("#network-status").textContent = `读取网络诊断失败：${error.message}`;
    grid.replaceChildren();
    ifaceList.className = "list empty";
    ifaceList.textContent = "读取失败";
    notice(error.message);
  }
}

function formatTrafficBytes(value) {
  const bytes = Math.max(0, Number(value || 0));
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = bytes;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  const digits = unit === 0 ? 0 : (amount >= 100 ? 0 : amount >= 10 ? 1 : 2);
  return `${amount.toFixed(digits)} ${units[unit]}`;
}

async function loadNetworkTraffic() {
  if (networkTrafficInFlight) return;
  networkTrafficInFlight = true;
  try {
    const sample = await api("/api/network/traffic");
    if (!sample.available) {
      networkTrafficPrevious = null;
      setValue("#traffic-rx-rate", "--", "muted");
      setValue("#traffic-tx-rate", "--", "muted");
      setValue("#traffic-session-rx", "--", "muted");
      setValue("#traffic-session-tx", "--", "muted");
      setValue("#traffic-session-total", "--", "muted");
      return;
    }

    let rxRate = 0;
    let txRate = 0;
    const previous = networkTrafficPrevious;
    if (previous && previous.interface === sample.interface) {
      const elapsed = (Number(sample.sampled_at_ms) - Number(previous.sampled_at_ms)) / 1000;
      if (elapsed > 0) {
        rxRate = Math.max(0, Number(sample.rx_bytes) - Number(previous.rx_bytes)) / elapsed;
        txRate = Math.max(0, Number(sample.tx_bytes) - Number(previous.tx_bytes)) / elapsed;
      }
    }
    networkTrafficPrevious = sample;
    setValue("#traffic-rx-rate", `${formatTrafficBytes(rxRate)}/s`, "neutral");
    setValue("#traffic-tx-rate", `${formatTrafficBytes(txRate)}/s`, "neutral");
    setValue("#traffic-session-rx", formatTrafficBytes(sample.session_rx_bytes), "neutral");
    setValue("#traffic-session-tx", formatTrafficBytes(sample.session_tx_bytes), "neutral");
    setValue("#traffic-session-total", formatTrafficBytes(sample.session_total_bytes), "emphasis");
    $("#traffic-session-total").title = "本次启动期间的下载与上传流量之和；关闭 DJOneHub 后清零";
  } catch (error) {
    setValue("#traffic-rx-rate", "--", "muted");
    setValue("#traffic-tx-rate", "--", "muted");
    setValue("#traffic-session-total", "--", "muted");
  } finally {
    networkTrafficInFlight = false;
  }
}

function setNetworkTrafficPolling(enabled) {
  clearInterval(networkTrafficTimer);
  networkTrafficTimer = null;
  if (!enabled) {
    networkTrafficPrevious = null;
    setValue("#traffic-rx-rate", "--", "muted");
    setValue("#traffic-tx-rate", "--", "muted");
    setValue("#traffic-session-rx", "--", "muted");
    setValue("#traffic-session-tx", "--", "muted");
    setValue("#traffic-session-total", "--", "muted");
    return;
  }
  void loadNetworkTraffic();
  networkTrafficTimer = setInterval(loadNetworkTraffic, 1000);
}

async function loadESIM() {
  const list = $("#esim-list");
  const status = $("#esim-status");
  const download = $("#esim-download-section");
  const runtime = $("#esim-runtime-section");
  const profilePanel = $("#esim-profile-panel");
  const phonebook = $("#esim-phonebook-section");
  $("#esim-chip").hidden = true;
  $("#esim-chip").replaceChildren();
  runtime.hidden = true;
  download.hidden = false;
  profilePanel.hidden = false;
  phonebook.hidden = false;
  list.className = "list empty";
  list.textContent = "正在读取 eUICC";
  status.textContent = "正在通过 AT+CCHO/CGLA 读取 eUICC/eSIM 卡片";
  try {
    const overview = await api("/api/esim");
    if (overview.card_type === "physical_sim") {
      status.textContent = overview.message;
      list.textContent = overview.message;
      download.hidden = true;
      profilePanel.hidden = true;
      phonebook.hidden = true;
      setESIMHealthPolling(false);
      return;
    }
    const notesResponse = await api("/api/esim/module-notes");
    const notes = notesResponse.notes || {};
    const profiles = profileRows(overview);
    const eidRows = renderESIMEIDList(overview);
    const eidPanel = renderESIMEIDPanel(eidRows);
    renderESIMChip(overview);
    const profileCount = profiles.length;
    const eidCount = esimEIDRows(overview).length;
    const active = activeProfile(profiles);
    status.textContent = active
      ? `已读取：${eidCount} 个 eUICC，${profileCount} 个 Profile · 当前使用 ${profileDisplayName(active)}`
      : `已读取：${eidCount} 个 eUICC，${profileCount} 个 Profile · 未发现已启用 Profile`;
    if (!profiles.length) {
      if (eidRows.length) {
        list.className = "list";
        list.replaceChildren(eidPanel);
        return;
      }
      list.textContent = "未发现 eUICC/eSIM 卡片参数";
      return;
    }
    list.className = "list";
    const profileItems = profiles.map((profile) => {
      const note = notes[profile.iccid] || {};
      const row = document.createElement("article");
      row.className = `item esim-profile ${profile.state === 1 ? "active" : ""}`;
      const name = document.createElement("strong");
      name.textContent = note.label || profileDisplayName(profile);
      const detail = document.createElement("p");
      detail.textContent = [
        note.label && note.label !== profileDisplayName(profile) ? `卡内名称：${profileDisplayName(profile)}` : "",
        profile.service_provider_name ? `服务商：${profile.service_provider_name}` : "",
        profile.class_text ? `类型：${profile.class_text}` : "",
        note.tags ? `标签：${note.tags}` : "",
      ].filter(Boolean).join("\n");
      const metadata = document.createElement("div");
      metadata.className = "profile-metadata";
      if (note.phone) {
        const phoneRow = document.createElement("div");
        phoneRow.className = "profile-identifier-row";
        const phone = document.createElement("code");
        phone.className = "profile-iccid";
        phone.textContent = `模块号码 ${maskPhoneNumber(note.phone)}`;
        const revealPhone = document.createElement("button");
        revealPhone.className = "secondary compact profile-toggle-button";
        revealPhone.type = "button";
        revealPhone.textContent = "显示";
        revealPhone.addEventListener("click", () => {
          const hidden = revealPhone.textContent === "显示";
          phone.textContent = `模块号码 ${hidden ? note.phone : maskPhoneNumber(note.phone)}`;
          revealPhone.textContent = hidden ? "隐藏" : "显示";
        });
        const copyPhone = document.createElement("button");
        copyPhone.className = "secondary compact profile-copy-button";
        copyPhone.type = "button";
        copyPhone.textContent = "复制号码";
        copyPhone.addEventListener("click", () => copyIdentifier(note.phone, "模块号码"));
        phoneRow.append(phone, revealPhone, copyPhone);
        metadata.append(phoneRow);
      }
      if (profile.iccid) {
        const iccidRow = document.createElement("div");
        iccidRow.className = "profile-identifier-row";
        const iccid = document.createElement("code");
        iccid.className = "profile-iccid";
        iccid.textContent = `ICCID ${maskIdentifier(profile.iccid)}`;
        const reveal = document.createElement("button");
        reveal.className = "secondary compact profile-toggle-button";
        reveal.type = "button";
        reveal.textContent = "显示";
        reveal.addEventListener("click", () => {
          const hidden = reveal.textContent === "显示";
          iccid.textContent = `ICCID ${hidden ? profile.iccid : maskIdentifier(profile.iccid)}`;
          reveal.textContent = hidden ? "隐藏" : "显示";
        });
        const copy = document.createElement("button");
        copy.className = "secondary compact profile-copy-button";
        copy.type = "button";
        copy.textContent = "复制 ICCID";
        copy.addEventListener("click", () => copyIdentifier(profile.iccid, "ICCID"));
        iccidRow.append(iccid, reveal, copy);
        metadata.append(iccidRow);
      }
      const actionBox = document.createElement("div");
      actionBox.className = "profile-actions";
      if (profile.state !== 1) {
        const button = document.createElement("button");
        button.className = "compact";
        button.textContent = "启用";
        button.addEventListener("click", async () => {
          const label = profileDisplayName(profile);
          const confirmed = await showModal({
            title: "启用 Profile",
            message: `确定启用 ${label} 吗？当前正在使用的 eSIM Profile 会被切换。`,
            confirmLabel: "启用",
          });
          if (!confirmed) {
            return;
          }
          button.disabled = true;
          button.textContent = "切换中";
          try {
            const result = await api("/api/esim/switch", {
              method: "POST",
              body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "" }),
            });
            if (result.module_reboot_requested) {
              status.textContent = `已切换到 ${label}；模块正在重启，等待新 Profile 接管（约 ${result.reconnect_wait_seconds || 10} 秒）`;
              notice(`已切换 ${label}，模块正在重新读取新卡`);
              setTimeout(async () => {
                await loadESIM();
                await loadStatus();
              }, (result.reconnect_wait_seconds || 10) * 1000);
            } else {
              status.textContent = `Profile 已切换到 ${label}，但模块重启未确认：${result.module_reboot_warning || "请手动重启后再读取号码"}`;
              notice("Profile 已切换，模块重启未确认");
              await loadESIM();
            }
          } catch (error) {
            status.textContent = `切换失败：${error.message}`;
            notice(error.message);
            button.disabled = false;
            button.textContent = "启用";
          }
        });
        actionBox.append(button);
      } else {
        const button = document.createElement("button");
        button.className = "secondary compact";
        button.type = "button";
        button.textContent = "启用";
        button.disabled = true;
        actionBox.append(button);
      }
      const rename = document.createElement("button");
      rename.className = "secondary compact";
      rename.type = "button";
      rename.textContent = "改名";
      rename.addEventListener("click", async () => {
        const values = await showModal({
          title: "修改 Profile 名称",
          message: "名称将写入 eUICC 卡片内部的 Profile nickname。",
          confirmLabel: "保存",
          fields: [{ name: "name", label: "Profile 名称", value: profileDisplayName(profile), required: true }],
        });
        if (!values?.name) return;
        rename.disabled = true;
        try {
          await api("/api/esim/profile", { method: "PATCH", body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "", name: values.name }) });
          notice("Profile 名称已修改");
          await loadESIM();
        } catch (error) { notice(error.message); } finally { rename.disabled = false; }
      });
      const localNote = document.createElement("button");
      localNote.className = "secondary compact";
      localNote.type = "button";
      localNote.textContent = "模块资料";
      localNote.addEventListener("click", () => editProfileNote(profile, note));
      const remove = document.createElement("button");
      remove.className = "secondary danger compact";
      remove.type = "button";
      remove.textContent = "删除";
      remove.disabled = profile.state === 1;
      remove.addEventListener("click", async () => {
        const last4 = String(profile.iccid || "").slice(-4);
        const values = await showModal({
          title: "删除 Profile",
          message: `删除不可恢复。请输入 ICCID 后四位 ${last4} 确认。`,
          confirmLabel: "删除",
          danger: true,
          fields: [{ name: "confirmation", label: "ICCID 后四位", required: true }],
        });
        if (!values) return;
        if (values.confirmation !== last4) {
          notice("ICCID 后四位不匹配，未执行删除");
          return;
        }
        remove.disabled = true;
        try {
          await api("/api/esim/profile", { method: "DELETE", body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "" }) });
          notice("Profile 已删除");
          await loadESIM();
        } catch (error) { notice(error.message); } finally { remove.disabled = false; }
      });
      actionBox.append(localNote, rename, remove);
      const description = document.createElement("div");
      description.className = "profile-description";
      description.append(detail, metadata);
      row.append(name, description, actionBox);
      return row;
    });
    list.replaceChildren(...(eidPanel ? [eidPanel] : []), ...profileItems);
    void loadESIMHealth();
    setESIMHealthPolling(true);
  } catch (error) {
    status.textContent = `读取失败：${error.message}`;
    list.textContent = error.message;
    setESIMHealthPolling(false);
  }
}

document.querySelectorAll(".sidebar-item, .tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll(".sidebar-item, .tab, .view").forEach((el) => el.classList.remove("active"));
    tab.classList.add("active");
    $(`#${tab.dataset.view}`).classList.add("active");
    if (tab.dataset.view === "esim") loadESIM();
    else setESIMHealthPolling(false);
    if (tab.dataset.view === "network") loadNetwork();
    if (tab.dataset.view === "gps") {
      void loadGPS();
      setGPSPolling(true);
    } else {
      setGPSPolling(false);
    }
  });
});

// ---------- 实时来电 ----------
//
// The call poll runs every few seconds, which is far too slow to ring a phone.
// The module announces RING and +CLIP the instant they happen and the backend
// forwards them over /api/events, so the browser reacts immediately and the
// poll is left to keep the displayed state honest.

const ringingStates = ["incoming", "waiting"];
let ringing = false;
let lastIncomingNumber = "";
let audioPrimed = false;

// Browsers refuse to start audio a page has not been touched for, so the first
// interaction quietly unlocks the element and asks about notifications.
function primeAlerts() {
  if (audioPrimed || platformCapabilities.os === "linux") return;
  audioPrimed = true;
  const el = $("#ringtone");
  if (!el.getAttribute("src")) el.src = "/api/ringtone";
  el.volume = 0.7;
  el.play().then(() => {
    el.pause();
    el.currentTime = 0;
  }).catch(() => {
    // Nothing to do: playback stays blocked until a real interaction lands.
  });
  if ("Notification" in window && Notification.permission === "default") {
    Notification.requestPermission().catch(() => {});
  }
}
document.addEventListener("pointerdown", primeAlerts, { once: true });
document.addEventListener("keydown", primeAlerts, { once: true });

function startRinging(number) {
  if (number) lastIncomingNumber = number;
  const panel = $("#active-call");
  panel.classList.add("ringing");
  if (ringing) return;
  ringing = true;

  // On Linux the backend rings and raises a GNOME notification itself; doing
  // it here as well would alert twice.
  if (platformCapabilities.os === "linux") return;

  const el = $("#ringtone");
  if (!el.getAttribute("src")) el.src = "/api/ringtone";
  el.currentTime = 0;
  el.play().catch(() => {
    // The page has not been interacted with yet; the banner still shows.
  });

  if ("Notification" in window && Notification.permission === "granted") {
    try {
      new Notification("来电", {
        body: lastIncomingNumber || "未知号码",
        tag: "djonehub-incoming-call",
        renotify: true,
      });
    } catch {
      // Notification support varies; the banner and ringtone are enough.
    }
  }
}

function stopRinging() {
  $("#active-call").classList.remove("ringing");
  if (!ringing) return;
  ringing = false;
  const el = $("#ringtone");
  el.pause();
  el.currentTime = 0;
}

function handleCallEvent(event) {
  switch (event.type) {
    case "ring":
      startRinging(lastIncomingNumber);
      void loadCalls();
      break;
    case "caller":
      lastIncomingNumber = event.number || "";
      startRinging(event.number);
      notice(`来电：${event.number || "未知号码"}`);
      void loadCalls();
      break;
    case "connected":
    case "ended":
      stopRinging();
      void loadCalls();
      break;
    case "state":
      if (ringingStates.includes(event.state)) startRinging(event.number);
      else stopRinging();
      break;
  }
}

function connectCallEvents() {
  if (!("EventSource" in window)) return;
  const stream = new EventSource("/api/events");
  stream.onmessage = (message) => {
    let event;
    try {
      event = JSON.parse(message.data);
    } catch {
      return;
    }
    handleCallEvent(event);
  };
  // EventSource reconnects on its own; nothing to do but stay quiet about it.
  stream.onerror = () => {};
}

// While a call is up the keypad addresses the far end rather than the dial
// field: that is what IVR menus need, and editing a number mid-call is not.
let dtmfMode = false;

function setDTMFMode(on) {
  if (dtmfMode === on) return;
  dtmfMode = on;
  $("#dtmf-row").hidden = !on;
  if (!on) $("#dtmf-sent").textContent = "";
}

async function sendDTMF(digit) {
  try {
    await api("/api/calls/dtmf", { method: "POST", body: JSON.stringify({ digit }) });
    const sent = $("#dtmf-sent");
    sent.textContent = (sent.textContent + digit).slice(-24);
  } catch (error) {
    notice(`按键发送失败：${error.message}`);
  }
}

$("#dial-pad").addEventListener("click", (event) => {
  const key = event.target.closest(".dial-key");
  if (!key) return;
  const input = $("#dial-number");
  const action = key.dataset.action;
  if (dtmfMode && key.dataset.key) {
    void sendDTMF(key.dataset.key);
    return;
  }
  if (action === "backspace") {
    input.value = input.value.slice(0, -1);
  } else if (action === "clear") {
    input.value = "";
  } else if (action === "call") {
    $("#dial-form").requestSubmit();
  } else if (key.dataset.key) {
    input.value += key.dataset.key;
  }
  input.focus();
});

$("#esim-download-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const confirmed = await showModal({
    title: "下载新的 Profile",
    message: "将向 SM-DP+ 服务器下载并写入新的 eSIM Profile。写入期间请勿拔出模块。",
    confirmLabel: "开始下载",
  });
  if (!confirmed) return;
  const button = event.currentTarget.querySelector("button[type=submit]");
  const status = $("#esim-download-status");
  button.disabled = true;
  status.textContent = "正在下载并写入 Profile，请勿拔出模块...";
  try {
    const result = await api("/api/esim/download", { method: "POST", body: JSON.stringify({
      smdp: $("#esim-smdp").value, matching_id: $("#esim-matching-id").value,
      confirmation_code: $("#esim-confirmation-code").value, imei: $("#esim-imei").value, aid: $("#esim-aid").value,
    }) });
    status.textContent = result.message || "Profile 下载完成，正在重新读取卡片";
    notice("Profile 下载完成");
    await loadESIM();
  } catch (error) { status.textContent = `下载失败：${error.message}`; notice(error.message); } finally { button.disabled = false; }
});

$("#send-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "发送中";
  try {
    const result = await api("/api/sms/send", {
      method: "POST",
      body: JSON.stringify({ phone: $("#phone").value, message: $("#message").value }),
    });
    $("#message").value = "";
    const segments = Number(result.segments || 1);
    notice(segments > 1 ? `短信已发送（${segments} 个分片）` : "短信已发送");
  } catch (error) {
    notice(error.message);
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
});

$("#at-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const output = $("#at-output");
  output.textContent = "执行中";
  try {
    const result = await api("/api/at", {
      method: "POST",
      body: JSON.stringify({ command: $("#at-command").value }),
    });
    output.textContent = result.response || "OK";
  } catch (error) {
    output.textContent = error.message;
  }
});

$("#refresh").addEventListener("click", async () => {
  await Promise.all([loadStatus(), loadSMS()]);
  notice("状态已刷新");
});
$("#refresh-sms").addEventListener("click", async () => {
  const button = $("#refresh-sms");
  button.disabled = true;
  $("#sms-status").textContent = "正在读取短信...";
  try {
    const result = await api("/api/sms/refresh", { method: "POST" });
    await loadSMS();
    $("#sms-status").textContent = `短信读取完成：${result.count ?? "未知"} 条`;
    notice("短信读取完成");
  } catch (error) {
    $("#sms-status").textContent = `读取短信失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#clear-module-sms").addEventListener("click", async () => {
  const confirmed = await showModal({
    title: "清空全部短信",
    message: "将删除 SIM 卡和模块存储中的全部短信，且无法恢复。",
    confirmLabel: "确认清空",
    danger: true,
  });
  if (!confirmed) return;
  const button = $("#clear-module-sms");
  button.disabled = true;
  $("#sms-status").textContent = "正在清空 SIM 与模块短信...";
  try {
    const result = await api("/api/sms/clear-module", { method: "POST" });
    $("#sms-status").textContent = `短信已清理：${result.before ?? 0} -> ${result.after ?? 0} 条`;
    await loadSMS();
    notice("短信已清理");
  } catch (error) {
    $("#sms-status").textContent = `清理短信失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#refresh-esim").addEventListener("click", loadESIM);
$("#probe-esim-phonebook").addEventListener("click", probeESIMPhonebook);
$("#refresh-network").addEventListener("click", loadNetwork);
$("#gps-toggle").addEventListener("click", toggleGPS);
$("#gps-header-toggle").addEventListener("click", toggleGPS);
$("#gps-refresh").addEventListener("click", async () => {
  const button = $("#gps-refresh");
  button.disabled = true;
  try {
    const fix = await api("/api/gps/refresh", { method: "POST" });
    renderGPS({ enabled: true, last_fix: fix, poll_interval_s: 15 });
    notice("定位已刷新");
  } catch (error) {
    notice(error.message);
    await loadGPS();
  } finally {
    button.disabled = false;
  }
});
$("#check-4g-route").addEventListener("click", () =>
  runNetworkCheck("4G 出口", "/api/network/check-4g", $("#check-4g-route")));
$("#check-proxy-route").addEventListener("click", () =>
  runNetworkCheck("代理", "/api/network/check-proxy", $("#check-proxy-route")));
$("#reject-call").addEventListener("click", async () => {
  stopRinging();
  const button = $("#reject-call");
  button.disabled = true;
  try {
    await api("/api/calls/reject", { method: "POST" });
    notice("已发送拒接指令");
    await loadCalls();
  } catch (error) {
    notice(`拒接失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});
$("#answer-call").addEventListener("click", async () => {
  stopRinging();
  const button = $("#answer-call");
  button.disabled = true;
  try {
    await api("/api/calls/answer", { method: "POST" });
    notice(platformCapabilities.call_audio ? "已接听，通话音频已启用" : "已接听；Windows 通话音频尚未启用");
    await loadCalls();
  } catch (error) {
    notice(`接听失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});
$("#hangup-call").addEventListener("click", async () => {
  stopRinging();
  const button = $("#hangup-call");
  button.disabled = true;
  try {
    await api("/api/calls/hangup", { method: "POST" });
    notice("已挂断");
    await loadCalls();
  } catch (error) {
    notice(`挂断失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});
$("#dial-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const number = $("#dial-number").value.trim();
  if (!number) return;
  const button = event.target.querySelector("button");
  button.disabled = true;
  try {
    await api("/api/calls/dial", { method: "POST", body: JSON.stringify({ number }) });
    notice(platformCapabilities.call_audio
      ? `正在拨打 ${number}，通话音频已启用`
      : `正在拨打 ${number}；Windows 通话音频尚未启用`);
    $("#dial-number").value = "";
    await loadCalls();
  } catch (error) {
    notice(`拨号失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});
$("#call-audio-toggle").addEventListener("click", async () => {
  const button = $("#call-audio-toggle");
  button.disabled = true;
  try {
    const running = button.textContent === "关闭";
    await api(running ? "/api/calls/audio/stop" : "/api/calls/audio/start", { method: "POST" });
    notice(running ? "通话音频已关闭" : "通话音频已开启");
    await loadCalls();
  } catch (error) {
    notice(`音频切换失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});
// The uplink source is a Windows-only capability; the endpoint does not exist
// elsewhere, so the control only appears once the backend answers for it.
function applySourceVisibility(source) {
  const usesSystem = source === "system" || source === "mix";
  for (const id of ["#call-audio-gain-label", "#call-audio-gain", "#call-audio-gain-value"]) {
    $(id).hidden = !usesSystem;
  }
}

async function pushAudioSource() {
  const source = $("#call-audio-source").value;
  const gain = Number($("#call-audio-gain").value) || 1;
  applySourceVisibility(source);
  $("#call-audio-gain-value").textContent = `${gain}×`;
  try {
    await api("/api/calls/audio/source", {
      method: "POST",
      body: JSON.stringify({ source, gain }),
    });
  } catch (error) {
    notice(`音频来源切换失败：${error.message}`);
  }
}

async function loadAudioSource() {
  try {
    const current = await api("/api/calls/audio/source");
    $("#call-audio-source-row").hidden = false;
    $("#call-audio-source").value = current.source || "mic";
    $("#call-audio-gain").value = Math.round(current.gain || 1);
    $("#call-audio-gain-value").textContent = `${Math.round(current.gain || 1)}×`;
    applySourceVisibility(current.source || "mic");
  } catch {
    // Not supported on this platform: leave the control hidden.
  }
}

$("#call-audio-source").addEventListener("change", () => void pushAudioSource());
$("#call-audio-gain").addEventListener("change", () => void pushAudioSource());
$("#call-audio-gain").addEventListener("input", () => {
  $("#call-audio-gain-value").textContent = `${$("#call-audio-gain").value}×`;
});

$("#call-audio-mute").addEventListener("click", async () => {
  const button = $("#call-audio-mute");
  const muted = button.textContent === "取消静音";
  button.disabled = true;
  try {
    await api("/api/calls/audio/mute", {
      method: "POST",
      body: JSON.stringify({ muted: !muted }),
    });
    button.textContent = muted ? "静音" : "取消静音";
    notice(muted ? "已取消静音" : "已静音（对方听不到你的声音）");
  } catch (error) {
    notice(`静音切换失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
});

loadPlatform();
connectCallEvents();
loadAudioSource();
loadStatus();
loadSMS();
loadCalls();
loadCellularPolicy();
loadGPS();
setNetworkTrafficPolling(true);
setInterval(loadStatus, 10000);
setInterval(loadSMS, 5000);
setInterval(loadCalls, 2000);
setInterval(loadCellularPolicy, 10000);
setInterval(loadGPS, 10000);
