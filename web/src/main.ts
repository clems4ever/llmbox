// The llmbox admin dashboard: a single-page app over the box-control API
// (/api/v1) — the same authenticated API llmbox-mcp and scripts use, driven
// here with the login cookie + CSRF header instead of a bearer key.
//
// Boot: /api/v1/me turns the login cookie into an API session (email, admin,
// CSRF token). 401 bounces to the server's sign-in page; a non-admin session
// renders a notice; an admin session renders the dashboard.
import "./style.css";
import { Api, ApiError, me } from "./api";
import type { BoxView, JoinTokenInfo, Me, ProxyInfo, SpokeEnrollment, SpokeStatus } from "./api";

/** h builds a DOM element with attributes and children — all data lands via
 * textContent/attributes, never markup, so API strings cannot inject HTML. */
function h(
  tag: string,
  attrs: Record<string, string> = {},
  ...children: (Node | string)[]
): HTMLElement {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  el.append(...children);
  return el;
}

const app = document.getElementById("app")!;

/** brand renders the header line, with the signed-in identity when known. */
function brand(email?: string): HTMLElement {
  const logo = h("img", { class: "logo", src: "/favicon.svg", width: "27", height: "27", alt: "" });
  const line = h("div", { class: "brand" }, logo, h("b", {}, "llmbox"));
  if (email) line.append(h("span", { class: "who" }, `Signed in as ${email}`));
  return line;
}

/** flash shows (or clears) the transient ok/error banner. */
function flash(kind?: "ok" | "err", msg?: string): void {
  document.getElementById("flash")?.remove();
  if (!kind || !msg) return;
  const banner = h("div", { class: `banner ${kind}`, id: "flash" }, msg);
  app.querySelector("h1")?.after(banner);
}

/** run wraps an action handler: it disables the triggering button, reports
 * failure in the flash banner, and refreshes the dashboard data on success. */
function run(btn: HTMLButtonElement | null, action: () => Promise<void>): void {
  if (btn) btn.disabled = true;
  action()
    .then(() => refresh())
    .catch((err: unknown) => {
      if (err instanceof ApiError && err.status === 401) {
        signIn(); // session expired mid-action
        return;
      }
      flash("err", err instanceof Error ? err.message : String(err));
    })
    .finally(() => {
      if (btn) btn.disabled = false;
    });
}

/** signIn bounces to the server-rendered sign-in page, returning here after. */
function signIn(): void {
  window.location.href = "/signin?return=/admin";
}

// ---- views ------------------------------------------------------------------

function renderNotAdmin(session: Me): void {
  app.replaceChildren(
    brand(session.email),
    h("h1", {}, "Cluster admin"),
    h("div", { class: "card" }, h("p", { class: "empty" }, `${session.email} isn't an administrator on this hub. Ask an operator to add it to auth.admin.emails.`)),
  );
}

interface DashboardData {
  spokes: SpokeStatus[];
  tokens: JoinTokenInfo[];
  boxes: BoxView[];
  proxyEnabled: boolean;
  proxies: ProxyInfo[];
}

let api: Api;
let session: Me;

/** refresh reloads every dashboard section from the API and re-renders. */
async function refresh(): Promise<void> {
  const [spokes, tokens, boxes, proxyEnabled] = await Promise.all([
    api.spokeStatuses(),
    api.listJoinTokens(),
    api.listBoxes(),
    api.proxyEnabled(),
  ]);
  const proxies = proxyEnabled ? await api.listProxies() : [];
  renderDashboard({ spokes, tokens, boxes, proxyEnabled, proxies });
}

function renderDashboard(data: DashboardData): void {
  const flashEl = document.getElementById("flash");
  const resultEl = document.getElementById("result");
  app.replaceChildren(
    brand(session.email),
    h("h1", {}, "Cluster admin"),
    ...(flashEl ? [flashEl] : []),
    ...(resultEl ? [resultEl] : []),
    spokesCard(data),
    boxesCard(data),
    ...(data.proxyEnabled ? [proxiesCard(data)] : []),
  );
}

/** result shows a one-time create result (spoke command, box auth link, proxy
 * URL) above the cards, surviving the next refresh only. */
function result(...children: (Node | string)[]): void {
  document.getElementById("result")?.remove();
  const box = h("div", { class: "result", id: "result" }, ...children);
  (document.getElementById("flash") ?? app.querySelector("h1"))!.after(box);
}

// ---- spokes -----------------------------------------------------------------

function spokesCard(data: DashboardData): HTMLElement {
  const card = h("div", { class: "card", id: "spokes-card" }, h("h2", {}, "Spokes"));

  if (data.spokes.length === 0) {
    card.append(h("p", { class: "empty" }, "No spokes enrolled yet. Create one below — it prints the command to run on the spoke host."));
  } else {
    const rows = data.spokes.map((sp) => {
      const name = h("td", { class: "mono" }, sp.name);
      if (sp.default) name.append(" ", h("span", { class: "pill on" }, "default"));
      const status = sp.connected
        ? h("span", { class: "pill on" }, "connected")
        : h("span", { class: "pill off" }, "offline");
      const actions = h("td", {});
      if (!sp.default) {
        const mk = h("button", { class: "quiet" }, "Make default") as HTMLButtonElement;
        mk.onclick = () => run(mk, () => api.setDefaultSpoke(sp.name).then(() => flash("ok", `default spoke is now ${sp.name}`)));
        actions.append(mk, " ");
      }
      const drop = h("button", { class: "danger", "data-spoke": sp.name }, "Drop") as HTMLButtonElement;
      drop.onclick = () => {
        if (!confirm(`Drop spoke ${sp.name} and kick its connection?`)) return;
        run(drop, () => api.dropSpoke(sp.name).then(() => flash("ok", `dropped spoke ${sp.name}`)));
      };
      actions.append(drop);
      return h("tr", {}, name, h("td", {}, status), h("td", { class: "mono" }, sp.enrolled_at ? sp.enrolled_at.slice(0, 16).replace("T", " ") : ""), actions);
    });
    card.append(h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Status"), h("th", {}, "Enrolled"), h("th", {}, ""))),
      h("tbody", {}, ...rows),
    ));
  }

  if (data.tokens.length > 0) {
    card.append(h("h3", {}, "Outstanding join tokens"));
    const rows = data.tokens.map((tok) => {
      const revoke = h("button", { class: "danger" }, "Revoke") as HTMLButtonElement;
      revoke.onclick = () => run(revoke, () => api.revokeJoinToken(tok.id).then(() => flash("ok", "revoked join token")));
      const expired = new Date(tok.expires_at).getTime() < Date.now();
      const expires = h("td", { class: "mono" }, tok.expires_at.slice(0, 16).replace("T", " "));
      if (expired) expires.append(" ", h("span", { class: "pill off" }, "expired"));
      return h("tr", {}, h("td", { class: "mono" }, tok.id.slice(0, 12)), h("td", { class: "mono" }, tok.name), expires, h("td", {}, revoke));
    });
    card.append(h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "ID"), h("th", {}, "Spoke"), h("th", {}, "Expires"), h("th", {}, ""))),
      h("tbody", {}, ...rows),
    ));
  }

  // Create-spoke form.
  const name = h("input", { type: "text", name: "name", placeholder: "edge-1", autocomplete: "off" }) as HTMLInputElement;
  const backend = h("select", { name: "backend" },
    h("option", { value: "docker" }, "docker"),
    h("option", { value: "firecracker" }, "firecracker"),
  ) as HTMLSelectElement;
  const ttl = h("input", { type: "text", name: "ttl", placeholder: "1h", autocomplete: "off" }) as HTMLInputElement;
  const submit = h("button", { type: "submit" }, "Create spoke") as HTMLButtonElement;
  const form = h("form", { class: "row", id: "create-spoke-form" },
    h("div", { class: "field" }, h("label", {}, "Name"), name),
    h("div", { class: "field" }, h("label", {}, "Backend"), backend),
    h("div", { class: "field" }, h("label", {}, "Token TTL (optional)"), ttl),
    submit,
  );
  form.onsubmit = (e) => {
    e.preventDefault();
    run(submit, async () => {
      const sp = await api.createSpoke(name.value.trim(), backend.value, ttl.value.trim());
      showSpokeResult(sp);
      flash();
    });
  };
  card.append(form);
  return card;
}

function showSpokeResult(sp: SpokeEnrollment): void {
  result(
    h("div", { class: "label" }, `spoke ${sp.name} — run this on the spoke host (the token is shown only once)`),
    h("pre", { class: "cmd" }, sp.command),
    h("p", { class: "note" }, "After first enrollment the spoke reconnects from its saved credential; the token is one-time."),
  );
}

// ---- boxes ------------------------------------------------------------------

function boxesCard(data: DashboardData): HTMLElement {
  const card = h("div", { class: "card", id: "boxes-card" }, h("h2", {}, "Boxes"));

  if (data.boxes.length === 0) {
    card.append(h("p", { class: "empty" }, "No boxes running."));
  } else {
    const rows = data.boxes.map((b) => {
      const id = b.box_id || b.name;
      const phase = b.phase === "ready"
        ? h("span", { class: "pill on" }, "ready")
        : h("span", { class: "pill muted" }, b.phase);
      const link = h("td", {});
      if (b.auth_url) link.append(h("a", { href: b.auth_url, target: "_blank", rel: "noopener" }, "Activate"));
      else if (b.session_url) link.append(h("a", { href: b.session_url, target: "_blank", rel: "noopener" }, "Open"));
      else link.append(h("span", { class: "empty" }, "—"));
      const destroy = h("button", { class: "danger", "data-box": id }, "Remove") as HTMLButtonElement;
      destroy.onclick = () => {
        if (!confirm(`Remove box ${id}?`)) return;
        run(destroy, () => api.destroyBox(id).then(() => flash("ok", `removed box ${id}`)));
      };
      return h("tr", {},
        h("td", { class: "mono" }, id),
        h("td", { class: "mono" }, b.spoke ?? ""),
        h("td", { class: "mono" }, b.image),
        h("td", {}, b.state),
        h("td", {}, phase),
        link,
        h("td", {}, destroy),
      );
    });
    card.append(h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Box ID"), h("th", {}, "Spoke"), h("th", {}, "Image"), h("th", {}, "State"), h("th", {}, "Phase"), h("th", {}, "Link"), h("th", {}, ""))),
      h("tbody", {}, ...rows),
    ));
  }

  // Create-box form.
  const boxId = h("input", { type: "text", name: "box_id", placeholder: "refactor-auth", autocomplete: "off", required: "" }) as HTMLInputElement;
  const desc = h("input", { type: "text", name: "description", autocomplete: "off" }) as HTMLInputElement;
  const spoke = h("select", { name: "spoke" }, h("option", { value: "" }, defaultSpokeLabel(data.spokes))) as HTMLSelectElement;
  for (const sp of data.spokes) {
    if (sp.connected) spoke.append(h("option", { value: sp.name }, sp.name));
  }
  const submit = h("button", { type: "submit" }, "Create box") as HTMLButtonElement;
  const form = h("form", { class: "row", id: "create-box-form" },
    h("div", { class: "field" }, h("label", {}, "Box ID"), boxId),
    h("div", { class: "field" }, h("label", {}, "Description (optional)"), desc),
    h("div", { class: "field" }, h("label", {}, "Spoke"), spoke),
    submit,
  );
  form.onsubmit = (e) => {
    e.preventDefault();
    run(submit, async () => {
      const created = await api.createBox(boxId.value.trim(), desc.value.trim(), spoke.value);
      const url = await api.authPageURL(created.token);
      result(
        h("div", { class: "label" }, `box ${created.box_id} — activation link`),
        h("div", { class: "val" }, h("a", { href: url, target: "_blank", rel: "noopener" }, url)),
        h("p", { class: "note" }, "Open the link to authenticate the box; it stays pending until activated."),
      );
      flash();
    });
  };
  card.append(form);
  return card;
}

function defaultSpokeLabel(spokes: SpokeStatus[]): string {
  const def = spokes.find((sp) => sp.default);
  return def ? `default (${def.name})` : "default";
}

// ---- proxies ----------------------------------------------------------------

function proxiesCard(data: DashboardData): HTMLElement {
  const card = h("div", { class: "card", id: "proxies-card" }, h("h2", {}, "HTTP proxies"));

  if (data.proxies.length === 0) {
    card.append(h("p", { class: "empty" }, "No proxies enabled."));
  } else {
    const rows = data.proxies.map((p) => {
      const del = h("button", { class: "danger", "data-proxy": `${p.box_id}:${p.port}` }, "Remove") as HTMLButtonElement;
      del.onclick = () => {
        if (!confirm(`Remove proxy for ${p.box_id}:${p.port}?`)) return;
        run(del, () => api.deleteProxy(p.box_id, p.port).then(() => flash("ok", `removed proxy ${p.box_id}:${p.port}`)));
      };
      return h("tr", {},
        h("td", { class: "mono" }, p.box_id),
        h("td", { class: "mono" }, String(p.port)),
        h("td", {}, h("a", { href: p.url, target: "_blank", rel: "noopener" }, p.url)),
        h("td", {}, p.description ?? ""),
        h("td", {}, del),
      );
    });
    card.append(h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Box ID"), h("th", {}, "Port"), h("th", {}, "URL"), h("th", {}, "Description"), h("th", {}, ""))),
      h("tbody", {}, ...rows),
    ));
  }

  const boxId = h("input", { type: "text", name: "box_id", placeholder: "refactor-auth", autocomplete: "off", required: "" }) as HTMLInputElement;
  const port = h("input", { type: "text", name: "port", placeholder: "8000", autocomplete: "off", required: "" }) as HTMLInputElement;
  const desc = h("input", { type: "text", name: "description", autocomplete: "off" }) as HTMLInputElement;
  const submit = h("button", { type: "submit" }, "Create proxy") as HTMLButtonElement;
  const form = h("form", { class: "row", id: "create-proxy-form" },
    h("div", { class: "field" }, h("label", {}, "Box ID"), boxId),
    h("div", { class: "field" }, h("label", {}, "Port"), port),
    h("div", { class: "field" }, h("label", {}, "Description (optional)"), desc),
    submit,
  );
  form.onsubmit = (e) => {
    e.preventDefault();
    run(submit, async () => {
      const p = await api.createProxy(boxId.value.trim(), parseInt(port.value, 10) || 0, desc.value.trim());
      result(
        h("div", { class: "label" }, `proxy for ${p.box_id}:${p.port}`),
        h("div", { class: "val" }, h("a", { href: p.url, target: "_blank", rel: "noopener" }, p.url)),
      );
      flash();
    });
  };
  card.append(form);
  return card;
}

// ---- boot -------------------------------------------------------------------

async function boot(): Promise<void> {
  try {
    session = await me();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      signIn();
      return;
    }
    app.replaceChildren(brand(), h("div", { class: "banner err" }, err instanceof Error ? err.message : String(err)));
    return;
  }
  if (!session.admin) {
    renderNotAdmin(session);
    return;
  }
  api = new Api(session.csrf);
  try {
    await refresh();
  } catch (err) {
    app.replaceChildren(brand(session.email), h("div", { class: "banner err" }, err instanceof Error ? err.message : String(err)));
  }
}

void boot();
