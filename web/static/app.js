// hippo web UI — small vanilla-JS layer for interactions that don't
// fit cleanly into server-rendered HTML: chat streaming, sticky-save
// dirty tracking, flash auto-dismiss, API-key show/hide, policy line
// numbering. No framework.

(function () {
  "use strict";

  // ── flash auto-dismiss ──────────────────────────────────────────
  document.querySelectorAll("[data-flash]").forEach(function (el) {
    setTimeout(function () {
      el.style.opacity = "0";
      el.style.transform = "translateY(8px)";
      setTimeout(function () { el.remove(); }, 260);
    }, 4500);
  });

  // ── sticky save bar dirty tracking ──────────────────────────────
  document.querySelectorAll("[data-dirty-form]").forEach(function (form) {
    const save = form.querySelector("[data-sticky-save]");
    if (!save) return;
    const snapshot = serializeForm(form);
    function refresh() {
      const dirty = serializeForm(form) !== snapshot;
      if (dirty) {
        save.hidden = false;
        save.classList.add("enter");
      } else {
        save.hidden = true;
      }
    }
    form.addEventListener("input", refresh);
    form.addEventListener("change", refresh);
  });
  function serializeForm(f) {
    const fd = new FormData(f);
    const pairs = [];
    fd.forEach(function (v, k) { pairs.push(k + "=" + (typeof v === "string" ? v : "")); });
    pairs.sort();
    return pairs.join("&");
  }

  // ── API key show/hide toggles ───────────────────────────────────
  document.querySelectorAll("[data-toggle-visibility]").forEach(function (btn) {
    const targetSel = btn.getAttribute("data-toggle-visibility");
    const input = document.querySelector(targetSel);
    if (!input) return;
    btn.addEventListener("click", function () {
      const showing = input.type === "text";
      input.type = showing ? "password" : "text";
      btn.setAttribute("aria-pressed", String(!showing));
      const eye = btn.querySelector(".eye-on");
      const eyeOff = btn.querySelector(".eye-off");
      if (eye && eyeOff) {
        eye.style.display    = showing ? "" : "none";
        eyeOff.style.display = showing ? "none" : "";
      }
    });
  });

  // ── policy editor line numbers ──────────────────────────────────
  const editor = document.querySelector(".policy-editor");
  if (editor) {
    const ta = editor.querySelector("textarea");
    const gutter = editor.querySelector(".linenums");
    function renderLines() {
      if (!ta || !gutter) return;
      const lines = ta.value.split("\n").length;
      const out = [];
      for (let i = 1; i <= lines; i++) out.push('<span class="ln">' + i + "</span>");
      gutter.innerHTML = out.join("");
    }
    if (ta) {
      ta.addEventListener("input", renderLines);
      ta.addEventListener("scroll", function () {
        gutter.style.transform = "translateY(" + -ta.scrollTop + "px)";
      });
      renderLines();
    }
  }

  // ── chat ────────────────────────────────────────────────────────
  const msgs = document.getElementById("messages");
  if (!msgs) return;

  const provSel = document.getElementById("provider");
  const modelSel = document.getElementById("model");
  const taskSel = document.getElementById("task");
  const memChk = document.getElementById("memory");
  const toolsChk = document.getElementById("tools");
  const promptBox = document.getElementById("prompt");
  const sendBtn = document.getElementById("send");
  const statusEl = document.getElementById("status");
  const newChatBtn = document.getElementById("new-chat");
  const emptyState = document.getElementById("chat-empty");

  const providersNode = document.getElementById("providers-data");
  const providers = providersNode
    ? JSON.parse(providersNode.textContent || "[]")
    : [];

  function refreshModels() {
    if (!provSel || !modelSel) return;
    const p = providers.find(function (x) { return x.name === provSel.value; });
    if (!p) return;
    modelSel.innerHTML = "";
    p.models.forEach(function (m) {
      const opt = document.createElement("option");
      opt.value = m;
      opt.textContent = m;
      if (m === p.default) opt.selected = true;
      modelSel.appendChild(opt);
    });
  }
  if (provSel) {
    provSel.addEventListener("change", refreshModels);
    refreshModels();
  }

  function updateSendEnabled() {
    if (!sendBtn || !promptBox) return;
    const has = promptBox.value.trim().length > 0;
    sendBtn.disabled = !has || activeInflight;
    sendBtn.classList.toggle("ready", has && !activeInflight);
  }
  if (promptBox) {
    promptBox.addEventListener("input", updateSendEnabled);
    updateSendEnabled();
  }

  document.querySelectorAll(".suggestion").forEach(function (b) {
    b.addEventListener("click", function () {
      if (!promptBox) return;
      promptBox.value = b.getAttribute("data-suggestion") || b.textContent.trim();
      promptBox.focus();
      updateSendEnabled();
    });
  });

  if (newChatBtn) {
    newChatBtn.addEventListener("click", function () {
      const empty = emptyState;
      msgs.innerHTML = "";
      if (empty) msgs.appendChild(empty);
      if (statusEl) statusEl.textContent = "";
      if (promptBox) { promptBox.value = ""; updateSendEnabled(); promptBox.focus(); }
    });
  }

  let activeStream = null;
  let activeBody = null;
  let activeMeta = null;
  let activeInflight = false;

  function hideEmpty() {
    if (emptyState && emptyState.parentNode) emptyState.remove();
  }

  function makeAvatar() {
    const w = document.createElement("div");
    w.className = "avatar";
    w.innerHTML =
      '<svg width="22" height="22" viewBox="0 0 32 32" aria-hidden="true">' +
      '<ellipse cx="8" cy="9" rx="3" ry="3.5" fill="var(--hippo-cyan)" stroke="var(--hippo-ink)" stroke-width="1.2"/>' +
      '<ellipse cx="24" cy="9" rx="3" ry="3.5" fill="var(--hippo-cyan)" stroke="var(--hippo-ink)" stroke-width="1.2"/>' +
      '<ellipse cx="8" cy="9.5" rx="1.2" ry="1.6" fill="var(--hippo-pink)"/>' +
      '<ellipse cx="24" cy="9.5" rx="1.2" ry="1.6" fill="var(--hippo-pink)"/>' +
      '<path d="M5 17 C5 11, 10 7, 16 7 C22 7, 27 11, 27 17 C27 22, 23 26, 16 26 C9 26, 5 22, 5 17 Z" fill="var(--hippo-cyan)" stroke="var(--hippo-ink)" stroke-width="1.2"/>' +
      '<ellipse cx="16" cy="20" rx="8" ry="5" fill="var(--hippo-cyan-soft)" stroke="var(--hippo-ink)" stroke-width="1.2"/>' +
      '<ellipse cx="13" cy="19" rx="0.8" ry="1" fill="var(--hippo-ink)" opacity="0.55"/>' +
      '<ellipse cx="19" cy="19" rx="0.8" ry="1" fill="var(--hippo-ink)" opacity="0.55"/>' +
      '<path d="M14 22 Q16 23.5 18 22" fill="none" stroke="var(--hippo-ink)" stroke-width="1" stroke-linecap="round"/>' +
      '<circle cx="11.5" cy="13" r="1.4" fill="var(--hippo-ink)"/>' +
      '<circle cx="20.5" cy="13" r="1.4" fill="var(--hippo-ink)"/>' +
      "</svg>";
    return w;
  }

  function appendMsg(role, text) {
    hideEmpty();
    const wrap = document.createElement("div");
    wrap.className = "msg " + role;
    if (role === "assistant") wrap.appendChild(makeAvatar());
    const bodyWrap = document.createElement("div");
    bodyWrap.className = "body-wrap";
    const body = document.createElement("div");
    body.className = "body";
    if (text) body.textContent = text;
    bodyWrap.appendChild(body);
    const meta = document.createElement("div");
    meta.className = "meta";
    bodyWrap.appendChild(meta);
    wrap.appendChild(bodyWrap);
    msgs.appendChild(wrap);
    msgs.scrollTop = msgs.scrollHeight;
    return { body: body, meta: meta };
  }

  function addThinking(text) {
    if (!activeBody) return;
    const el = document.createElement("span");
    el.className = "thinking";
    el.textContent = text;
    activeBody.appendChild(el);
    msgs.scrollTop = msgs.scrollHeight;
  }

  function addTool(label, err) {
    if (!activeBody) return;
    const el = document.createElement("div");
    el.className = "tool" + (err ? " error" : "");
    el.textContent = label;
    activeBody.appendChild(el);
    msgs.scrollTop = msgs.scrollHeight;
  }

  function setStatus(text) { if (statusEl) statusEl.textContent = text; }

  function beforeUnload(e) {
    if (activeInflight) {
      e.preventDefault();
      e.returnValue = "A response is still streaming. Leave anyway?";
      return e.returnValue;
    }
  }
  window.addEventListener("beforeunload", beforeUnload);

  function send() {
    if (activeInflight) return;
    const prompt = promptBox.value.trim();
    if (!prompt) return;

    appendMsg("user", prompt);
    const pair = appendMsg("assistant", "");
    activeBody = pair.body;
    activeMeta = pair.meta;

    const pulse = document.createElement("span");
    pulse.className = "dot-pulse";
    pulse.innerHTML = "<span></span><span></span><span></span>";
    activeBody.appendChild(pulse);

    promptBox.value = "";
    setStatus("dispatching…");
    activeInflight = true;
    updateSendEnabled();

    const body = new URLSearchParams({
      prompt: prompt,
      provider: provSel ? provSel.value : "",
      model: modelSel ? modelSel.value : "",
      task: taskSel ? taskSel.value : "generate",
      memory: memChk && memChk.checked ? "on" : "",
      tools: toolsChk && toolsChk.checked ? "on" : "",
    });

    fetch("/chat", { method: "POST", body: body })
      .then(function (r) {
        if (!r.ok) return r.text().then(function (t) { throw new Error(t || "POST failed"); });
        return r.json();
      })
      .then(function (payload) { openStream(payload.session); })
      .catch(function (err) {
        setStatus("error: " + err.message);
        activeInflight = false;
        updateSendEnabled();
      });
  }

  function openStream(sessionID) {
    setStatus("streaming…");
    activeStream = new EventSource("/chat/stream?session=" + encodeURIComponent(sessionID));
    let firstDelta = true;

    activeStream.addEventListener("delta", function (e) {
      if (!activeBody) return;
      if (firstDelta) {
        // Drop the placeholder dot-pulse once real content arrives.
        const pulse = activeBody.querySelector(".dot-pulse");
        if (pulse) pulse.remove();
        firstDelta = false;
      }
      activeBody.appendChild(document.createTextNode(e.data));
      msgs.scrollTop = msgs.scrollHeight;
    });
    activeStream.addEventListener("thinking", function (e) { addThinking(e.data); });
    activeStream.addEventListener("tool_call", function (e) {
      try {
        const d = JSON.parse(e.data);
        addTool("→ " + d.name + " " + (d.args || "{}"));
      } catch (_) { addTool(e.data); }
    });
    activeStream.addEventListener("tool_result", function (e) {
      try {
        const d = JSON.parse(e.data);
        addTool("← " + (d.content || "").slice(0, 120), d.is_error);
      } catch (_) { addTool(e.data); }
    });
    activeStream.addEventListener("usage", function (e) {
      try {
        const d = JSON.parse(e.data);
        const parts = [
          d.provider + "/" + d.model,
          d.input_tokens + "→" + d.output_tokens + " tok",
          "$" + Number(d.cost_usd).toFixed(6),
          d.latency_ms + " ms",
        ];
        if (activeMeta) activeMeta.textContent = parts.join(" · ");
        setStatus(parts.join(" · "));
      } catch (_) {
        setStatus("usage: " + e.data);
      }
    });
    activeStream.addEventListener("error", function (e) {
      if (e && e.data) setStatus("error: " + e.data);
    });
    activeStream.addEventListener("done", function () {
      if (activeStream) { activeStream.close(); activeStream = null; }
      activeInflight = false;
      updateSendEnabled();
    });
  }

  if (sendBtn) sendBtn.addEventListener("click", send);
  if (promptBox) {
    promptBox.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); send(); return; }
      if (e.key === "Enter" && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
        e.preventDefault(); send();
      }
    });
  }
})();
