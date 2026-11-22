"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const state = {
    session: null,
    accounts: [],
    connection: null,
    flow: null,
    deleting: null,
    page: "accounts",
    loading: false,
    videos: [],
  };
  const statusLabels = {
    ready: "可用",
    expired: "待刷新",
    error: "异常",
    disabled: "已停用",
  };
  let toastTimer;
  let videoPollTimer;
  let videoLoadVersion = 0;
  function errorAt(id, message) {
    $(id).textContent = message || "";
    $(id).hidden = !message;
  }
  function toast(message, error = false) {
    clearTimeout(toastTimer);
    $("toast").textContent = message;
    $("toast").classList.toggle("error", error);
    $("toast").hidden = false;
    toastTimer = setTimeout(() => {
      $("toast").hidden = true;
    }, 3500);
  }
  function busy(button, active, text) {
    if (active) {
      button.dataset.originalText = button.innerHTML;
      button.disabled = true;
      button.setAttribute("aria-busy", "true");
      button.classList.add("busy");
      if (text) button.textContent = text;
    } else {
      button.disabled = false;
      button.removeAttribute("aria-busy");
      button.classList.remove("busy");
      if (button.dataset.originalText) {
        button.innerHTML = button.dataset.originalText;
        delete button.dataset.originalText;
      }
    }
  }
  async function api(path, { method = "GET", body } = {}) {
    const headers = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (method !== "GET" && state.session)
      headers["X-CSRF-Token"] = state.session.csrf_token;
    const response = await fetch("/admin/api" + path, {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: "no-store",
    });
    const result = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (response.status === 401 && path !== "/login" && path !== "/session")
        showLogin();
      throw new Error(
        result.error?.message ||
          result.error ||
          `请求失败（${response.status}）`,
      );
    }
    return result;
  }
  function showLogin() {
    state.session = null;
    state.accounts = [];
    state.connection = null;
    state.flow = null;
    state.videos = [];
    clearTimeout(videoPollTimer);
    videoLoadVersion++;
    $("video-jobs").replaceChildren();
    $("video-prompt").value = "";
    $("video-image").value = "";
    $("reference-image").removeAttribute("src");
    $("reference-preview").hidden = true;
    $("loading-screen").hidden = true;
    $("console").hidden = true;
    $("login-screen").hidden = false;
    $("password").value = "";
    $("api-key").value = "";
    $("api-key").type = "password";
    if ($("add-dialog").open) $("add-dialog").close();
    if ($("delete-dialog").open) $("delete-dialog").close();
    $("account-rows").replaceChildren();
  }
  function showConsole(session) {
    state.session = session;
    $("loading-screen").hidden = true;
    $("login-screen").hidden = true;
    $("console").hidden = false;
    $("admin-label").textContent =
      session.username === "admin" ? "管理员" : session.username;
    errorAt("login-error", "");
    navigate(
      location.hash === "#videos"
        ? "videos"
        : location.hash === "#connection"
          ? "connection"
          : "accounts",
    );
  }
  function escape(value) {
    return String(value ?? "").replace(
      /[&<>"']/g,
      (c) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[c],
    );
  }
  function date(epoch) {
    return epoch
      ? new Intl.DateTimeFormat("zh-CN", {
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        }).format(new Date(epoch * 1000))
      : "—";
  }
  function renderAccounts() {
    $("count-total").textContent = state.accounts.length;
    $("count-ready").textContent = state.accounts.filter(
      (a) => a.status === "ready",
    ).length;
    $("count-attention").textContent = state.accounts.filter(
      (a) => a.status === "error" || a.status === "expired",
    ).length;
    const query = $("account-search").value.trim().toLowerCase();
    const filter = $("status-filter").value;
    const list = state.accounts.filter(
      (a) =>
        (!query ||
          `${a.name} ${a.email} ${a.id}`.toLowerCase().includes(query)) &&
        (filter === "all" || a.status === filter),
    );
    $("account-rows").innerHTML = list
      .map((a) => {
        const name = a.email || a.name || `账号 ${a.id}`;
        const provider =
          a.provider === "social"
            ? "社交账号"
            : a.provider === "enterprise"
              ? "企业 SSO"
              : a.provider;
        return `<tr><td><div class="account-name"><span class="avatar" aria-hidden="true">${escape(name.slice(0, 1).toUpperCase())}</span><div><span class="account-email" title="${escape(name)}">${escape(name)}</span><span class="account-secondary">${escape(a.name || `账号 #${a.id}`)}</span></div></div></td><td>${escape(provider)}<span class="account-secondary">${escape(a.region)}</span></td><td><span class="status ${escape(a.status)}">${escape(statusLabels[a.status] || a.status)}</span>${a.last_error ? `<p class="account-error" title="${escape(a.last_error)}">${escape(a.last_error)}</p>` : ""}</td><td>${date(a.expires_at)}<span class="account-secondary">${a.expires_at && a.expires_at < Date.now() / 1000 ? "已过期" : "登录状态"}</span></td><td>${escape(a.use_count.toLocaleString("zh-CN"))}<span class="account-secondary">最近 ${date(a.last_used_at)}</span></td><td><div class="row-actions"><button class="row-action refresh" type="button" data-action="refresh" data-id="${a.id}" aria-label="刷新 ${escape(name)}">刷新</button><button class="row-action" type="button" data-action="toggle" data-id="${a.id}" aria-label="${a.disabled ? "启用" : "停用"} ${escape(name)}">${a.disabled ? "启用" : "停用"}</button><button class="row-action delete" type="button" data-action="delete" data-id="${a.id}" aria-label="删除 ${escape(name)}">删除</button></div></td></tr>`;
      })
      .join("");
    const filtered = state.accounts.length > 0;
    $("empty-state").hidden = list.length > 0;
    $("empty-title").textContent = filtered ? "没有匹配的账号" : "暂无账号";
    $("empty-description").textContent = filtered
      ? "试试其他关键词，或切换账号状态。"
      : "添加第一个 Amazon Quick 账号，开始使用 API。";
    $("empty-add").hidden = filtered;
  }
  async function loadAccounts() {
    if (state.loading) return;
    state.loading = true;
    busy($("refresh-list"), true);
    if (!state.accounts.length) $("table-loading").hidden = false;
    try {
      const data = await api("/accounts");
      state.accounts = data.accounts;
      renderAccounts();
      errorAt("page-error-text", "");
      $("page-error").hidden = true;
    } catch (error) {
      $("page-error-text").textContent = error.message;
      $("page-error").hidden = false;
    } finally {
      state.loading = false;
      busy($("refresh-list"), false);
      $("table-loading").hidden = true;
    }
  }
  async function loadConnection() {
    try {
      state.connection = await api("/connection");
      $("base-url").textContent = state.connection.base_url;
      $("anthropic-base").textContent = state.connection.anthropic_base_url;
      $("model-list").replaceChildren(
        ...state.connection.models.map((model) => {
          const el = document.createElement("span");
          el.textContent = model;
          return el;
        }),
      );
      $("api-key").value = state.connection.api_keys[0] || "";
      $("video-model").replaceChildren(
        ...state.connection.models.map((model) => {
          const option = document.createElement("option");
          option.value = model;
          option.textContent = model;
          return option;
        }),
      );
    } catch (error) {
      toast(error.message, true);
    }
  }
  function navigate(page) {
    videoLoadVersion++;
    state.page = page;
    $("accounts-section").hidden = page !== "accounts";
    $("header-actions").hidden = page !== "accounts";
    $("key-details").hidden = page !== "connection";
    $("videos-section").hidden = page !== "videos";
    $("connection-panel").hidden = page === "videos";
    $("page-title").textContent =
      page === "accounts"
        ? "账号管理"
        : page === "videos"
          ? "视频生成"
          : "接入信息";
    $("page-description").textContent =
      page === "accounts"
        ? "管理 Amazon Quick 授权，账号自动轮询使用。"
        : page === "videos"
          ? "通过 Quick 聊天制作视频，查看任务进度与成片。"
          : "在 OpenAI 兼容客户端中使用此服务。";
    history.replaceState(null, "", "#" + page);
    clearTimeout(videoPollTimer);
    if (page === "videos") loadVideos();
    for (const name of ["accounts", "connection", "videos"]) {
      $("nav-" + name).classList.toggle("selected", name === page);
      if (name === page) $("nav-" + name).setAttribute("aria-current", "page");
      else $("nav-" + name).removeAttribute("aria-current");
    }
  }
  function renderVideos() {
    const root = $("video-jobs");
    const existing = new Map(
      Array.from(root.children, (card) => [card.dataset.jobId, card]),
    );
    const labels = {
      queued: "等待开始",
      in_progress: "正在生成",
      completed: "已完成",
      failed: "生成失败",
    };
    $("video-history-status").textContent = state.videos.length
      ? `${state.videos.length} 条记录 · 生成中自动更新`
      : "暂无生成记录，提交描述后会显示在这里。";
    const cards = state.videos.map((job) => {
      const renderKey = JSON.stringify([job.status, job.error, job.file_id]);
      const previous = existing.get(job.id);
      if (previous?.dataset.renderKey === renderKey) {
        const link = previous.querySelector(".video-download");
        if (link && job.url) link.href = job.url;
        const player = previous.querySelector("video");
        if (
          player &&
          job.url &&
          Number(new URL(player.src).searchParams.get("expires")) <
            Date.now() / 1000 + 60
        )
          player.src = job.url;
        const details = previous.querySelector(".video-result");
        if (details && details.textContent !== job.result)
          details.textContent = job.result;
        return previous;
      }
      const card = document.createElement("article");
      card.className = "video-card";
      card.dataset.jobId = job.id;
      card.dataset.renderKey = renderKey;
      const header = document.createElement("div");
      header.className = "video-card-heading";
      const status = document.createElement("span");
      status.className =
        "status " +
        (job.status === "completed"
          ? "ready"
          : job.status === "failed"
            ? "error"
            : "expired");
      status.textContent = labels[job.status] || job.status;
      const time = document.createElement("span");
      time.className = "form-hint";
      time.textContent = date(job.created_at);
      header.append(status, time);
      const prompt = document.createElement("p");
      prompt.className = "video-job-prompt";
      prompt.textContent = job.prompt;
      const meta = document.createElement("p");
      meta.className = "form-hint";
      meta.textContent = `${job.model} · ${job.seconds} 秒 · ${job.size}`;
      card.append(header, prompt, meta);
      if (job.status === "completed" && job.url) {
        const player = document.createElement("video");
        player.controls = true;
        player.preload = "metadata";
        player.playsInline = true;
        player.src = job.url;
        player.setAttribute("aria-label", "生成的视频");
        card.append(player);
        const link = document.createElement("a");
        link.className = "button small video-download";
        link.href = job.url;
        link.download = job.id + ".mp4";
        link.textContent = "下载视频";
        card.append(link);
      }
      if (job.error) {
        const error = document.createElement("p");
        error.className = "form-error";
        error.textContent = job.error;
        card.append(error);
      }
      if (job.result) {
        const details = document.createElement("details");
        const summary = document.createElement("summary");
        summary.textContent = "Quick 生成说明";
        const text = document.createElement("p");
        text.className = "video-result";
        text.textContent = job.result;
        details.append(summary, text);
        card.append(details);
      }
      return card;
    });
    cards.forEach((card, index) => {
      const current = root.children[index];
      if (current !== card) {
        if (current?.dataset.jobId === card.dataset.jobId)
          root.replaceChild(card, current);
        else root.insertBefore(card, current || null);
      }
    });
    while (root.children.length > cards.length) root.lastElementChild.remove();
  }
  async function loadVideos() {
    const version = ++videoLoadVersion;
    clearTimeout(videoPollTimer);
    if (!state.session || state.page !== "videos") return;
    try {
      const result = await api("/videos");
      if (
        version !== videoLoadVersion ||
        !state.session ||
        state.page !== "videos"
      )
        return;
      state.videos = result.data;
      renderVideos();
    } catch (error) {
      $("video-history-status").textContent = error.message;
    } finally {
      if (
        version === videoLoadVersion &&
        state.session &&
        state.page === "videos"
      )
        videoPollTimer = setTimeout(loadVideos, 5000);
    }
  }
  function readReference(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = () => reject(new Error("参考图片读取失败"));
      reader.readAsDataURL(file);
    });
  }
  $("video-image").addEventListener("change", async () => {
    errorAt("video-error", "");
    const file = $("video-image").files[0];
    if (!file) {
      $("reference-preview").hidden = true;
      return;
    }
    if (
      file.size > 10 * 1024 * 1024 ||
      !["image/png", "image/jpeg", "image/webp", "image/gif"].includes(
        file.type,
      )
    ) {
      errorAt("video-error", "请选择 10 MiB 以内的 PNG、JPG、WebP 或 GIF 图片");
      $("video-image").value = "";
      $("reference-preview").hidden = true;
      return;
    }
    try {
      $("reference-image").src = await readReference(file);
      $("reference-preview").hidden = false;
    } catch (error) {
      errorAt("video-error", error.message);
    }
  });
  $("remove-reference").addEventListener("click", () => {
    $("video-image").value = "";
    $("reference-image").removeAttribute("src");
    $("reference-preview").hidden = true;
  });
  $("video-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    errorAt("video-error", "");
    busy($("video-submit"), true, "正在提交…");
    try {
      const file = $("video-image").files[0];
      await api("/videos", {
        method: "POST",
        body: {
          prompt: $("video-prompt").value.trim(),
          model: $("video-model").value,
          seconds: $("video-seconds").value,
          size: $("video-size").value,
          input_reference: file ? await readReference(file) : undefined,
        },
      });
      toast("视频任务已提交");
      await loadVideos();
    } catch (error) {
      errorAt("video-error", error.message);
    } finally {
      busy($("video-submit"), false);
    }
  });
  $("refresh-videos").addEventListener("click", loadVideos);
  $("nav-videos").addEventListener("click", () => navigate("videos"));
  async function copy(text, button) {
    if (!text) {
      toast("暂无可复制的内容", true);
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      const original = button.innerHTML;
      button.textContent = "已复制";
      button.disabled = true;
      setTimeout(() => {
        button.innerHTML = original;
        button.disabled = false;
      }, 1300);
    } catch {
      toast("复制失败，请手动选择并复制", true);
    }
  }
  function openAdd() {
    state.flow = null;
    $("create-flow-form").hidden = false;
    $("callback-form").hidden = true;
    $("flow-success").hidden = true;
    $("callback-url").value = "";
    $("authorization-url").removeAttribute("href");
    errorAt("flow-error", "");
    setSteps("discovery");
    $("add-dialog").showModal();
  }
  function setSteps(step) {
    const order = ["discovery", "authorize", "complete"];
    for (const item of order) {
      $("step-" + item).classList.toggle("active", item === step);
      $("step-" + item).classList.toggle(
        "completed",
        order.indexOf(item) < order.indexOf(step),
      );
    }
  }
  function renderFlow(flowState) {
    state.flow.state = flowState;
    setSteps(flowState.step);
    $("create-flow-form").hidden = true;
    $("callback-form").hidden = flowState.step === "complete";
    $("flow-success").hidden = flowState.step !== "complete";
    if (flowState.step === "complete") {
      $("authorization-url").removeAttribute("href");
      $("callback-url").value = "";
      loadAccounts();
      return;
    }
    if (flowState.step === "expired") {
      errorAt("flow-error", "授权链接已过期，请关闭窗口并重新添加账号");
      $("callback-form").hidden = true;
      return;
    }
    const isDiscovery = flowState.step === "discovery";
    $("authorization-title").textContent = isDiscovery
      ? "第一步：登录 Amazon Quick"
      : "第二步：确认账号授权";
    $("authorization-description").textContent = isDiscovery
      ? "打开登录页面并完成登录。浏览器跳转到 localhost 后，复制地址栏中的完整地址，粘贴到下方。"
      : "打开下面的授权页面，确认账号访问授权。跳转到 127.0.0.1 或 localhost 后，再次复制完整地址并提交。";
    const url = new URL(flowState.url);
    if (url.protocol !== "https:")
      throw new Error("授权地址无效，请重新添加账号");
    $("authorization-url").href = url.href;
    $("authorization-link-label").textContent = isDiscovery
      ? "打开 Amazon Quick"
      : "打开账号授权页面";
    $("submit-callback").textContent = isDiscovery ? "提交并继续" : "完成授权";
  }
  async function closeAdd() {
    if ($("submit-callback").disabled || $("create-flow-button").disabled)
      return;
    $("add-dialog").close();
    const flow = state.flow;
    state.flow = null;
    $("callback-url").value = "";
    $("authorization-url").removeAttribute("href");
    if (flow)
      await api("/login-flows/" + encodeURIComponent(flow.id), {
        method: "DELETE",
      }).catch(() => {});
  }
  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    errorAt("login-error", "");
    busy($("login-submit"), true, "正在登录…");
    try {
      const session = await api("/login", {
        method: "POST",
        body: {
          username: $("username").value.trim(),
          password: $("password").value,
        },
      });
      $("password").value = "";
      showConsole(session);
      await Promise.all([loadAccounts(), loadConnection()]);
    } catch (error) {
      errorAt("login-error", error.message);
      $("password").focus();
    } finally {
      busy($("login-submit"), false);
    }
  });
  $("logout-button").addEventListener("click", async () => {
    busy($("logout-button"), true);
    try {
      await api("/logout", { method: "POST" });
      showLogin();
    } catch (error) {
      toast(error.message, true);
    } finally {
      busy($("logout-button"), false);
    }
  });
  $("nav-accounts").addEventListener("click", () => navigate("accounts"));
  $("nav-connection").addEventListener("click", () => navigate("connection"));
  $("refresh-list").addEventListener("click", loadAccounts);
  $("retry-load").addEventListener("click", loadAccounts);
  $("account-search").addEventListener("input", renderAccounts);
  $("status-filter").addEventListener("change", renderAccounts);
  $("copy-base").addEventListener("click", () =>
    copy(state.connection?.base_url, $("copy-base")),
  );
  $("copy-key").addEventListener("click", () =>
    copy(state.connection?.api_keys[0], $("copy-key")),
  );
  function claudeEnvironment() {
    if (!state.connection) return null;
    return {
      ANTHROPIC_BASE_URL: state.connection.anthropic_base_url,
      ANTHROPIC_AUTH_TOKEN: state.connection.api_keys[0] || "",
      ANTHROPIC_MODEL: "smart",
      ANTHROPIC_DEFAULT_OPUS_MODEL: "smart",
      ANTHROPIC_DEFAULT_SONNET_MODEL: "smart",
      ANTHROPIC_DEFAULT_HAIKU_MODEL: "fast",
      DISABLE_PROMPT_CACHING: "1",
      ENABLE_TOOL_SEARCH: "false",
      MAX_THINKING_TOKENS: "0",
    };
  }
  $("copy-claude-powershell").addEventListener("click", () => {
    const env = claudeEnvironment();
    if (!env) return;
    copy(
      Object.entries(env)
        .map(([name, value]) => `$env:${name} = '${value.replace(/'/g, "''")}'`)
        .join("\n") + "\nclaude",
      $("copy-claude-powershell"),
    );
  });
  $("copy-claude-vscode").addEventListener("click", () => {
    const env = claudeEnvironment();
    if (!env) return;
    copy(
      JSON.stringify(
        {
          "claudeCode.environmentVariables": Object.entries(env).map(
            ([name, value]) => ({ name, value }),
          ),
        },
        null,
        2,
      ),
      $("copy-claude-vscode"),
    );
  });
  $("reveal-key").addEventListener("click", () => {
    const showing = $("api-key").type === "password";
    $("api-key").type = showing ? "text" : "password";
    $("reveal-key").textContent = showing ? "隐藏" : "显示";
    $("reveal-key").setAttribute("aria-pressed", String(showing));
  });
  document
    .querySelectorAll("[data-add-account]")
    .forEach((button) => button.addEventListener("click", openAdd));
  $("provider").addEventListener("change", () => {
    $("region-field").hidden = $("provider").value !== "enterprise";
  });
  $("close-add").addEventListener("click", closeAdd);
  $("finish-flow").addEventListener("click", closeAdd);
  $("add-dialog").addEventListener("cancel", (event) => {
    event.preventDefault();
    closeAdd();
  });
  $("create-flow-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    errorAt("flow-error", "");
    busy($("create-flow-button"), true, "正在生成…");
    try {
      state.flow = await api("/login-flows", {
        method: "POST",
        body: {
          provider: $("provider").value,
          region:
            $("provider").value === "enterprise"
              ? $("region").value.trim()
              : "",
        },
      });
      renderFlow(state.flow.state);
    } catch (error) {
      errorAt("flow-error", error.message);
    } finally {
      busy($("create-flow-button"), false);
    }
  });
  $("callback-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!state.flow) return;
    errorAt("flow-error", "");
    busy($("submit-callback"), true, "正在处理授权…");
    try {
      const result = await api(
        "/login-flows/" + encodeURIComponent(state.flow.id) + "/callback",
        { method: "POST", body: { url: $("callback-url").value.trim() } },
      );
      $("callback-url").value = "";
      busy($("submit-callback"), false);
      renderFlow(result);
    } catch (error) {
      errorAt("flow-error", error.message);
    } finally {
      if ($("submit-callback").disabled) busy($("submit-callback"), false);
    }
  });
  $("account-rows").addEventListener("click", async (event) => {
    const button = event.target.closest("[data-action]");
    if (!button || button.disabled) return;
    const account = state.accounts.find(
      (a) => a.id === Number(button.dataset.id),
    );
    if (!account) return;
    if (button.dataset.action === "delete") {
      state.deleting = account;
      $("delete-account-name").textContent =
        account.email || account.name || `账号 ${account.id}`;
      errorAt("delete-error", "");
      $("delete-dialog").showModal();
      return;
    }
    busy(
      button,
      true,
      button.dataset.action === "refresh" ? "刷新中…" : "处理中…",
    );
    try {
      if (button.dataset.action === "refresh") {
        await api(`/accounts/${account.id}/refresh`, { method: "POST" });
        toast("账号刷新成功");
      } else {
        await api(`/accounts/${account.id}`, {
          method: "PATCH",
          body: { disabled: !account.disabled },
        });
        toast(account.disabled ? "账号已启用" : "账号已停用");
      }
      await loadAccounts();
    } catch (error) {
      toast(error.message, true);
      await loadAccounts();
    } finally {
      if (button.isConnected) busy(button, false);
    }
  });
  function closeDelete() {
    if (!$("confirm-delete").disabled) {
      $("delete-dialog").close();
      state.deleting = null;
    }
  }
  $("close-delete").addEventListener("click", closeDelete);
  $("cancel-delete").addEventListener("click", closeDelete);
  $("delete-dialog").addEventListener("cancel", (event) => {
    event.preventDefault();
    closeDelete();
  });
  $("confirm-delete").addEventListener("click", async () => {
    if (!state.deleting) return;
    busy($("confirm-delete"), true, "正在删除…");
    errorAt("delete-error", "");
    try {
      await api(`/accounts/${state.deleting.id}`, { method: "DELETE" });
      $("delete-dialog").close();
      state.deleting = null;
      toast("账号已删除");
      await loadAccounts();
    } catch (error) {
      errorAt("delete-error", error.message);
    } finally {
      busy($("confirm-delete"), false);
    }
  });
  async function start() {
    try {
      const session = await api("/session");
      showConsole(session);
      await Promise.all([loadAccounts(), loadConnection()]);
    } catch (error) {
      showLogin();
      if (error.message && !error.message.includes("登录"))
        errorAt("login-error", "连接失败，请刷新页面重试");
    }
  }
  start();
})();
