/* Harbor's browser enhancements. Served directly by Go: no packages or build. */
(() => {
  "use strict";
  const $ = (s, root = document) => root.querySelector(s);
  const $$ = (s, root = document) => [...root.querySelectorAll(s)];
  const name = document.body.dataset.project || "";
  const encoder = new TextEncoder();
  let refreshBusy = false;
  const inspection = new WeakMap();
  const inflightForms = new Set();
  let toastTimer;
  function notify(message) {
    const toast = $("#toast");
    toast.textContent = message; toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 6000);
  }

  async function api(fn, input = {}) {
    const path = fn.startsWith("auth/") ? "/api/" + fn : "/api/dashboard/" + fn;
    const response = await fetch(path, {
      method: "POST", credentials: "same-origin", cache: "no-store",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(input),
    });
    if (response.status === 401 && fn !== "auth/login") {
      location.assign("/login");
      throw new Error("Your session expired. Please sign in again.");
    }
    const result = await response.json().catch(() => null);
    if (!response.ok) throw new Error(result?.error || "The request failed. Try again.");
    return result;
  }

  // Parsing never evaluates shell syntax, variable references, or JavaScript.
  function environment(text) {
    if (encoder.encode(text).length > 60000) throw new Error("Environment input is limited to 60 KB.");
    const out = Object.create(null);
    for (const [i, raw] of text.split(/\r?\n/).entries()) {
      const line = raw.trim();
      if (!line || line.startsWith("#")) continue;
      const match = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]{0,127})\s*=\s*(.*)$/.exec(line);
      if (!match) throw new Error(`Line ${i + 1}: expected KEY=value.`);
      if (Object.hasOwn(out, match[1])) throw new Error(`Duplicate key: ${match[1]}.`);
      let value = match[2];
      if (value.startsWith('"') || value.startsWith("'")) {
        const m = value.startsWith('"') ? /^"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$/.exec(value) : /^'([^']*)'\s*(?:#.*)?$/.exec(value);
        if (!m) throw new Error(`Line ${i + 1}: unmatched quote or multiline value.`);
        value = value.startsWith('"') ? m[1].replace(/\\(n|r|t|"|\\)/g, (_, c) => ({ n: "\n", r: "\r", t: "\t", '"': '"', "\\": "\\" })[c]) : m[1];
      } else value = value.replace(/\s+#.*$/, "").trimEnd();
      if (value.includes("\0")) throw new Error("NUL bytes are not allowed.");
      out[match[1]] = value;
      if (Object.keys(out).length > 100) throw new Error("Use at most 100 environment keys.");
    }
    return out;
  }
  function setPath(out, path, value) {
    const keys = path.split(".");
    let obj = out;
    for (const key of keys.slice(0, -1)) {
      if (["__proto__", "prototype", "constructor"].includes(key)) throw new Error("Invalid field.");
      obj = obj[key] ||= Object.create(null);
    }
    const key = keys.at(-1);
    if (["__proto__", "prototype", "constructor"].includes(key)) throw new Error("Invalid field.");
    obj[key] = value;
  }
  function formInput(form) {
    const out = Object.create(null);
    if (form.dataset.action) out.action = form.dataset.action;
    for (const el of $$('[name]', form)) {
      if (el.name.startsWith("_") || el.disabled) continue;
      if (form.id === "live-logs" && ["paused", "follow", "search"].includes(el.name)) continue;
      let value = el.value;
      if (el.dataset.omitEmpty === "true" && !value.trim()) continue;
      if (el.dataset.type === "bool") value = el.checked;
      else if (el.dataset.type === "number") {
        value = Number(value);
        if (!Number.isFinite(value)) throw new Error("Enter a valid number.");
      } else if (el.dataset.type === "list") value = value.split(/[\n,]/).map(s => s.trim()).filter(Boolean);
      else if (el.dataset.type === "env") value = environment(value);
      setPath(out, el.name, value);
    }
    if ($('[name="_clearBuildEnvironment"]', form)?.checked) {
      out.source.buildEnvironment = {};
    }
    if ($('[name="_clearWebhook"]', form)?.checked) out.webhookURL = "";
    if (encoder.encode(JSON.stringify(out)).length > 65536) throw new Error("The request exceeds 64 KiB. Use fewer variables or smaller values.");
    return out;
  }
  function report(form, text, error = false) {
    const target = $(".form-result", form);
    target.textContent = text;
    target.classList.toggle("error", error);
    if (text && !error && text !== "Working…" && text !== "Inspecting repository…") {
      const dismiss = document.createElement("button");
      dismiss.type = "button"; dismiss.className = "secondary"; dismiss.textContent = "Dismiss result";
      dismiss.addEventListener("click", () => { target.replaceChildren(); });
      target.append(dismiss);
    }
  }
  function download(filename, data, type = "application/octet-stream") {
    const url = URL.createObjectURL(new Blob([data], { type }));
    const a = document.createElement("a");
    a.href = url; a.download = filename; a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  function clearSecrets(form) {
    for (const el of $$('input[type="password"], textarea[data-type="env"]', form)) el.value = "";
  }

  // Go re-renders live sections. Forms outside those sections retain drafts.
  // Do not replace a section while one of its action forms has a pending result.
  async function refresh() {
    if (refreshBusy || document.hidden || document.body.dataset.page === "/login") return;
    refreshBusy = true;
    try {
      const response = await fetch(location.href, { credentials: "same-origin", cache: "no-store" });
      if (response.redirected && new URL(response.url).pathname === "/login") { location.assign("/login"); return; }
      if (!response.ok) throw new Error("Status could not be refreshed. Showing the last loaded data.");
      const page = new DOMParser().parseFromString(await response.text(), "text/html");
      const serverError = $('[role="alert"]', page);
      if (serverError) throw new Error(serverError.textContent);
      for (const old of $$("[data-live]")) {
        if ($$("form", old).some(f => inflightForms.has(f) || $(".form-result", f)?.textContent)) continue;
        const next = $$("[data-live]", page).find(s => s.dataset.live === old.dataset.live);
        if (next && !old.isEqualNode(next)) old.replaceWith(next);
      }
      // Update available choices and state without replacing in-progress drafts.
      for (const form of $$("form[data-api]")) {
        const nextForm = page.getElementById(form.getAttribute("id"));
        if (!nextForm || inflightForms.has(form)) continue;
        for (const select of $$("select", form)) {
          const nextSelect = nextForm.elements.namedItem(select.name);
          if (!nextSelect || nextSelect.tagName !== "SELECT") continue;
          const value = select.value;
          select.replaceChildren(...[...nextSelect.options].map(option => option.cloneNode(true)));
          if ([...select.options].some(option => option.value === value)) select.value = value;
        }
        for (const help of $$(".field-help", form)) {
          const field = $("[name]", help.closest("label"));
          const nextField = nextForm.elements.namedItem(field?.name);
          const nextHelp = nextField ? $(".field-help", nextField.closest("label")) : null;
          if (nextHelp) help.textContent = nextHelp.textContent;
        }
        const heading = $("h2", form), nextHeading = $("h2", nextForm);
        if (heading && nextHeading) heading.textContent = nextHeading.textContent;
        form.dataset.success = nextForm.dataset.success || "";
        form.dataset.unavailable = nextForm.dataset.unavailable || "";
        let hint = $("[data-unavailable-hint]", form);
        if (hint) { hint.textContent = form.dataset.unavailable; hint.hidden = !form.dataset.unavailable; }
        const submit = $('button[type="submit"]', form);
        if (submit && form.id !== "import-project" && !form.dataset.confirm) {
          submit.disabled = Boolean(form.dataset.unavailable);
          const nextSubmit = $('button[type="submit"]', nextForm);
          if (nextSubmit) submit.textContent = nextSubmit.textContent;
        }
      }
      const error = $("#refresh-error");
      if (error) { error.hidden = true; error.textContent = ""; }
    } catch (error) {
      const el = $("#refresh-error");
      if (el) { el.hidden = false; el.textContent = error.message; }
    } finally { refreshBusy = false; }
  }

  function inspectInput(input) {
    const out = { name: input.name, repoURL: input.repoURL, ref: input.ref, rootDir: input.rootDir };
    if (input.config) out.config = input.config;
    return out;
  }
  document.addEventListener("click", async event => {
    const refreshButton = event.target.closest("[data-refresh]");
    if (refreshButton) {
      refreshButton.disabled = true; refreshButton.setAttribute("aria-busy", "true");
      try { await refresh(); } finally { refreshButton.disabled = false; refreshButton.removeAttribute("aria-busy"); }
      return;
    }
    const button = event.target.closest("[data-inspect]");
    if (!button) return;
    const form = button.closest("form");
    const submit = $('button[type="submit"]', form);
    button.disabled = true; submit.disabled = true;
    inspection.delete(form);
    try {
      const input = inspectInput(formInput(form));
      if (!input.repoURL) throw new Error("Enter a GitHub repository URL.");
      const snapshot = JSON.stringify(input);
      report(form, "Inspecting repository…");
      const result = await api("inspectProject", input);
      if (snapshot !== JSON.stringify(inspectInput(formInput(form)))) throw new Error("The source changed while inspection was running. Inspect it again.");
      for (const [key, value] of Object.entries(result.config || {})) {
        const el = form.elements.namedItem("config." + key);
        if (el) el.value = value ?? "";
      }
      inspection.set(form, JSON.stringify(inspectInput(formInput(form))));
      submit.disabled = false; submit.hidden = false;
      button.classList.add("secondary"); button.textContent = "Inspect again";
      for (const details of $$('[data-review="true"]', form)) details.open = true;
      $('[data-step="source"]')?.classList.add("done");
      $('[data-step="source"]')?.classList.remove("current");
      $('[data-step="review"]')?.classList.add("current");
      report(form, [result.existingConfig ? "Found paas.yaml." : "Detected project settings.", ...result.warnings || [], "Review the settings, then deploy."].join("\n"));
    } catch (error) {
      report(form, error.message, true);
      // Nonstandard projects may need explicit commands before inspection can succeed.
      for (const details of $$('[data-review="true"]', form)) details.open = true;
    }
    finally { button.disabled = false; }
  });
  document.addEventListener("input", event => {
    const form = event.target.closest("form");
    if (form?.dataset.confirm) {
      $('button[type="submit"]', form).disabled = $('[name="_confirmation"]', form)?.value !== form.dataset.confirm;
    }
    if (form?.id === "import-project" && inspection.has(form)) {
      try { if (inspection.get(form) !== JSON.stringify(inspectInput(formInput(form)))) {
        inspection.delete(form); $('button[type="submit"]', form).disabled = true;
        report(form, "Settings changed. Inspect the repository again before deploying.");
      } } catch { inspection.delete(form); $('button[type="submit"]', form).disabled = true; }
    }
  });
  document.addEventListener("submit", async event => {
    const form = event.target;
    if (!form.matches("form[data-api]")) return;
    event.preventDefault();
    if (inflightForms.has(form)) return;
    const button = $('button[type="submit"]', form);
    try {
      if (form.dataset.confirm && $('[name="_confirmation"]', form)?.value !== form.dataset.confirm) throw new Error("Type the exact project name to confirm deletion.");
      const input = formInput(form);
      if (input.createSqlite && !input.sqliteService) throw new Error("Enter a database service name, or turn off database creation.");
      if (form.id === "import-project" && inspection.get(form) !== JSON.stringify(inspectInput(input))) throw new Error("Inspect the repository before deploying.");
      if (form.id === "live-logs") { await pollLogs(true); return; }
      inflightForms.add(form); button.disabled = true; report(form, "Working…");
      const result = await api(form.dataset.api, input);
      clearSecrets(form);
      if (form.dataset.api === "auth/login" && !result.ok) throw new Error(result.message || "Sign-in failed.");
      if (form.dataset.redirect) { location.assign(form.dataset.redirect); return; }
      if (result.download || result.export) {
        const file = result.download || result.export;
        const bytes = Uint8Array.from(atob(file.data || file.encryptedData), c => c.charCodeAt(0));
        download(file.fileName, bytes, file.contentType); report(form, "Download ready.");
      } else if (form.dataset.action === "bundle") {
        download(`${name}-diagnostics.json`, JSON.stringify(result, null, 2), "application/json"); report(form, "Download ready.");
      } else if (result.secret) {
        report(form, ["Copy this secret now. It will not be shown again.", result.secret, result.githubPath ? "GitHub: " + location.origin + result.githubPath : "", result.deployPath ? "CI: " + location.origin + result.deployPath : ""].filter(Boolean).join("\n"));
      } else if (result.explanation) {
        report(form, [result.explanation, ...result.evidence || [], "Next step: " + result.nextStep].join("\n\n"));
      } else if (form.dataset.api === "listGitHubRepos") {
        showRepositories(form, result);
      } else if (result.id && result.state && ["submitOnboardingJob", "retryGitHubDeployment"].includes(form.dataset.api)) {
        location.assign("/projects/" + encodeURIComponent(result.name) + "?tab=automation");
      } else if (result.job) {
        report(form, [result.job.state, result.job.stage, result.job.error, ...(result.job.logs || [])].filter(Boolean).join("\n"));
      } else if (form.dataset.api === "getOnboardingJob") {
        report(form, [result.state, result.stage, result.error, ...(result.logs || [])].filter(Boolean).join("\n"));
      } else if (result.preflight) {
        report(form, [(result.preflight.ready ? "Ready for deployment." : "Review before deploying."), result.preflight.capacity?.message, result.preflight.missingRequiredEnvironmentKeys?.length ? "Missing: " + result.preflight.missingRequiredEnvironmentKeys.join(", ") : ""].filter(Boolean).join("\n"));
      } else {
        report(form, "");
        notify(result.restartRequired ? "Saved. Restart or deploy the project to apply the database binding." : (form.dataset.success || (form.dataset.api === "disconnectGitHub" ? "GitHub disconnected." : "Changes saved.")));
      }
      for (const el of $$('[name="removeEnvironment"]', form)) el.value = "";
      for (const el of $$('[name="_clearBuildEnvironment"], [name="_clearWebhook"]', form)) el.checked = false;
      // Keep every other form draft intact. Refresh also updates select choices.
      inflightForms.delete(form);
      await refresh();
    } catch (error) { report(form, error.message || "The request failed.", true); }
    finally {
      inflightForms.delete(form);
      if (button) button.disabled = Boolean(form.dataset.unavailable) || (form.id === "import-project" && !inspection.has(form)) || (Boolean(form.dataset.confirm) && $('[name="_confirmation"]', form)?.value !== form.dataset.confirm);
    }
  });

  function showRepositories(form, repos) {
    const target = $(".form-result", form); target.replaceChildren();
    if (!repos.length) { target.textContent = "No repositories found."; return; }
    for (const repo of repos) {
      const button = document.createElement("button"); button.type = "button"; button.className = "secondary";
      button.textContent = repo.fullName + (repo.private ? " · private" : "");
      button.addEventListener("click", () => {
        const importer = $("#import-project");
        importer.elements.namedItem("repoURL").value = repo.url;
        importer.elements.namedItem("ref").value = repo.defaultBranch;
        importer.elements.namedItem("name").value = repo.name.toLowerCase().replace(/[^a-z0-9-]/g, "-").replace(/^-+|-+$/g, "").slice(0, 63);
        inspection.delete(importer); $('button[type="submit"]', importer).disabled = true;
        importer.scrollIntoView({ behavior: "smooth" });
      });
      const p = document.createElement("p"); p.append(button); target.append(p);
    }
  }

  let logBusy = false, cursor = "", logIdentity = "", logText = "", generation = 0;
  function drawLogs(form) {
    let pre = $("pre.logs", form);
    if (!pre) {
      pre = document.createElement("pre"); pre.className = "logs"; pre.tabIndex = 0; form.append(pre);
      const button = document.createElement("button"); button.type = "button"; button.className = "secondary"; button.textContent = "Download logs";
      button.addEventListener("click", () => download(name + "-logs.txt", logText, "text/plain")); form.append(button);
    }
    const search = form.elements.namedItem("search").value;
    pre.replaceChildren();
    if (!search) pre.textContent = logText;
    else {
      let position = 0, count = 0;
      const lower = logText.toLowerCase(), query = search.toLowerCase();
      let next;
      while (count++ < 2000 && (next = lower.indexOf(query, position)) !== -1) {
        pre.append(document.createTextNode(logText.slice(position, next)));
        const mark = document.createElement("mark"); mark.textContent = logText.slice(next, next + search.length); pre.append(mark);
        position = next + search.length;
      }
      pre.append(document.createTextNode(logText.slice(position)));
    }
    if (form.elements.namedItem("follow").checked) pre.scrollTop = pre.scrollHeight;
  }
  async function pollLogs(force = false) {
    const form = $("#live-logs");
    if (!form || form.dataset.unavailable || logBusy || document.hidden || (!force && form.elements.namedItem("paused").checked)) return;
    const input = formInput(form);
    const identity = JSON.stringify(input);
    if (identity !== logIdentity) { logIdentity = identity; logText = ""; cursor = ""; }
    const requestGeneration = generation;
    logBusy = true;
    try {
      const result = await api("observe", { ...input, cursor });
      if (requestGeneration !== generation) return;
      cursor = result.cursor;
      logText = ((result.reset ? "" : logText) + result.text).slice(-1048576);
      report(form, ""); drawLogs(form);
    } catch (error) { report(form, error.message, true); }
    finally { logBusy = false; }
  }
  $("#live-logs")?.addEventListener("input", event => {
    if (["deploymentId", "source"].includes(event.target.name)) { generation++; cursor = ""; logIdentity = ""; logText = ""; }
    drawLogs(event.currentTarget);
    if (!["search", "follow"].includes(event.target.name)) pollLogs();
  });
  const query = new URLSearchParams(location.search);
  const filter = $("form[data-filter]");
  if (filter) for (const el of $$('[name]', filter)) { if (el.type === "checkbox") el.checked = query.get(el.name) === "1"; else el.value = query.get(el.name) || ""; }
  setInterval(refresh, 5000);
  setInterval(pollLogs, 2000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) { refresh(); pollLogs(); } });
  pollLogs();
})();
