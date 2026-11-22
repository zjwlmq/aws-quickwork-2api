"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const state = {
    session: null,
    accounts: [],
    quotas: [],
    quotaLoading: false,
    connection: null,
    flow: null,
    deleting: null,
    page: "accounts",
    loading: false,
    videos: [],
    logs: [],
    requests: [],
    requestOffset: 0,
    requestTotal: 0,
    requestDetail: null,
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
  let logPollTimer;
  let logLoadVersion = 0;
  let requestLoadVersion = 0;
  let requestDetailVersion = 0;
  let requestSearchTimer;
  let quotaPollTimer;
  let quotaLoadVersion = 0;
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
    state.quotas = [];
    state.quotaLoading = false;
    quotaLoadVersion++;
    clearTimeout(quotaPollTimer);
    $("quota-cards").replaceChildren();
    $("quota-updated").textContent = "";
    errorAt("quota-error", "");
    state.connection = null;
    state.flow = null;
    state.videos = [];
    clearTimeout(videoPollTimer);
    clearTimeout(logPollTimer);
    logLoadVersion++;
    state.logs = [];
    requestLoadVersion++;
    requestDetailVersion++;
    state.requests = [];
    state.requestDetail = null;
    state.requestOffset = 0;
    $("request-search").value = "";
    $("request-rows").replaceChildren();
    for (const key of [
      "request-summary-json",
      "request-input-json",
      "request-output-json",
      "request-detail-id",
      "request-detail-note",
    ])
      $(key).textContent = "";
    if ($("request-dialog").open) $("request-dialog").close();
    $("log-entries").replaceChildren();
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
      location.hash === "#requests"
        ? "requests"
        : location.hash === "#logs"
          ? "logs"
          : location.hash === "#videos"
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
  function filteredAccounts() {
    const query = $("account-search").value.trim().toLowerCase();
    const filter = $("status-filter").value;
    return state.accounts.filter(
      (a) =>
        (!query ||
          `${a.name} ${a.email} ${a.id}`.toLowerCase().includes(query)) &&
        (filter === "all" || a.status === filter),
    );
  }
  function renderAccounts() {
    $("count-total").textContent = state.accounts.length;
    $("count-ready").textContent = state.accounts.filter(
      (a) => a.status === "ready",
    ).length;
    $("count-attention").textContent = state.accounts.filter(
      (a) => a.status === "error" || a.status === "expired",
    ).length;
    const list = filteredAccounts();
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
    renderQuotas();
  }
  function quotaPercent(value) {
    if (typeof value !== "number" || !Number.isFinite(value)) return "—";
    if (value > 0 && value < 0.1) return "<0.1%";
    if (value < 100 && value > 99.9) return "99.9%";
    return (
      new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(
        value,
      ) + "%"
    );
  }
  function quotaPlan(subscription) {
    const kind = subscription?.type || "";
    if (kind.includes("FREE_TRIAL")) return "免费试用";
    for (const plan of [
      "MAX",
      "PLUS",
      "PROFESSIONAL",
      "ENTERPRISE",
      "ESSENTIALS",
      "FREE",
    ])
      if (kind.includes(plan))
        return plan === "FREE" ? "Free" : plan[0] + plan.slice(1).toLowerCase();
    return "Quick 账号";
  }
  function quotaWindowHTML(window, label, name) {
    const hasValue =
      typeof window?.remaining_percent === "number" &&
      Number.isFinite(window.remaining_percent);
    const used = hasValue ? Math.min(100, Math.max(0, window.used_percent)) : 0;
    const severity =
      window?.warning?.severity === "error" ||
      (hasValue && window.remaining_percent === 0)
        ? "error"
        : window?.warning?.severity === "warning" ||
            (hasValue && window.remaining_percent <= 10)
          ? "warning"
          : "normal";
    const reset = window?.resets_at
      ? `${date(window.resets_at)} 重置`
      : window?.resume_in_minutes > 0
        ? `约 ${window.resume_in_minutes} 分钟后恢复`
        : "";
    return `<div class="quota-window ${severity}"><div class="quota-window-heading"><span>${escape(label)}剩余</span><strong>${hasValue ? escape(quotaPercent(window.remaining_percent)) : "—"}</strong></div><div class="quota-window-caption"><span>${hasValue ? "已用 " + escape(quotaPercent(window.used_percent)) : "上游暂未提供可计算的额度"}</span>${reset ? `<span>${escape(reset)}</span>` : ""}</div>${hasValue ? `<progress class="quota-track" max="100" value="${used}" aria-label="${escape(name + " " + label + "已使用")}"></progress>` : ""}${window?.warning?.description ? `<p class="quota-warning">${escape(window.warning.description)}</p>` : ""}</div>`;
  }
  function renderQuotas() {
    $("quota-panel").hidden = state.accounts.length === 0;
    const byID = new Map(state.quotas.map((item) => [item.account_id, item]));
    $("quota-cards").innerHTML = filteredAccounts()
      .map((account) => {
        const item = byID.get(account.id);
        const name = account.email || account.name || `账号 ${account.id}`;
        const hasData =
          item?.status === "available" || item?.status === "stale";
        const trial = item?.subscription?.trial_expires_at;
        const trialText = trial
          ? `试用${trial <= Date.now() / 1000 ? "已到期" : "到期"} ${date(trial)}`
          : "";
        const note =
          item?.status === "stale"
            ? `显示上次数据 · ${item.error}`
            : item?.error ||
              (state.quotaLoading
                ? "正在查询账号额度…"
                : "尚未读取额度，请点击刷新额度");
        return `<article class="quota-card${item?.status === "stale" ? " stale" : ""}" data-quota-account="${account.id}"><div class="quota-card-heading"><div><h3 title="${escape(name)}">${escape(name)}</h3><span class="quota-account-id">账号 ${account.id}${account.disabled ? " · 已停用" : ""}</span></div><span class="quota-plan">${escape(quotaPlan(item?.subscription))}</span></div>${hasData ? quotaWindowHTML(item.monthly, "月度额度", name) + quotaWindowHTML(item.session, "会话额度", name) + (item.overage_monthly ? quotaWindowHTML(item.overage_monthly, "额外额度", name) : "") : `<p class="quota-unavailable">${escape(note)}</p>`}${hasData && item.status === "stale" ? `<p class="quota-warning">${escape(note)}</p>` : ""}<div class="quota-card-footer"><span>${escape(trialText)}</span><span>${item?.updated_at ? "更新于 " + escape(date(item.updated_at)) : ""}</span></div></article>`;
      })
      .join("");
  }
  async function loadQuotas(force = false) {
    if (!state.session || state.page !== "accounts" || state.quotaLoading)
      return;
    const version = ++quotaLoadVersion;
    state.quotaLoading = true;
    clearTimeout(quotaPollTimer);
    busy($("refresh-quotas"), true, "查询中…");
    $("quota-updated").textContent = "正在从 Quick 查询…";
    errorAt("quota-error", "");
    renderQuotas();
    try {
      const data = await api(force ? "/quotas/refresh" : "/quotas", {
        method: force ? "POST" : "GET",
      });
      if (
        version !== quotaLoadVersion ||
        !state.session ||
        state.page !== "accounts"
      )
        return;
      state.quotas = data.data;
      const good = data.data.filter(
        (item) => item.status === "available",
      ).length;
      $("quota-updated").textContent =
        `${good} / ${data.data.length} 个账号查询成功 · 页面打开时自动更新`;
      renderQuotas();
    } catch (error) {
      if (
        version !== quotaLoadVersion ||
        !state.session ||
        state.page !== "accounts"
      )
        return;
      errorAt("quota-error", error.message);
      const previous = new Map(
        state.quotas.map((item) => [item.account_id, item]),
      );
      state.quotas = state.accounts.map((account) => {
        const old = previous.get(account.id);
        return {
          ...old,
          account_id: account.id,
          status: old?.updated_at ? "stale" : "error",
          error: error.message,
        };
      });
      $("quota-updated").textContent =
        "本次更新失败，已有额度保留为上次查询结果";
    } finally {
      if (version === quotaLoadVersion) {
        state.quotaLoading = false;
        renderQuotas();
        busy($("refresh-quotas"), false);
        if (state.session && state.page === "accounts")
          quotaPollTimer = setTimeout(() => {
            if (!document.hidden) loadQuotas();
          }, 60000);
      }
    }
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
      if (state.page === "accounts") loadQuotas();
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
    logLoadVersion++;
    requestLoadVersion++;
    quotaLoadVersion++;
    state.quotaLoading = false;
    clearTimeout(quotaPollTimer);
    busy($("refresh-quotas"), false);
    state.page = page;
    $("accounts-section").hidden = page !== "accounts";
    $("header-actions").hidden = page !== "accounts";
    $("key-details").hidden = page !== "connection";
    $("videos-section").hidden = page !== "videos";
    $("logs-section").hidden = page !== "logs";
    $("requests-section").hidden = page !== "requests";
    $("connection-panel").hidden = !["accounts", "connection"].includes(page);
    $("page-title").textContent =
      page === "accounts"
        ? "账号管理"
        : page === "videos"
          ? "视频生成"
          : page === "logs"
            ? "诊断日志"
            : page === "requests"
              ? "请求日志"
              : "接入信息";
    $("page-description").textContent =
      page === "accounts"
        ? "管理 Amazon Quick 授权，账号自动轮询使用。"
        : page === "videos"
          ? "通过 Quick 聊天制作视频，查看任务进度与成片。"
          : page === "logs"
            ? "按请求查看完整处理过程，定位失败阶段与上游异常。"
            : page === "requests"
              ? "查看最近 2,000 次调用；出错时可复制或下载详细请求与返回。"
              : "在 OpenAI 兼容客户端中使用此服务。";
    history.replaceState(null, "", "#" + page);
    clearTimeout(videoPollTimer);
    clearTimeout(logPollTimer);
    if (page === "videos") loadVideos();
    if (page === "logs") loadLogs();
    if (page === "requests") loadRequests();
    if (page === "accounts") loadQuotas();
    for (const name of [
      "accounts",
      "connection",
      "videos",
      "logs",
      "requests",
    ]) {
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
  $("nav-logs").addEventListener("click", () => navigate("logs"));
  $("nav-requests").addEventListener("click", () => navigate("requests"));
  const requestStatusLabels = {
    success: "成功",
    error: "失败",
    recovered: "出错后成功",
    cancelled: "已取消",
    interrupted: "已中断",
    running: "处理中",
    queued: "已提交",
    in_progress: "生成中",
  };
  const tokenNumber = (value) =>
    new Intl.NumberFormat("zh-CN").format(value || 0);
  function renderRequests() {
    const protocols = {
      anthropic_messages: "messages",
      openai_chat: "chat.completions",
      video: "videos",
    };
    $("request-rows").innerHTML = state.requests
      .map((row) => {
        const time = new Intl.DateTimeFormat("zh-CN", {
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        }).format(new Date(row.started_at));
        const session = row.session_id || "未识别会话";
        const account = row.account_id
          ? `账号 ${row.account_id}`
          : "尚未选择账号";
        const model = row.model || "未解析模型";
        const tokens = row.usage_known
          ? `${tokenNumber(row.input_tokens)} / ${tokenNumber(row.output_tokens)}`
          : "— / —";
        const usageHint = row.usage_known
          ? `缓存读 ${tokenNumber(row.cache_read_tokens)} · 写 ${tokenNumber(row.cache_write_tokens)}`
          : "暂无上游用量";
        const status = requestStatusLabels[row.status] || row.status;
        return `<tr><td data-label="时间 / 密钥"><span>${escape(time)}</span><small>${escape(row.key_id || "管理员 / 未携带密钥")}</small><small>${escape(account)} · ${escape(session)}</small></td><td data-label="模型 / 协议"><span class="request-model">${escape(model)}</span><small class="request-protocol">${escape(protocols[row.protocol] || row.protocol)} · ${row.stream ? "stream" : "JSON"}</small><small>${row.upstream_model ? `路由 ${escape(row.upstream_model)}` : "尚未建立上游连接"}${row.retry_count ? ` · 重试 ${row.retry_count} 次` : ""}</small></td><td data-label="状态"><span class="request-status ${escape(row.status)}"><i aria-hidden="true"></i>${escape(status)}</span><small>HTTP ${row.http_status || "—"}</small></td><td data-label="输入 / 输出 Token"><span>${tokens}</span><small>${usageHint}</small></td><td data-label="耗时"><span>${(row.duration_ms / 1000).toFixed(2)} s</span></td><td data-label="操作"><button class="text-button request-details-button" type="button" data-request-id="${escape(row.id)}">详情</button></td></tr>`;
      })
      .join("");
    $("requests-empty").hidden = state.requests.length > 0;
    $("request-count").textContent =
      `共 ${tokenNumber(state.requestTotal)} 条${state.requests.length ? ` · 当前 ${state.requestOffset + 1}–${state.requestOffset + state.requests.length}` : ""}`;
    $("request-page").textContent = String(
      Math.floor(state.requestOffset / 50) + 1,
    );
    $("requests-previous").disabled = state.requestOffset === 0;
    $("requests-next").disabled =
      state.requestOffset + 50 >= state.requestTotal;
  }
  async function loadRequests() {
    const version = ++requestLoadVersion;
    errorAt("request-error", "");
    $("request-count").textContent = "正在读取调用记录…";
    const query = new URLSearchParams({
      limit: "50",
      offset: String(state.requestOffset),
      status: $("request-status").value,
      search: $("request-search").value.trim(),
    });
    try {
      const search = query.get("search");
      const possibleKey = search.replace(/^Bearer\s+/i, "");
      if (
        possibleKey &&
        (/^sk-[A-Za-z0-9_-]{8,}$/.test(possibleKey) ||
          state.connection?.api_keys?.includes(possibleKey))
      ) {
        const digest = await crypto.subtle.digest(
          "SHA-256",
          new TextEncoder().encode(possibleKey),
        );
        query.set(
          "search",
          "key_" +
            Array.from(new Uint8Array(digest), (byte) =>
              byte.toString(16).padStart(2, "0"),
            )
              .join("")
              .slice(0, 12),
        );
      }
      const result = await api("/requests?" + query);
      if (
        version !== requestLoadVersion ||
        !state.session ||
        state.page !== "requests"
      )
        return;
      state.requests = result.data;
      state.requestTotal = result.total;
      renderRequests();
    } catch (error) {
      if (version === requestLoadVersion && state.page === "requests")
        errorAt("request-error", error.message);
    }
  }
  function closeRequestDetail() {
    requestDetailVersion++;
    state.requestDetail = null;
    $("request-dialog").close();
  }
  async function openRequestDetail(id) {
    const version = ++requestDetailVersion;
    state.requestDetail = null;
    errorAt("request-detail-error", "");
    $("request-detail-id").textContent = id;
    $("request-detail-note").textContent = "正在读取详情…";
    for (const key of [
      "request-summary-json",
      "request-input-json",
      "request-output-json",
    ])
      $(key).textContent = "";
    for (const key of [
      "copy-request-json",
      "copy-response-json",
      "download-request-detail",
      "request-diagnostics",
    ])
      $(key).disabled = true;
    $("request-dialog").showModal();
    try {
      const detail = await api("/requests/" + encodeURIComponent(id));
      if (version !== requestDetailVersion || !state.session) return;
      state.requestDetail = detail;
      $("request-summary-json").textContent = JSON.stringify(
        {
          record: detail.record,
          attempts: detail.attempts || [],
          events: detail.events || [],
        },
        null,
        2,
      );
      $("request-input-json").textContent = JSON.stringify(
        detail.request || { note: detail.body_note },
        null,
        2,
      );
      $("request-output-json").textContent = JSON.stringify(
        detail.response || { note: detail.body_note },
        null,
        2,
      );
      $("request-detail-note").textContent =
        (detail.storage === "disk"
          ? "已保存的报错详情"
          : detail.storage === "memory"
            ? "本次正文暂存于内存，服务重启或内存清理后仅保留概要"
            : "正文详情已清理，仅保留调用概要") +
        "。" +
        (detail.redaction_note || detail.body_note || "");
      for (const key of [
        "copy-request-json",
        "copy-response-json",
        "download-request-detail",
        "request-diagnostics",
      ])
        $(key).disabled = false;
      $("copy-request-json").disabled = !detail.request;
      $("copy-response-json").disabled = !detail.response;
    } catch (error) {
      if (version === requestDetailVersion)
        errorAt("request-detail-error", error.message);
    }
  }
  $("request-rows").addEventListener("click", (event) => {
    const button = event.target.closest("[data-request-id]");
    if (button) openRequestDetail(button.dataset.requestId);
  });
  $("close-request-detail").addEventListener("click", closeRequestDetail);
  $("request-dialog").addEventListener("cancel", (event) => {
    event.preventDefault();
    closeRequestDetail();
  });
  $("refresh-requests").addEventListener("click", loadRequests);
  $("request-status").addEventListener("change", () => {
    state.requestOffset = 0;
    loadRequests();
  });
  $("request-search").addEventListener("input", () => {
    clearTimeout(requestSearchTimer);
    requestSearchTimer = setTimeout(() => {
      state.requestOffset = 0;
      loadRequests();
    }, 300);
  });
  $("requests-previous").addEventListener("click", () => {
    state.requestOffset = Math.max(0, state.requestOffset - 50);
    loadRequests();
  });
  $("requests-next").addEventListener("click", () => {
    state.requestOffset += 50;
    loadRequests();
  });
  $("copy-request-json").addEventListener("click", () =>
    copy($("request-input-json").textContent, $("copy-request-json")),
  );
  $("copy-response-json").addEventListener("click", () =>
    copy($("request-output-json").textContent, $("copy-response-json")),
  );
  $("download-request-detail").addEventListener("click", async () => {
    const detail = state.requestDetail;
    if (!detail) return;
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(detail, null, 2)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = `quickwork-request-${detail.record.id}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });
  $("request-diagnostics").addEventListener("click", () => {
    const id = state.requestDetail?.record.id;
    if (!id) return;
    closeRequestDetail();
    $("log-request").value = id;
    $("log-search").value = "";
    $("log-level").value = "";
    navigate("logs");
  });
  function logQuery(limit) {
    return new URLSearchParams({
      request_id: $("log-request").value.trim(),
      search: $("log-search").value.trim(),
      level: $("log-level").value,
      limit: String(limit),
    });
  }
  function renderLogs(requestID) {
    const root = $("log-entries");
    const opened = new Set(
      Array.from(
        root.querySelectorAll("details[open]"),
        (node) => node.dataset.entryId,
      ),
    );
    const entries = requestID ? [...state.logs].reverse() : state.logs;
    if (!entries.length) {
      root.innerHTML =
        '<p class="logs-empty">没有匹配的日志。可调整筛选条件，或发起一次 API 请求后重新查询。</p>';
      return;
    }
    const nodes = entries.map((entry) => {
      const node = document.createElement("details");
      node.className = "log-entry";
      node.dataset.entryId = entry.entry_id;
      node.open = opened.has(entry.entry_id);
      const level = ["DEBUG", "INFO", "WARN", "ERROR"].includes(entry.level)
        ? entry.level
        : "INFO";
      const time = new Date(entry.time);
      const timeText = Number.isNaN(time.valueOf())
        ? entry.time
        : new Intl.DateTimeFormat("zh-CN", {
            month: "2-digit",
            day: "2-digit",
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
            hour12: false,
          }).format(time) +
          "." +
          String(time.getMilliseconds()).padStart(3, "0");
      const hints = [
        entry.request_id,
        entry.account_id ? `账号 ${entry.account_id}` : null,
        entry.attempt ? `第 ${entry.attempt} 次尝试` : null,
        entry.duration_ms !== undefined ? `${entry.duration_ms} ms` : null,
        entry.status ? `状态 ${entry.status}` : null,
      ]
        .filter(Boolean)
        .join(" · ");
      node.innerHTML = `<summary><span class="log-time">${escape(timeText)}</span><span class="log-level ${level.toLowerCase()}">${level}</span><span class="log-event">${escape(entry.event)}<small>${escape(hints)}</small></span></summary><div class="log-detail"><div class="log-detail-actions">${entry.request_id ? '<button type="button" class="text-button" data-log-trace>查看此请求全部事件</button>' : ""}<button type="button" class="text-button" data-log-copy>复制事件 JSON</button></div><pre>${escape(JSON.stringify(entry, null, 2))}</pre></div>`;
      node.querySelector("[data-log-trace]")?.addEventListener("click", () => {
        $("log-request").value = entry.request_id;
        $("log-search").value = "";
        $("log-level").value = "";
        loadLogs();
      });
      node
        .querySelector("[data-log-copy]")
        .addEventListener("click", (event) =>
          copy(JSON.stringify(entry, null, 2), event.currentTarget),
        );
      return node;
    });
    root.replaceChildren(...nodes);
  }
  async function loadLogs() {
    clearTimeout(logPollTimer);
    const version = ++logLoadVersion;
    const query = logQuery(500);
    $("log-status").textContent = "正在读取日志…";
    errorAt("log-error", "");
    try {
      const result = await api("/logs?" + query);
      if (version !== logLoadVersion || !state.session || state.page !== "logs")
        return;
      state.logs = result.entries;
      renderLogs(query.get("request_id"));
      const order = query.get("request_id") ? "按时间正序" : "最新在前";
      $("log-status").textContent =
        `${result.entries.length} 条 · ${order} · ${result.configured_level.toUpperCase()} · 每文件 ${result.max_mb} MiB / ${result.backups} 个备份${result.truncated ? " · 已达显示或扫描上限，可缩小筛选范围" : ""}`;
      if (result.write_error)
        errorAt("log-error", "日志写入异常：" + result.write_error);
    } catch (error) {
      if (version === logLoadVersion && state.page === "logs")
        errorAt("log-error", error.message);
    } finally {
      if (
        version === logLoadVersion &&
        state.session &&
        state.page === "logs" &&
        $("log-auto").checked
      )
        logPollTimer = setTimeout(loadLogs, 10000);
    }
  }
  $("logs-filter").addEventListener("submit", (event) => {
    event.preventDefault();
    loadLogs();
  });
  $("clear-logs-filter").addEventListener("click", () => {
    $("log-request").value = "";
    $("log-search").value = "";
    $("log-level").value = "";
    loadLogs();
  });
  $("log-auto").addEventListener("change", () => {
    clearTimeout(logPollTimer);
    if ($("log-auto").checked) loadLogs();
  });
  $("export-logs").addEventListener("click", async () => {
    const button = $("export-logs");
    busy(button, true, "正在导出…");
    try {
      const response = await fetch(
        "/admin/api/logs?" + logQuery(5000) + "&download=1",
        { credentials: "same-origin", cache: "no-store" },
      );
      if (!response.ok) throw new Error("导出失败，请重新登录或稍后重试");
      const url = URL.createObjectURL(await response.blob());
      const link = document.createElement("a");
      link.href = url;
      link.download = "quickwork-diagnostics.jsonl";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      toast(
        response.headers.get("X-Log-Truncated") === "true"
          ? "已导出，结果达到条数或扫描上限"
          : "诊断日志已导出",
      );
    } catch (error) {
      toast(error.message, true);
    } finally {
      busy(button, false);
    }
  });
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
  $("refresh-quotas").addEventListener("click", () => loadQuotas(true));
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && state.session && state.page === "accounts")
      loadQuotas();
  });
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
    const settings = JSON.stringify({ env, model: "smart" }, null, 2);
    copy(
      "$quickworkClaudeDirectory = Join-Path $env:USERPROFILE '.claude'\nNew-Item -ItemType Directory -Force -Path $quickworkClaudeDirectory | Out-Null\n$quickworkClaudeSettings = Join-Path $quickworkClaudeDirectory 'quickwork.settings.json'\n@'\n" +
        settings +
        "\n'@ | Set-Content -LiteralPath $quickworkClaudeSettings -Encoding UTF8\nclaude --settings $quickworkClaudeSettings --model smart",
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
  $("copy-claude-settings").addEventListener("click", () => {
    const env = claudeEnvironment();
    if (env)
      copy(
        JSON.stringify({ env, model: "smart" }, null, 2),
        $("copy-claude-settings"),
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
