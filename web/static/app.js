// Minimal chat bridge: collect form state, POST /chat, open an
// EventSource on the returned session id, and append streamed deltas
// into the messages pane. No framework.

(function () {
  "use strict";

  const msgs = document.getElementById("messages");
  if (!msgs) return; // not on the chat page

  const provSel = document.getElementById("provider");
  const modelSel = document.getElementById("model");
  const taskSel = document.getElementById("task");
  const memChk = document.getElementById("memory");
  const toolsChk = document.getElementById("tools");
  const promptBox = document.getElementById("prompt");
  const sendBtn = document.getElementById("send");
  const status = document.getElementById("status");

  const providers = JSON.parse(
    document.getElementById("providers-data").textContent || "[]"
  );

  function refreshModels() {
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

  let activeStream = null;
  let activeMsg = null;
  let activeInflight = false;

  function appendMsg(role) {
    const el = document.createElement("div");
    el.className = "msg " + role;
    const label = document.createElement("div");
    label.className = "role";
    label.textContent = role;
    const body = document.createElement("div");
    body.className = "body";
    el.appendChild(label);
    el.appendChild(body);
    msgs.appendChild(el);
    msgs.scrollTop = msgs.scrollHeight;
    return body;
  }

  function addThinking(text) {
    if (!activeMsg) return;
    const el = document.createElement("span");
    el.className = "thinking";
    el.textContent = text;
    activeMsg.appendChild(el);
  }

  function addTool(label, err) {
    if (!activeMsg) return;
    const el = document.createElement("div");
    el.className = "tool" + (err ? " error" : "");
    el.textContent = label;
    activeMsg.appendChild(el);
  }

  function setStatus(text) {
    if (status) status.textContent = text;
  }

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

    appendMsg("user").textContent = prompt;
    activeMsg = appendMsg("assistant");
    promptBox.value = "";
    setStatus("dispatching…");
    activeInflight = true;
    sendBtn.disabled = true;

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
      .then(function (payload) {
        openStream(payload.session);
      })
      .catch(function (err) {
        setStatus("error: " + err.message);
        activeInflight = false;
        sendBtn.disabled = false;
      });
  }

  function openStream(sessionID) {
    setStatus("streaming…");
    activeStream = new EventSource("/chat/stream?session=" + encodeURIComponent(sessionID));

    activeStream.addEventListener("delta", function (e) {
      if (!activeMsg) return;
      activeMsg.appendChild(document.createTextNode(e.data));
      msgs.scrollTop = msgs.scrollHeight;
    });
    activeStream.addEventListener("thinking", function (e) {
      addThinking(e.data);
    });
    activeStream.addEventListener("tool_call", function (e) {
      try {
        const d = JSON.parse(e.data);
        addTool("→ tool " + d.name + " " + (d.args || "{}"));
      } catch (err) {
        addTool(e.data);
      }
    });
    activeStream.addEventListener("tool_result", function (e) {
      try {
        const d = JSON.parse(e.data);
        addTool("← " + (d.content || "").slice(0, 120), d.is_error);
      } catch (err) {
        addTool(e.data);
      }
    });
    activeStream.addEventListener("usage", function (e) {
      try {
        const d = JSON.parse(e.data);
        setStatus(
          d.provider + "/" + d.model + " · " +
          d.input_tokens + "→" + d.output_tokens + " tok · " +
          "$" + Number(d.cost_usd).toFixed(6) + " · " + d.latency_ms + " ms"
        );
      } catch (err) {
        setStatus("usage: " + e.data);
      }
    });
    activeStream.addEventListener("error", function (e) {
      if (e && e.data) setStatus("error: " + e.data);
    });
    activeStream.addEventListener("done", function () {
      if (activeStream) { activeStream.close(); activeStream = null; }
      activeInflight = false;
      sendBtn.disabled = false;
    });
  }

  if (sendBtn) sendBtn.addEventListener("click", send);
  if (promptBox) {
    promptBox.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        send();
      }
    });
  }
})();
