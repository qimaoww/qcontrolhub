const generatedPlanFields = Object.freeze([
  "tag",
  "port",
  "username",
  "credential",
  "secondary_credential",
  "transport_path",
  "reality_private_key",
  "reality_public_key",
  "reality_short_id",
]);

const generatedFieldActions = Object.freeze([
  ["tag", "tag", "生成入站标签"],
  ["port", "port", "生成监听端口"],
  ["username", "username", "生成用户名"],
  ["credential", "credential", "生成凭据"],
  ["secondary_credential", "secondary_credential", "生成次凭据"],
  ["transport_path", "transport_path", "生成路径或 ServiceName"],
  [
    "reality_public_key",
    "reality_private_key,reality_public_key",
    "生成 Reality 密钥对",
  ],
  [
    "reality_private_key",
    "reality_private_key,reality_public_key",
    "生成 Reality 密钥对",
  ],
  ["reality_short_id", "reality_short_id", "生成 Short ID"],
]);

function installGeneratedFieldButtons(form) {
  generatedFieldActions.forEach(([name, fields, label]) => {
    const control = form.elements.namedItem(name);
    if (!control || control.type === "hidden") return;
    const button = document.createElement("button");
    button.className = "field-generate-button";
    button.type = "button";
    button.dataset.regenerate = fields;
    button.dataset.regenerateSuccess = `${label.replace(/^生成\s*/, "")}已生成`;
    button.setAttribute("aria-label", label);
    button.textContent = fields.includes(",") ? "生成密钥对" : "生成";
    if (control.parentElement.classList.contains("secret-value-control")) {
      control.parentElement.append(button);
      return;
    }
    const wrapper = document.createElement("span");
    wrapper.className = "generated-input-control";
    control.replaceWith(wrapper);
    wrapper.append(control, button);
    if (name === "transport_path") {
      const transport = form.elements.namedItem("transport");
      const updateVisibility = () => {
        button.hidden = transport?.value === "raw";
      };
      transport?.addEventListener("change", updateVisibility);
      updateVisibility();
    }
  });
}

export function readServerPlanInput(form, protocol) {
  const values = new FormData(form);
  return {
    protocol: protocol.key,
    tag: values.get("tag"),
    listen: values.get("listen"),
    port: Number(values.get("port")),
    username: values.get("username"),
    credential: values.get("credential"),
    secondary_credential: values.get("secondary_credential"),
    method: values.get("method"),
    flow: protocol.uses_reality ? "xtls-rprx-vision" : "",
    transport: values.get("transport"),
    transport_path: values.get("transport_path"),
    tls_enabled:
      values.get("tls_enabled") === "1" || protocol.requires_tls,
    certificate_path: values.get("certificate_path") || "",
    private_key_path: values.get("private_key_path") || "",
    reality_enabled: values.get("reality_enabled") === "1",
    reality_private_key: values.get("reality_private_key") || "",
    reality_public_key: values.get("reality_public_key") || "",
    reality_short_id: values.get("reality_short_id") || "",
    reality_server_name: values.get("reality_server_name") || "",
  };
}

export function bindServerPlanRegeneration({
  form,
  buttons,
  api,
  base,
  protocol,
  report,
  onApplied,
}) {
  let latestRequest = 0;
  let formRevision = 0;
  let pendingRequests = 0;
  const buttonState = new Map(
    buttons.map((button) => [
      button,
      { label: button.textContent, disabled: button.disabled },
    ]),
  );
  const markChanged = () => {
    formRevision += 1;
  };
  form.addEventListener("input", markChanged);
  form.addEventListener("change", markChanged);
  buttons.forEach((button) => {
    button.addEventListener("click", async () => {
      const request = ++latestRequest;
      const requestedRevision = formRevision;
      const input = readServerPlanInput(form, protocol);
      const requestedFields =
        !button.dataset.regenerate || button.dataset.regenerate === "all"
          ? generatedPlanFields
          : button.dataset.regenerate.split(",");
      pendingRequests += 1;
      buttons.forEach((item) => {
        item.disabled = true;
      });
      button.textContent = "生成中…";
      button.setAttribute("aria-busy", "true");
      try {
        const plan = await api(`${base}/plans`, {
          method: "POST",
          body: JSON.stringify({ protocol: protocol.key, input }),
        });
        if (request !== latestRequest || form.isConnected === false) return;
        if (requestedRevision !== formRevision) {
          report("当前参数已变更，未应用过期的生成结果", "error");
          return;
        }
        requestedFields.forEach((name) => {
          const control = form.elements.namedItem(name);
          if (control && Object.hasOwn(plan, name)) {
            control.value = String(plan[name] ?? "");
          }
        });
        onApplied?.(readServerPlanInput(form, protocol));
        report(button.dataset.regenerateSuccess || "参数已重新生成");
      } catch (error) {
        if (request === latestRequest && form.isConnected !== false) {
          report(`生成参数失败：${error.message}`, "error");
        }
      } finally {
        pendingRequests -= 1;
        if (pendingRequests === 0) {
          buttonState.forEach((initial, item) => {
            item.disabled = initial.disabled;
            item.textContent = initial.label;
            item.removeAttribute("aria-busy");
          });
        }
      }
    });
  });
}

export function installConfigPages(ctx) {
  const { api, optionalAPI, state, engines, can, esc, engineName, conciseVersion, date, ago, bytes, confirmAction, notify, shell, submitTask, bindCodeEditors } = ctx;
async function agentConfig() {
  const agents = state.data.agents || (await api("/agents"));
  state.data.agents = agents;
  const agent = agents.find((item) => item.id === state.data.agentId);
  if (!agent) return void (location.hash = "#node-settings");
  const engine = state.data.engine || agent.capabilities?.[0] || engines[0];
  state.data.engine = engine;
  const engineInstalled = Boolean(agent.runtime?.[engine]?.installed);
  const base = `/agents/${encodeURIComponent(agent.id)}/configs/${encodeURIComponent(engine)}`;
  const workspace = await api(`${base}/workspace`);
  const config = workspace.config;
  let selectedInbound = (workspace.inbounds || []).find(
    (input) => input.tag === state.data.inboundTag,
  );
  const selectedProtocolKey =
    state.data.protocol ||
    selectedInbound?.protocol ||
    workspace.protocols[0]?.key;
  const protocol = workspace.protocols.find(
    (item) => item.key === selectedProtocolKey,
  );
  let plan = selectedInbound;
  const planKey = `${agent.id}|${engine}|${selectedProtocolKey}`;
  state.data.serverPlans ||= {};
  if (!plan && can("operator") && protocol) {
    plan = state.data.serverPlans[planKey];
    if (!plan) {
      plan = await api(`${base}/plans`, {
        method: "POST",
        body: JSON.stringify({ protocol: selectedProtocolKey }),
      });
      state.data.serverPlans[planKey] = plan;
    }
  }
  plan ||= {
    protocol: selectedProtocolKey,
    listen: "0.0.0.0",
    port: protocol?.default_port || 443,
    transport: "raw",
  };
  const operation = selectedInbound ? "modify" : "add";
  const fields = workspace.catalog.fields || [];
  const selectedField =
    fields.find((field) => field.key === state.data.configField) || fields[0];
  state.data.configField = selectedField?.key || "";
  let fieldValue = { present: false, fragment: "" };
  if (config && selectedField)
    fieldValue = await api(
      `${base}/fields/${encodeURIComponent(selectedField.key)}`,
    );
  const revisions = config
    ? await api(`/configs/${encodeURIComponent(config.id)}/revisions?limit=50`)
    : [];
  const inboundNav = (workspace.inbounds || [])
    .map(
      (input) =>
        `<a class="${selectedInbound?.tag === input.tag ? "active" : ""}" href="#agent-config" data-inbound="${esc(input.tag)}"><span><strong>${esc(input.tag)}</strong><small>${esc(input.listen)}:${input.port}</small></span></a>`,
    )
    .join("");
  const protocolNav = workspace.protocols
    .map(
      (item) =>
        `<a class="${item.key === selectedProtocolKey ? "active" : ""}" href="#agent-config" data-protocol="${esc(item.key)}"><b>${esc(item.badge)}</b><span><strong>${esc(item.name)}</strong></span></a>`,
    )
    .join("");
  const methods = (protocol?.methods || [])
    .map(
      (method) =>
        `<option value="${esc(method)}" ${method === plan.method ? "selected" : ""}>${esc(method)}</option>`,
    )
    .join("");
  const transports = (protocol?.transports || ["raw"])
    .map(
      (transport) =>
        `<option value="${esc(transport)}" ${transport === plan.transport ? "selected" : ""}>${transport === "raw" ? "Raw / TCP" : transport === "websocket" ? "WebSocket" : "gRPC"}</option>`,
    )
    .join("");
  const security = protocol?.uses_reality
    ? `<input type="hidden" name="reality_enabled" value="1"><section class="builder-section security-section" id="security"><header><span class="section-number">04</span><strong>Reality</strong></header><div><div class="plan-fields two"><label>目标域名 / ServerName<input name="reality_server_name" list="reality-presets" required value="${esc(plan.reality_server_name)}"><datalist id="reality-presets">${workspace.reality_presets.map((value) => `<option value="${esc(value)}">`).join("")}</datalist><small>校验公网 DNS；拒绝 Cloudflare 与非公网地址。</small></label><label>Short ID<input name="reality_short_id" required value="${esc(plan.reality_short_id)}"></label></div><div class="plan-fields one"><label>客户端 Public Key<input name="reality_public_key" required value="${esc(plan.reality_public_key)}"></label><label class="secret-input">服务端 Private Key<span class="secret-value-control"><input type="password" name="reality_private_key" required value="${esc(plan.reality_private_key)}"><button type="button" data-secret-visibility>显示</button></span></label></div></div></section>`
    : protocol?.supports_tls
      ? `<input type="hidden" name="reality_enabled" value="0"><section class="builder-section security-section" id="security"><header><span class="section-number">04</span><strong>TLS</strong></header><div><label class="tls-switch"><input type="checkbox" name="tls_enabled" value="1" ${plan.tls_enabled || protocol.requires_tls ? "checked" : ""} ${protocol.requires_tls ? "disabled" : ""}><strong>${protocol.requires_tls ? "TLS" : "启用 TLS"}</strong></label><div class="plan-fields two"><label>证书路径<input name="certificate_path" value="${esc(plan.certificate_path)}"></label><label>私钥路径<input name="private_key_path" value="${esc(plan.private_key_path)}"></label></div><p class="validation-note">私钥仅目标内核服务组可读。</p></div></section>`
      : '<input type="hidden" name="reality_enabled" value="0"><input type="hidden" name="tls_enabled" value="0">';
  const sourceStudio = config
    ? `<details class="source-studio"><summary>完整源码</summary><form id="source-config-form"><div class="form-grid"><label>配置名称<input name="name" maxlength="100" required value="${esc(config.name)}"></label><label>说明<input name="description" maxlength="300" value="${esc(config.description)}"></label></div><textarea name="content" spellcheck="false" required>${esc(config.content)}</textarea><footer><div><button class="button" type="submit" data-source-intent="validate" ${engineInstalled ? "" : "disabled"}>保存源码并校验</button><button class="button primary" type="submit" data-source-intent="deploy" ${engineInstalled ? "" : "disabled"}>保存源码并部署</button></div></footer></form></details>`
    : "";
  const revisionTimeline = config
    ? `<details class="revision-timeline node-revision-timeline" id="revisions"><summary><b>版本历史</b><strong>${revisions.length} 个版本</strong></summary><div class="timeline-body"><nav>${revisions.map((revision) => `<span class="${revision.version === config.version ? "current" : ""}"><i></i><span><b>v${revision.version}</b><strong>${esc(revision.name)}</strong><small>${ago(revision.updated_at)}${revision.version === config.version ? " · 当前" : ""}</small></span></span>`).join("")}</nav><div class="timeline-placeholder">当前 v${config.version}</div></div></details>`
    : "";
  const executionCallout = !engineInstalled
    ? `<aside class="config-execution-callout"><span><b>${esc(engineName(engine))} 尚未安装</b><small>可以编辑方案，但校验、部署和服务操作需要先在节点设置中安装该内核。</small></span><a class="button small" href="#node-settings">前往安装内核</a></aside>`
    : "";
  shell(
    `<section class="config-command-bar loaded"><header class="config-command-head"><div class="config-command-title"><span class="engine-badge ${esc(engine)}">${esc(engineName(engine))}</span><div><p class="eyebrow">Server recipe</p><h2>${esc(protocol?.name || "Protocol")} · ${selectedInbound ? esc(selectedInbound.tag) : "新入站"}</h2><small>${esc(agent.name)} · ${esc(workspace.catalog.name)}</small></div></div><div class="config-command-state"><span class="status-label ${!engineInstalled ? "muted" : config ? "ok" : "warn"}">${!engineInstalled ? "内核未安装" : config ? "已读取" : "新方案"}</span><span class="recipe-version"><b>${config ? `v${config.version}` : "草稿"}</b><small>${esc(workspace.catalog.format)}</small></span><a href="${esc(protocol?.docs)}" target="_blank" rel="noopener noreferrer">文档 ↗</a></div></header><details class="config-hierarchy-menu" open><summary><b>切换入站 / 协议</b><i>＋</i></summary><div class="config-command-selectors">${inboundNav ? `<section class="inbound-browser config-selector"><header><span><b>入站</b><small>${workspace.inbounds.length} 个</small></span><button class="button small" type="button" data-new-inbound>＋ 新增</button></header><nav>${inboundNav}</nav></section>` : ""}<section class="protocol-browser config-selector"><header><span><b>协议</b><small>${workspace.protocols.length} 种</small></span></header><nav>${protocolNav}</nav></section></div></details></section>${executionCallout}<article class="recipe-workspace"><form class="server-form" id="server-plan-form"><div class="config-mutation"><label>操作<select name="operation">${selectedInbound ? `<option value="modify">修改 · ${esc(selectedInbound.tag)}</option><option value="add">新增入站</option><option value="delete">删除 · ${esc(selectedInbound.tag)}</option>` : '<option value="add">新增入站</option>'}</select></label></div><div class="builder-layout" data-builder-workbench><nav class="builder-index"><a href="#listen" data-builder-step="listen"><b>01</b><strong>监听</strong></a><a href="#identity" data-builder-step="identity"><b>02</b><strong>认证</strong></a>${protocol?.transport_config ? '<a href="#transport" data-builder-step="transport"><b>03</b><strong>传输</strong></a>' : ""}${protocol?.uses_reality || protocol?.supports_tls ? '<a href="#security" data-builder-step="security"><b>04</b><strong>安全</strong></a>' : ""}</nav><div class="builder-sections"><section class="builder-section" id="listen"><header><span class="section-number">01</span><strong>监听</strong></header><div class="plan-fields three"><label>入站标签<input name="tag" maxlength="64" required value="${esc(plan.tag)}"></label><label>监听地址<input name="listen" required value="${esc(plan.listen)}"></label><label>监听端口<input type="number" name="port" min="1" max="65535" required value="${Number(plan.port)}"></label></div></section><section class="builder-section" id="identity"><header><span class="section-number">02</span><strong>认证</strong></header><div><div class="plan-fields two">${protocol?.ignores_username ? '<input type="hidden" name="username" value="default">' : `<label>用户名或备注<input name="username" maxlength="64" required value="${esc(plan.username)}"></label>`}<label class="secret-input">${esc(protocol?.credential_label || "凭据")}<span class="secret-value-control"><input type="password" name="credential" required value="${esc(plan.credential)}"><button type="button" data-secret-visibility>显示</button></span></label>${protocol?.secondary_credential_label ? `<label class="secret-input">${esc(protocol.secondary_credential_label)}<span class="secret-value-control"><input type="password" name="secondary_credential" required value="${esc(plan.secondary_credential)}"><button type="button" data-secret-visibility>显示</button></span></label>` : '<input type="hidden" name="secondary_credential" value="">'}</div>${methods ? `<div class="plan-fields one"><label>加密方式<select name="method">${methods}</select></label></div>` : '<input type="hidden" name="method" value="">'}</div></section>${protocol?.transport_config ? `<section class="builder-section" id="transport"><header><span class="section-number">03</span><strong>传输</strong></header><div class="plan-fields two"><label>传输<select name="transport">${transports}</select></label><label>路径 / ServiceName<input name="transport_path" value="${esc(plan.transport_path)}"></label></div></section>` : '<input type="hidden" name="transport" value="raw"><input type="hidden" name="transport_path" value="">'}${security}</div></div><footer class="builder-actions compact"><span class="builder-regenerate-status" data-regenerate-status role="status" aria-live="polite"></span><div><button class="button" type="button" data-regenerate>重新生成参数</button><button class="button" type="submit" data-plan-intent="validate" ${agent.status !== "online" || !engineInstalled ? "disabled" : ""}>保存并校验</button><button class="button primary" type="submit" data-plan-intent="deploy" ${agent.status !== "online" || !engineInstalled ? "disabled" : ""}>保存并部署</button></div></footer></form></article>${revisionTimeline}<details class="advanced-studio" id="advanced"><summary><b>全局字段与源码</b><i>＋</i></summary><div class="advanced-studio-body"><nav class="field-rail"><header><b>全局配置项</b><small>${fields.length}</small></header>${fields.map((field) => `<a class="${field.key === selectedField?.key ? "active" : ""}" href="#agent-config" data-config-field="${esc(field.key)}"><i class="${workspace.present_fields[field.key] ? "present" : ""}"></i><span><strong>${esc(field.label)}</strong><code>${esc(field.key)}</code></span><small>${esc(field.kind)}</small></a>`).join("")}</nav><section class="field-canvas"><header><div><h2>${esc(selectedField?.label)}</h2><code>${esc(selectedField?.key)}</code></div><a href="${esc(selectedField?.docs)}" target="_blank" rel="noopener noreferrer">文档 ↗</a></header>${config && selectedField ? `<form id="field-form"><div class="field-mutation"><label>操作<select name="mutation">${fieldValue.present ? '<option value="modify">修改字段</option><option value="delete">删除字段</option>' : '<option value="add">新增字段</option>'}</select></label></div><label>${esc(workspace.catalog.format)} 字段值<textarea name="fragment" spellcheck="false">${esc(fieldValue.fragment)}</textarea></label><footer><div><button class="button" type="submit" data-field-intent="validate">保存并校验</button><button class="button primary" type="submit" data-field-intent="deploy">保存并部署</button></div></footer></form>${sourceStudio}` : '<div class="empty compact"><strong>先创建一个服务端入站</strong></div>'}</section><aside class="official-rail"><header><b>官方文档</b><small>${workspace.catalog.topic_count}</small></header>${workspace.catalog.topic_groups.map((group) => `<details><summary>${esc(group.name)} <b>${group.topics.length}</b></summary><div>${group.topics.map((topic) => `<a href="${esc(topic.docs)}" target="_blank" rel="noopener noreferrer">${esc(topic.label)} ↗</a>`).join("")}</div></details>`).join("")}</aside></div></details>`,
    "节点配置",
  );
  bindAgentConfigPage({
    agent,
    engine,
    workspace,
    protocol,
    plan,
    selectedInbound,
    selectedField,
    fieldValue,
    base,
    engineInstalled,
  });
}

function bindAgentConfigPage(ctx) {
  if (!ctx.engineInstalled)
    document
      .querySelectorAll(
        "#field-form button[type=submit], #source-config-form button[type=submit]",
      )
      .forEach((button) => (button.disabled = true));
  document.querySelectorAll("[data-engine-select]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        state.data.engine = link.dataset.engineSelect;
        state.data.protocol = "";
        state.data.inboundTag = "";
        agentConfig();
      }),
  );
  document.querySelectorAll("[data-inbound]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        const input = ctx.workspace.inbounds.find(
          (item) => item.tag === link.dataset.inbound,
        );
        state.data.inboundTag = input.tag;
        state.data.protocol = input.protocol;
        agentConfig();
      }),
  );
  document.querySelectorAll("[data-protocol]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        state.data.protocol = link.dataset.protocol;
        state.data.inboundTag = "";
        agentConfig();
      }),
  );
  document
    .querySelector("[data-new-inbound]")
    ?.addEventListener("click", () => {
      state.data.inboundTag = "";
      state.data.protocol = ctx.workspace.protocols[0]?.key;
      agentConfig();
    });
  document.querySelectorAll("[data-config-field]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        state.data.configField = link.dataset.configField;
        agentConfig();
      }),
  );
  document.querySelectorAll("[data-secret-visibility]").forEach(
    (button) =>
      (button.onclick = () => {
        const input = button.parentElement.querySelector("input");
        input.type = input.type === "password" ? "text" : "password";
        button.textContent = input.type === "password" ? "显示" : "隐藏";
      }),
  );
  const serverPlanForm = document.querySelector("#server-plan-form");
  if (serverPlanForm) installGeneratedFieldButtons(serverPlanForm);
  const regenerateStatus = serverPlanForm?.querySelector(
    "[data-regenerate-status]",
  );
  const regenerateButtons = [
    ...(serverPlanForm?.querySelectorAll("[data-regenerate]") || []),
  ];
  if (serverPlanForm && regenerateButtons.length && regenerateStatus) {
    bindServerPlanRegeneration({
      form: serverPlanForm,
      buttons: regenerateButtons,
      api,
      base: ctx.base,
      protocol: ctx.protocol,
      report: (message, tone = "success") => {
        regenerateStatus.classList.toggle("error", tone === "error");
        regenerateStatus.setAttribute(
          "role",
          tone === "error" ? "alert" : "status",
        );
        regenerateStatus.textContent = message;
      },
      onApplied: (plan) => {
        state.data.serverPlans[
          `${ctx.agent.id}|${ctx.engine}|${ctx.protocol.key}`
        ] = plan;
      },
    });
  }
  document
    .querySelector("#server-plan-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!ctx.engineInstalled) {
        notify("请先安装当前内核，再提交校验或部署任务", "error");
        return;
      }
      const form = new FormData(event.currentTarget);
      const input = readServerPlanInput(event.currentTarget, ctx.protocol);
      try {
        await api(`${ctx.base}/server-inbounds`, {
          method: "POST",
          body: JSON.stringify({
            operation: form.get("operation"),
            original_tag: ctx.selectedInbound?.tag || "",
            expected_version: ctx.workspace.config?.version || 0,
            name:
              ctx.workspace.config?.name ||
              `${ctx.agent.name} · ${engineName(ctx.engine)}`,
            description: `${ctx.protocol.name} 服务端入站，由 QControlHub 方案生成`,
            intent: event.submitter?.dataset.planIntent || "validate",
            input,
          }),
        });
        state.data.inboundTag = input.tag;
        notify("配置已保存，任务已提交");
        await agentConfig();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  document
    .querySelector("#field-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!ctx.engineInstalled) {
        notify("请先安装当前内核，再提交校验或部署任务", "error");
        return;
      }
      const form = new FormData(event.currentTarget);
      try {
        await api(
          `${ctx.base}/fields/${encodeURIComponent(ctx.selectedField.key)}`,
          {
            method: "POST",
            body: JSON.stringify({
              mutation: form.get("mutation"),
              fragment: form.get("fragment"),
              expected_version: ctx.workspace.config.version,
              name: ctx.workspace.config.name,
              description: ctx.workspace.config.description,
              intent: event.submitter?.dataset.fieldIntent || "validate",
            }),
          },
        );
        notify("字段已保存，任务已提交");
        await agentConfig();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  document
    .querySelector("#source-config-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!ctx.engineInstalled) {
        notify("请先安装当前内核，再提交校验或部署任务", "error");
        return;
      }
      const form = new FormData(event.currentTarget);
      try {
        const saved = await api(`${ctx.base}`, {
          method: "PUT",
          body: JSON.stringify({
            agent_id: ctx.agent.id,
            engine: ctx.engine,
            name: form.get("name"),
            description: form.get("description"),
            content: form.get("content"),
            version: ctx.workspace.config.version,
          }),
        });
        await api("/tasks", {
          method: "POST",
          body: JSON.stringify({
            agent_id: ctx.agent.id,
            engine: ctx.engine,
            action: event.submitter?.dataset.sourceIntent || "validate",
            config_id: saved.id,
          }),
        });
        notify("源码已保存，任务已提交");
        await agentConfig();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  document.querySelectorAll("[data-builder-workbench]").forEach((workbench) => {
    const links = [...workbench.querySelectorAll("[data-builder-step]")];
    const sections = [...workbench.querySelectorAll(".builder-sections > .builder-section")];
    if (!links.length || !sections.length) return;
    const activate = (id) => {
      const selected = sections.find((section) => section.id === id) || sections[0];
      sections.forEach((section) => {
        const active = section === selected;
        section.hidden = !active;
        section.setAttribute("role", "tabpanel");
        section.setAttribute("aria-hidden", active ? "false" : "true");
      });
      links.forEach((link) => {
        const active = link.dataset.builderStep === selected.id;
        link.classList.toggle("active", active);
        link.setAttribute("role", "tab");
        link.setAttribute("aria-selected", active ? "true" : "false");
        link.tabIndex = active ? 0 : -1;
      });
    };
    links.forEach((link) => link.addEventListener("click", (event) => {
      event.preventDefault();
      activate(link.dataset.builderStep);
    }));
    activate(state.data.builderStep || sections[0].id);
  });
}

async function liveConfig() {
  const agents = state.data.agents || (await api("/agents"));
  state.data.agents = agents;
  const eligibleAgents = agents.filter((item) =>
    (item.capabilities || []).some(
      (engine) =>
        item.runtime?.[engine]?.installed ||
        item.runtime?.[engine]?.existing_config_unsupported_reason,
    ),
  );
  if (
    !state.data.liveAgent ||
    !eligibleAgents.some((agent) => agent.id === state.data.liveAgent)
  ) {
    state.data.liveAgent =
      eligibleAgents.find((item) => item.status === "online")?.id ||
      eligibleAgents[0]?.id ||
      "";
  }
  const agent = eligibleAgents.find(
    (item) => item.id === state.data.liveAgent,
  );
  if (!agent) {
    shell(
      '<section class="empty large live-config-empty"><strong>没有可读取的节点配置</strong><p>请先让节点上线并安装支持的内核。</p><a class="button primary" href="#node-settings">前往节点设置</a></section>',
      "手动配置",
    );
    return;
  }
  const installedEngines = (agent.capabilities || []).filter(
    (item) =>
      agent.runtime?.[item]?.installed ||
      agent.runtime?.[item]?.existing_config_unsupported_reason,
  );
  if (
    !state.data.liveEngine ||
    !installedEngines.includes(state.data.liveEngine)
  ) {
    state.data.liveEngine = installedEngines[0];
  }
  const engine = state.data.liveEngine;
  const configWorkspace = await api(
    `/agents/${encodeURIComponent(agent.id)}/configs/${encodeURIComponent(engine)}/workspace`,
  );
  const saved = configWorkspace.config || null;
  const sourceKey = `${agent.id}|${engine}`;
  state.data.liveSources ||= {};
  const source = state.data.liveSources[sourceKey] || null;
  const runtime = agent.runtime?.[engine] || {};
  const unsupportedReason = String(
    runtime.existing_config_unsupported_reason || "",
  );
  const current = !unsupportedReason && source?.content
    ? {
        ...(saved || {
          name: `${agent.name} · ${engineName(engine)}`,
          description: "节点实际配置",
          version: 0,
        }),
        content: source.content,
      }
    : null;
  const language = engine === "mihomo" ? "YAML" : "JSON";
  const existingAvailable = Boolean(runtime.existing_config_available);
  const liveActions = unsupportedReason || !can("operator")
    ? ""
    : existingAvailable
      ? '<button class="button primary" type="submit" data-live-intent="import">手动导入并迁移</button>'
      : '<button class="button" type="submit" data-live-intent="validate">保存并校验</button><button class="button primary" type="submit" data-live-intent="deploy">保存并部署</button>';
  shell(
    `<article class="live-config-workspace"><header class="editor-toolbar"><h2>${esc(agent.name)} · ${esc(engineName(engine))}</h2><div class="editor-toolbar-state"><span class="engine-badge ${esc(engine)}">${esc(engineName(engine))}</span><b>${unsupportedReason ? "不可自动迁移" : saved?.version ? `v${saved.version}` : existingAvailable ? "待导入" : "未保存"}</b></div></header>${current ? `<form class="live-config-editor" id="live-config-form" data-profile-editor data-new-config="0" data-engine="${esc(engine)}"><section class="code-workspace" data-code-editor data-code-language="${language}" data-code-max-bytes="2097152"><header class="code-editor-toolbar"><div class="code-file-meta"><span class="code-file-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M7 3.5h7l4 4V20.5H7zM14 3.5v4h4M10 12h5M10 16h3"/></svg></span><b>${engine === "mihomo" ? "config.yaml" : "config.json"}</b></div><div class="code-editor-meta"><span class="code-language">${language}</span><span data-code-status aria-live="polite">节点快照</span><span data-code-bytes>—</span><span data-code-position>行 1，列 1</span></div></header><div class="code-editor-frame"><aside class="code-gutter" aria-hidden="true" data-line-numbers>1</aside><textarea class="code-editor-input" name="content" data-code-input aria-label="${esc(engineName(engine))} 节点配置源码" spellcheck="false" required ${can("operator") ? "" : "readonly"}>${esc(current.content)}</textarea></div><footer><span><i class="code-status-dot" data-code-status-dot></i><span data-code-validation aria-live="polite"></span></span><div><button class="button code-reset" type="button" data-code-reset disabled>恢复原文</button>${liveActions}</div></footer></section><aside class="live-config-inspector"><dl><div><dt>节点</dt><dd>${esc(agent.name)}</dd></div><div><dt>系统</dt><dd>${esc(agent.os)} / ${esc(agent.arch)}</dd></div><div><dt>内核</dt><dd>${esc(conciseVersion(engine, runtime.version))}</dd></div><div><dt>来源</dt><dd>${existingAvailable ? "待迁移的现有服务快照" : "QAgent 管理配置快照"}</dd></div></dl></aside><input type="hidden" name="name" value="${esc(current.name)}"><input type="hidden" name="description" value="${esc(current.description)}"><input type="hidden" name="version" value="${current.version}"></form>` : agent.status !== "online" ? '<section class="node-config-source"><h2>节点离线</h2><span class="status-label warn">无法读取</span></section>' : unsupportedReason ? `<section class="node-config-source" role="status"><h2>检测到现有服务，但不可自动迁移</h2><span class="status-label bad">${esc(unsupportedReason)}</span><p>QAgent 未执行或接管该服务。请按提示调整为受支持的精确布局并重启 Agent 重新发现。</p></section>` : source?.error ? `<section class="node-config-source"><h2>读取配置失败</h2><span class="status-label bad">${esc(source.error)}</span><button class="button" type="button" data-read-current>重新读取</button></section>` : '<section class="node-config-source" role="status" aria-live="polite"><h2>正在读取配置</h2><span class="status-label warn">读取中</span><form data-auto-read-current hidden></form></section>'}</article>`,
    "手动配置",
  );
  state.data.liveEngines = installedEngines;
  document.querySelectorAll("[data-live-agent]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        state.data.liveAgent = link.dataset.liveAgent;
        state.data.liveEngine = "";
        liveConfig();
      }),
  );
  document.querySelectorAll("[data-live-engine]").forEach(
    (link) =>
      (link.onclick = (event) => {
        event.preventDefault();
        state.data.liveEngine = link.dataset.liveEngine;
        liveConfig();
      }),
  );
  document
    .querySelector("[data-read-current]")
    ?.addEventListener("click", async () => {
      delete state.data.liveSources[sourceKey];
      await readCurrentConfig(agent, engine, sourceKey);
    });
  bindCodeEditors();
  document
    .querySelector("#live-config-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      const intent = event.submitter?.dataset.liveIntent || "validate";
      try {
        if (
          intent === "import" &&
          !(await confirmAction(
            "确定导入当前快照并迁移服务？Agent 将停止并禁用原服务，启动 QAgent 专用服务；迁移任一步失败都会自动恢复原服务。",
            "手动导入并迁移",
          ))
        )
          return;
        if (
          intent === "deploy" &&
          !(await confirmAction(
            "确定保存当前源码、写入节点固定配置并重启服务？",
            "保存并部署",
          ))
        )
          return;
        const saved = await api(
          `/agents/${encodeURIComponent(agent.id)}/configs/${encodeURIComponent(engine)}`,
          {
            method: "PUT",
            body: JSON.stringify({
              agent_id: agent.id,
              engine,
              name: form.get("name"),
              description: form.get("description"),
              content: form.get("content"),
              version: Number(form.get("version")),
            }),
          },
        );
        if (intent === "deploy")
          await submitTask({
            agent_id: agent.id,
            engine,
            action: "deploy",
            config_id: saved.id,
          });
        else if (intent === "validate")
          await submitTask({
            agent_id: agent.id,
            engine,
            action: "validate",
            config_id: saved.id,
          });
        else {
          const task = await api("/tasks", {
            method: "POST",
            body: JSON.stringify({
              agent_id: agent.id,
              engine,
              action: "import-existing",
              config_id: saved.id,
            }),
          });
          if (!task?.id) throw new Error("迁移任务未创建");
          notify("配置已保存，服务迁移任务已提交");
        }
        state.data.liveSources[sourceKey] = {
          ...source,
          content: form.get("content"),
        };
        await liveConfig();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  if (
    !current &&
    !unsupportedReason &&
    agent.status === "online" &&
    !source?.error
  )
    void readCurrentConfig(agent, engine, sourceKey);
}

async function readCurrentConfig(agent, engine, sourceKey) {
  if (state.data.liveSources?.[sourceKey]?.reading) return;
  state.data.liveSources ||= {};
  state.data.liveSources[sourceKey] = { reading: true };
  try {
    const task = await api("/tasks", {
      method: "POST",
      body: JSON.stringify({
        agent_id: agent.id,
        engine,
        action: "read-config",
      }),
    });
    const finished = await waitForTask(task.id);
    if (finished.status !== "succeeded")
      throw new Error(finished.error || "节点未能读取当前配置");
    const snapshot = await api(`/tasks/${encodeURIComponent(finished.id)}/config-snapshot`);
    if (!snapshot.content)
      throw new Error("节点返回的配置快照已失效，请重新读取");
    state.data.liveSources[sourceKey] = {
      content: snapshot.content,
      taskId: finished.id,
      reading: false,
    };
  } catch (error) {
    state.data.liveSources[sourceKey] = {
      error: error.message,
      reading: false,
    };
  }
  if (
    state.route === "live-config" &&
    state.data.liveAgent === agent.id &&
    state.data.liveEngine === engine
  )
    await liveConfig();
}

async function waitForTask(taskID) {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    const task = await api(`/tasks/${encodeURIComponent(taskID)}`);
    if (["succeeded", "failed", "canceled"].includes(task.status)) return task;
    await new Promise((resolve) => setTimeout(resolve, 600));
  }
  throw new Error("等待节点返回配置超时");
}

async function archiveConfigs() {
  const [items, templates, agents] = await Promise.all([
    api("/configs"),
    api("/templates"),
    api("/agents"),
  ]);
  state.data.configs = items;
  state.data.agents = agents;
  const isNew = state.data.newConfig || items.length === 0;
  let selected = items.find((item) => item.id === state.data.archiveConfigId);
  if (!selected && !isNew) selected = items[0];
  const formConfig = selected || {
    id: "",
    name: "新配置",
    description: "",
    engine: "mihomo",
    content:
      "mixed-port: 7890\nallow-lan: false\nmode: rule\nlog-level: info\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n",
    version: 0,
  };
  state.data.archiveConfigId = formConfig.id;
  const revisions = formConfig.id
    ? await api(
        `/configs/${encodeURIComponent(formConfig.id)}/revisions?limit=50`,
      )
    : [];
  let preview = null;
  if (formConfig.id && state.data.revisionVersion) {
    preview = await optionalAPI(
      `/configs/${encodeURIComponent(formConfig.id)}/revisions/${state.data.revisionVersion}`,
    );
  }
  const deployAgents = agents.filter(
    (agent) =>
      agent.status === "online" &&
      (agent.capabilities || []).includes(formConfig.engine) &&
      agent.runtime?.[formConfig.engine]?.installed,
  );
  const templateCards =
    templates
      .map((item) => {
        const eligibleAgents = agents.filter(
          (agent) =>
            agent.status === "online" &&
            (agent.capabilities || []).includes(item.engine) &&
            agent.runtime?.[item.engine]?.installed,
        );
        return `<article class="template-card"><header><span class="engine-badge ${esc(item.engine)}">${esc(engineName(item.engine))}</span><h4>${esc(item.name)}</h4><small>${ago(item.updated_at)}</small></header><pre>${esc(item.content)}</pre>${
            can("operator")
              ? `<footer><form data-template-apply="${esc(item.id)}"><label>应用至<select name="agent_id" required><option value="">${eligibleAgents.length ? "选择在线且已安装内核的节点" : "没有可用节点"}</option>${eligibleAgents
                  .map(
                    (agent) =>
                      `<option value="${esc(agent.id)}">${esc(agent.name)} · 在线 · 已安装</option>`,
                  )
                  .join(
                    "",
                  )}</select></label><button class="button small" type="submit" ${eligibleAgents.length ? "" : "disabled"}>应用</button></form>${can("admin") ? `<button class="button small danger-button" type="button" data-delete-template="${esc(item.id)}">删除</button>` : ""}</footer>`
              : ""
          }</article>`;
      })
      .join("") ||
    '<p class="template-empty">还没有模板。新建模板后可按节点变量生成配置。</p>';
  const revisionTimeline = formConfig.id
    ? `<details class="revision-timeline" ${preview ? "open" : ""}><summary><b>版本历史</b><strong>${revisions.length} 个版本</strong></summary><div class="timeline-body"><nav aria-label="配置修订历史">${revisions.map((revision) => `<button class="${preview?.version === revision.version ? "active" : ""} ${revision.version === formConfig.version ? "current" : ""}" type="button" data-revision="${revision.version}"><i></i><span><b>v${revision.version}</b><strong>${esc(revision.name)}</strong><small>${ago(revision.updated_at)}${revision.version === formConfig.version ? " · 当前" : ""}</small></span></button>`).join("")}</nav>${preview ? `<section class="timeline-preview"><header><div><b>v${preview.version} · ${esc(preview.name)}</b><small>${esc(engineName(preview.engine))} · ${date(preview.updated_at)}</small></div>${preview.version === formConfig.version ? '<span class="status-label ok">当前版本</span>' : ""}</header><textarea readonly>${esc(preview.content)}</textarea>${can("admin") && preview.version !== formConfig.version ? `<button class="button" type="button" data-restore-revision="${preview.version}">以此版本创建新版本</button>` : ""}</section>` : '<div class="timeline-placeholder">选择版本</div>'}</div></details>`
    : "";
  const delivery = formConfig.id
    ? `<section class="delivery-bar"><div><span class="delivery-icon"><svg viewBox="0 0 24 24"><path d="M13 2.5 5.5 13H11l-1 8.5L18.5 11H13z"/></svg></span><h3>校验或部署</h3></div><form id="archive-delivery"><label>目标节点<select name="agent_id" required><option value="">${deployAgents.length ? `选择在线且已安装 ${esc(engineName(formConfig.engine))} 的节点` : `没有在线且已安装 ${esc(engineName(formConfig.engine))} 的节点`}</option>${deployAgents.map((agent) => `<option value="${esc(agent.id)}">${esc(agent.name)} · 在线 · 已安装</option>`).join("")}</select></label><label>执行方式<select name="action"><option value="validate">仅校验，不写入</option><option value="deploy">部署并重启</option></select></label><button class="button primary" type="submit" ${!deployAgents.length || !can("operator") ? "disabled" : ""}>提交任务</button></form></section>`
    : "";
  shell(
    `<article class="config-workspace"><header class="editor-toolbar"><h2>${esc(formConfig.name)}</h2><div class="editor-toolbar-state"><span class="engine-badge ${esc(formConfig.engine)}">${esc(engineName(formConfig.engine))}</span><b>${isNew ? "草稿" : `v${formConfig.version}`}</b></div></header><form class="config-editor-grid" id="archive-form" data-profile-editor data-new-config="${isNew ? 1 : 0}" data-engine="${esc(formConfig.engine)}"><section class="code-workspace" data-code-editor data-code-language="${formConfig.engine === "mihomo" ? "YAML" : "JSON"}" data-code-max-bytes="2097152"><header class="code-editor-toolbar"><div class="code-file-meta"><span class="code-file-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M7 3.5h7l4 4V20.5H7zM14 3.5v4h4M10 12h5M10 16h3"/></svg></span><b>${formConfig.engine === "mihomo" ? "config.yaml" : "config.json"}</b></div><div class="code-editor-meta"><span class="code-language">${formConfig.engine === "mihomo" ? "YAML" : "JSON"}</span><span data-code-status aria-live="polite">${isNew ? "草稿" : "已保存"}</span><span data-code-bytes>—</span><span data-code-position>行 1，列 1</span></div></header><div class="code-editor-frame"><aside class="code-gutter" aria-hidden="true" data-line-numbers>1</aside><textarea class="code-editor-input" name="content" data-code-input aria-label="${esc(engineName(formConfig.engine))} 配置档案源码" spellcheck="false" required ${can("operator") ? "" : "readonly"}>${esc(formConfig.content)}</textarea></div><footer><span><i class="code-status-dot" data-code-status-dot></i><span data-code-validation aria-live="polite"></span></span><div><button class="button code-reset" type="button" data-code-reset data-archive-reset disabled>恢复原文</button>${can("operator") ? `<button class="button primary" type="submit">${isNew ? "创建配置档案" : "保存新版本"}</button>` : ""}</div></footer></section><aside class="config-inspector"><header><h3>属性</h3></header><label>名称<input name="name" maxlength="100" required value="${esc(formConfig.name)}" ${can("operator") ? "" : "readonly"}></label><label>内核<select name="engine" ${isNew && can("operator") ? "" : "disabled"}>${engines.map((engine) => `<option value="${engine}" ${engine === formConfig.engine ? "selected" : ""}>${esc(engineName(engine))} · ${engine === "mihomo" ? "YAML" : "JSON"}</option>`).join("")}</select></label><label>说明<textarea class="description-input" name="description" maxlength="300" placeholder="填写用途、节点或变更说明" ${can("operator") ? "" : "readonly"}>${esc(formConfig.description || "")}</textarea></label></aside></form>${delivery}${revisionTimeline}${can("admin") && formConfig.id ? '<footer class="config-danger"><span><b>删除配置档案</b><small>相关任务记录会保留，配置档案删除后无法恢复。</small></span><button type="button" data-remove="' + esc(formConfig.id) + '">删除配置</button></footer>' : ""}</article><section class="template-workspace" id="templates"><header class="template-head"><h3>配置模板</h3><span>用 {{node_name}}、{{node_id}}、{{lan_ip}}、{{random_port}} 占位符，按节点批量生成配置。</span></header>${can("operator") ? '<details class="template-create" ' + (!templates.length ? "open" : "") + '><summary><b>＋ 新建模板</b></summary><form id="template-form"><label>模板名称<input name="name" maxlength="100" required></label><label>内核<select name="engine">' + engines.map((engine) => `<option value="${engine}">${esc(engineName(engine))}</option>`).join("") + '</select></label><label class="template-content-field">模板正文<textarea name="content" spellcheck="false" required></textarea></label><button class="button primary" type="submit">保存模板</button></form></details>' : ""}<div class="template-grid">${templateCards}</div></section>`,
    "配置档案",
  );
  document.querySelectorAll("[data-archive-config]").forEach((link) => {
    link.onclick = (event) => {
      event.preventDefault();
      state.data.newConfig = false;
      state.data.archiveConfigId = link.dataset.archiveConfig;
      state.data.revisionVersion = 0;
      archiveConfigs();
    };
  });
  document.querySelector("#new-config")?.addEventListener("click", () => {
    state.data.newConfig = true;
    state.data.archiveConfigId = "";
    state.data.revisionVersion = 0;
    archiveConfigs();
  });
  bindCodeEditors();
  document
    .querySelector("#archive-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      const payload = {
        name: form.get("name"),
        description: form.get("description"),
        engine: form.get("engine") || formConfig.engine,
        content: form.get("content"),
        version: formConfig.version,
      };
      try {
        const saved = await api(
          formConfig.id
            ? `/configs/${encodeURIComponent(formConfig.id)}`
            : "/configs",
          {
            method: formConfig.id ? "PUT" : "POST",
            body: JSON.stringify(payload),
          },
        );
        state.data.newConfig = false;
        state.data.archiveConfigId = saved.id;
        await archiveConfigs();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  document
    .querySelector("#archive-delivery")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      if (
        form.get("action") === "deploy" &&
        !(await confirmAction(
          "确定将当前配置部署到所选节点并重启对应服务？",
          "部署并重启",
        ))
      )
        return;
      await submitTask({
        agent_id: form.get("agent_id"),
        engine: formConfig.engine,
        action: form.get("action"),
        config_id: formConfig.id,
      });
      location.hash = "#tasks";
    });
  document.querySelectorAll("[data-revision]").forEach(
    (button) =>
      (button.onclick = () => {
        state.data.revisionVersion = Number(button.dataset.revision);
        archiveConfigs();
      }),
  );
  document
    .querySelector("[data-restore-revision]")
    ?.addEventListener("click", async (event) => {
      if (!(await confirmAction(
        `确定以 v${event.currentTarget.dataset.restoreRevision} 的内容创建新版本？`,
        "创建新版本",
      )))
        return;
      await api(
        `/configs/${encodeURIComponent(formConfig.id)}/revisions/${event.currentTarget.dataset.restoreRevision}/restore`,
        {
          method: "POST",
          body: JSON.stringify({ expected_version: formConfig.version }),
        },
      );
      state.data.revisionVersion = 0;
      await archiveConfigs();
    });
  document.querySelectorAll("[data-remove]").forEach(
    (button) =>
      (button.onclick = async () => {
        if (!(await confirmAction("确认删除配置？", "删除配置"))) return;
        try {
          await api(`/configs/${button.dataset.remove}`, { method: "DELETE" });
          state.data.archiveConfigId = "";
          state.data.newConfig = false;
          archiveConfigs();
        } catch (error) {
          notify(error.message, "error");
        }
      }),
  );
  document
    .querySelector("#template-form")
    ?.addEventListener("submit", async (event) => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      try {
        await api("/templates", {
          method: "POST",
          body: JSON.stringify({
            name: form.get("name"),
            engine: form.get("engine"),
            content: form.get("content"),
          }),
        });
        archiveConfigs();
      } catch (error) {
        notify(error.message, "error");
      }
    });
  document.querySelectorAll("[data-delete-template]").forEach(
    (button) =>
      (button.onclick = async () => {
        if (!(await confirmAction("确认删除模板？", "删除模板"))) return;
        try {
          await api(`/templates/${button.dataset.deleteTemplate}`, {
            method: "DELETE",
          });
          archiveConfigs();
        } catch (error) {
          notify(error.message, "error");
        }
      }),
  );
  document.querySelectorAll("[data-template-apply]").forEach(
    (form) =>
      (form.onsubmit = async (event) => {
        event.preventDefault();
        const agentID = new FormData(form).get("agent_id");
        try {
          await api(`/templates/${form.dataset.templateApply}/apply`, {
            method: "POST",
            body: JSON.stringify({ agent_id: agentID }),
          });
          notify("模板已应用");
        } catch (error) {
          notify(error.message, "error");
        }
      }),
  );
}
  return { agentConfig, liveConfig, archiveConfigs };
}
