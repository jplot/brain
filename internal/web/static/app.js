"use strict";

const TOKEN = new URLSearchParams(location.search).get("token") || "";
const PAGE_SIZE = 20;
const SEARCH_DELAY = 250;
const LOCALE = "en-GB";
const ROLES = { user: "you", claude: "Claude", tool: "tool", subagent: "subagent" };
const VIEWS = [
  ["fil", "Feed"], ["souvenirs", "Memories"], ["recherche", "Search"],
  ["memo", "Memo"], ["sujets", "Subjects"],
];
const KIND_TABS = [["all", "All"], ["decision", "Decisions"], ["pending", "Pending"], ["fact", "Facts about you"]];
const KINDS = { topic: "Topic", decision: "Decision", pending: "Pending", fact: "Fact about you" };
const LINKS = { depends_on: "depends on", about: "concerns" };
const LEVELS = [1, 2, 3, 4, 5];
const MEMORY_PAGE = 50;

const state = {
  view: "fil", subject: "", query: "", notice: "", kind: "all", focus: null, memoSubject: "", counts: null,
  tree: [], subjects: [],
};
const memoryUi = { editing: {}, draft: {} };
const subjectUi = { open: {}, draft: {} };
let epoch = 0;

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === "class") el.className = value;
    else if (key === "checked" || key === "value") el[key] = value;
    else if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child !== null && child !== undefined && child !== false) el.append(child);
  }
  return el;
}

async function call(path, options) {
  let response;
  try {
    response = await fetch(path, options);
  } catch {
    throw new Error("The server is not responding.");
  }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw Object.assign(new Error(body.error || "request refused"), { status: response.status });
  return body;
}

function get(path, params) {
  const query = new URLSearchParams({ ...params, token: TOKEN });
  return call(`${path}?${query}`);
}

function post(path, body) {
  return call(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Paul-Token": TOKEN },
    body: JSON.stringify(body),
  });
}

function replaceKeepingFocus(parent, ...nodes) {
  const focused = document.activeElement && document.activeElement.dataset.key;
  parent.replaceChildren(...nodes);
  const again = focused && [...parent.querySelectorAll("[data-key]")].find((n) => n.dataset.key === focused);
  if (again) again.focus();
}

function mount(...nodes) {
  replaceKeepingFocus(document.getElementById("main"), ...nodes);
}

function alertBox(message) {
  const box = h("p", { class: "alert", role: "alert" }, message);
  box.hidden = !message;
  return box;
}

function say(box, message) {
  box.textContent = message;
  box.hidden = !message;
}

function heading(title, lede) {
  return [h("h1", {}, title), h("p", { class: "lede" }, lede)];
}

function capitalize(text) {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function dayKey(date) {
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
}

function dayLabel(date) {
  return capitalize(date.toLocaleDateString(LOCALE, { weekday: "long", day: "numeric", month: "long" }));
}

function clock(date) {
  return date.toLocaleTimeString(LOCALE, { hour: "2-digit", minute: "2-digit" });
}

function plural(n, word, many = `${word}s`) {
  return `${n} ${n > 1 ? many : word}`;
}

function pillRow(items, current, pick, prefix) {
  return h("div", { class: "pills" }, items.map(([value, text]) => h("button", {
    type: "button", class: "pill", "aria-pressed": String(current === value), "data-key": `${prefix}-${value}`,
    onclick: () => pick(value),
  }, text)));
}

function flatten(nodes, depth, out) {
  for (const node of nodes) {
    out.push({ uid: node.uid, name: node.name, depth });
    flatten(node.children, depth + 1, out);
  }
  return out;
}

async function loadSubjects() {
  state.tree = await get("/api/subjects");
  state.subjects = flatten(state.tree, 0, []);
  if (!state.subjects.some((s) => s.uid === state.subject)) state.subject = "";
  if (!state.subjects.some((s) => s.uid === state.memoSubject)) state.memoSubject = "";
}

function subjectOptions(subjects, current, first) {
  return [
    first === null ? null : h("option", { value: "", selected: current === "" }, first),
    ...subjects.map((s) => h("option", { value: s.uid, selected: s.uid === current },
      "  ".repeat(s.depth) + s.name)),
  ];
}

function subjectFilter() {
  return h("label", { class: "field" }, "Subject", h("select", {
    "data-key": "subject-filter",
    onchange: (event) => { state.subject = event.target.value; state.focus = null; show(); },
  }, subjectOptions(state.subjects, state.subject, "All subjects")));
}

async function refreshCounts() {
  try {
    state.counts = await get("/api/counts");
  } catch {
    state.counts = null;
  }
  renderMenu();
}

function renderMenu() {
  const menu = document.getElementById("menu");
  const counted = state.counts
    ? { fil: state.counts.sessions, souvenirs: state.counts.points, sujets: state.counts.subjects } : {};
  menu.replaceChildren(...VIEWS.map(([view, text]) => h("button", {
    type: "button", "aria-current": state.view === view ? "page" : "false",
    onclick: () => { state.view = view; state.focus = null; show(); },
  }, h("span", {}, text), h("span", { class: "count" }, String(counted[view] ?? "")))));
}

function show() {
  epoch += 1;
  renderMenu();
  const views = {
    fil: showFil, souvenirs: showMemories, recherche: showSearch, memo: showMemo, sujets: showSubjects,
  };
  views[state.view](epoch);
}

function messageRow(message) {
  const tool = message.role === "tool";
  const when = clock(new Date(message.ts));
  return h("div", { class: "message" },
    h("span", { class: "message-meta" }, `${when} · ${ROLES[message.role] || message.role}`),
    h("span", { class: tool ? "message-text tool" : "message-text" }, message.text));
}

function forgetControl(askLabel, question, run) {
  const box = h("div", { class: "actions" });
  const alert = alertBox("");
  function ask() {
    box.replaceChildren(
      h("span", { class: "confirm" }, question),
      h("button", { type: "button", class: "btn strong", onclick: confirm }, "Forget"),
      h("button", { type: "button", class: "btn", onclick: idle }, "Cancel"));
  }
  function idle() {
    box.replaceChildren(h("button", { type: "button", class: "btn", onclick: ask }, askLabel));
  }
  async function confirm(event) {
    if (event.detail > 1) return;
    try {
      await run();
    } catch (error) {
      say(alert, `Forgetting refused: ${error.message}`);
    }
  }
  idle();
  return h("div", { class: "forget" }, box, alert);
}

function countsText(row) {
  const parts = [];
  if (row.decisions) parts.push(plural(row.decisions, "decision"));
  if (row.pending) parts.push(`${row.pending} pending ${row.pending > 1 ? "points" : "point"}`);
  return parts.length ? parts.join(", ") : plural(row.n_messages, "message");
}

function sessionCard(row, startOpen, gone) {
  const ended = new Date(row.ended_at);
  let body = null;
  const toggle = h("button", { type: "button", class: "session-toggle", "aria-expanded": "false" },
    h("span", { class: "line" },
      h("span", { class: "time" }, clock(ended)),
      h("span", { class: "title" }, row.episode_title ?? row.title ?? "(untitled)"),
      row.subjects.length ? h("span", { class: "subjects" }, row.subjects.join(", ")) : null),
    row.summary ? h("span", { class: "summary" }, row.summary) : null,
    h("span", { class: "sub" }, countsText(row)));
  const card = h("article", { class: "card" }, toggle);
  async function open() {
    const mine = body = h("div", { class: "messages" }, h("span", { class: "sub" }, "Loading…"));
    card.append(mine);
    toggle.setAttribute("aria-expanded", "true");
    try {
      const session = await get(`/api/sessions/${encodeURIComponent(row.id)}`);
      mine.replaceChildren(...session.messages.map(messageRow), forgetControl(
        "Forget this session", "Forget this whole session? This is permanent.", async () => {
          try {
            await post("/api/sessions/forget", { id: row.id });
          } catch (error) {
            if (error.status !== 404) throw error;
          }
          if (state.focus === row.id) state.focus = null;
          memoryUi.editing = {};
          memoryUi.draft = {};
          gone(card);
          refreshCounts();
        }));
    } catch {
      mine.replaceChildren(h("span", { class: "alert", role: "alert" }, "Could not load the session."));
    }
  }
  toggle.addEventListener("click", () => {
    if (body) {
      body.remove();
      body = null;
      toggle.setAttribute("aria-expanded", "false");
    } else {
      open();
    }
  });
  if (startOpen) open();
  return card;
}

function showFil(mine) {
  const alert = alertBox(state.notice);
  const days = h("div", { class: "days" });
  const more = h("div", { class: "more" });
  const focusAlert = alertBox("");
  const pinned = h("div", { class: "pinned" });
  mount(...heading("Feed", "Your sessions by date, with the subjects they talk about."), subjectFilter(), alert,
    focusAlert, pinned, days, more);

  function drop(card) {
    const list = card.parentElement;
    card.remove();
    if (list.classList.contains("stack") && !list.children.length) {
      list.previousElementSibling.remove();
      list.remove();
      if (current && current.list === list) current = null;
    }
    if (!days.children.length && !pinned.children.length) days.append(h("p", { class: "empty" }, "No sessions."));
  }
  let before = null;
  let current = null;
  let loading = false;

  async function load() {
    if (loading) return;
    loading = true;
    try {
      await loadPage();
    } finally {
      loading = false;
    }
  }

  async function loadPage() {
    const params = { limit: PAGE_SIZE };
    if (state.subject) params.subject = state.subject;
    if (before) params.before = before;
    let rows;
    try {
      rows = await get("/api/sessions", params);
    } catch {
      if (mine === epoch) say(alert, "Could not load the sessions.");
      return;
    }
    if (mine !== epoch) return;
    say(alert, "");
    for (const row of rows) {
      if (row.id === state.focus) continue;
      const key = dayKey(new Date(row.ended_at));
      if (!current || current.key !== key) {
        const list = h("div", { class: "stack" });
        days.append(h("h2", {}, dayLabel(new Date(row.ended_at))), list);
        current = { key, list };
      }
      current.list.append(sessionCard(row, false, drop));
    }
    if (!before && rows.length === 0) days.append(h("p", { class: "empty" }, "No sessions."));
    if (rows.length) before = rows[rows.length - 1].ended_at;
    more.replaceChildren(...(rows.length >= PAGE_SIZE ? [
      h("button", { type: "button", class: "btn", "data-key": "more", onclick: load }, "Show more"),
    ] : []));
  }
  async function showFocus() {
    try {
      const session = await get(`/api/sessions/${encodeURIComponent(state.focus)}`);
      if (mine === epoch) pinned.append(sessionCard(session, true, drop));
    } catch (error) {
      state.focus = null;
      if (mine === epoch) {
        say(focusAlert, error.status === 404 ? "This conversation no longer exists." : "Could not load the conversation.");
      }
    }
  }
  if (state.focus) showFocus();
  load();
}

function memoryDate(point) {
  return new Date(point.source_ts).toLocaleDateString(LOCALE, { day: "numeric", month: "short" });
}

function pointCard(point, refresh, gone) {
  const editing = !!memoryUi.editing[point.id];
  const closed = point.closed_at !== null;
  const status = closed ? "Closed" : point.corrected ? "Corrected by you" : "";
  const levels = LEVELS.map((n) => h("button", {
    type: "button", class: "pill level", "aria-pressed": String(point.importance === n),
    "aria-label": `Importance ${n}`, "data-key": `level-${point.id}-${n}`,
    onclick: () => (n === point.importance ? null
      : change(() => post("/api/points/correct", { id: point.id, importance: n }), refresh)),
  }, String(n)));
  const text = editing
    ? h("label", { class: "edit-field" }, "Memory text", h("input", {
      type: "text", value: memoryUi.draft[point.id] ?? point.text, "data-key": `text-${point.id}`,
      oninput: (event) => { memoryUi.draft[point.id] = event.target.value; },
    }))
    : h("div", {}, point.text);
  const edit = h("button", {
    type: "button", class: "btn", "data-key": `edit-${point.id}`,
    onclick: () => (editing ? change(async () => {
      const typed = memoryUi.draft[point.id] ?? point.text;
      if (typed !== point.text) await post("/api/points/correct", { id: point.id, text: typed });
      delete memoryUi.editing[point.id];
      delete memoryUi.draft[point.id];
    }, refresh) : (memoryUi.editing[point.id] = true, refresh())),
  }, editing ? "Done" : "Correct");
  const card = h("article", { class: closed ? "card memory closed" : "card memory" },
    h("div", { class: "hit-head" },
      h("span", { class: "kind" }, KINDS[point.kind] || point.kind),
      point.subject_name ? h("span", { class: "subjects" }, point.subject_name) : null,
      h("span", {}, memoryDate(point)),
      status ? h("span", {}, status) : null),
    text,
    h("div", { class: "actions" },
      h("span", { class: "sub" }, "Importance"), levels, edit,
      point.kind === "pending" && !closed ? h("button", {
        type: "button", class: "btn", "data-key": `close-${point.id}`,
        onclick: () => change(() => post("/api/points/close", { id: point.id }), refresh),
      }, "Close the point") : null,
      h("button", {
        type: "button", class: "btn link", "data-key": `conversation-${point.id}`,
        onclick: () => { state.focus = point.session_id; state.view = "fil"; state.subject = ""; show(); },
      }, "View the conversation"),
      forgetControl("Forget", "Permanent.", async () => {
        try {
          await post("/api/points/forget", { id: point.id });
        } catch (error) {
          if (error.status !== 404) throw error;
        }
        delete memoryUi.editing[point.id];
        delete memoryUi.draft[point.id];
        gone(card);
        refreshCounts();
      })));
  return card;
}

async function showMemories(mine) {
  const alert = alertBox(state.notice);
  const total = h("div", { class: "result-count" });
  const list = h("div", { class: "stack" });
  const more = h("div", { class: "more" });
  mount(...heading("Memories",
    "What the summaries kept. A correction from you is never rewritten."),
  pillRow(KIND_TABS, state.kind, (kind) => { state.kind = kind; show(); }, "kind"), subjectFilter(), alert, total,
  list, more);
  let shown = 0;
  let count = 0;

  function fetchPage(offset, limit) {
    const params = { limit, offset };
    if (state.kind !== "all") params.kind = state.kind;
    if (state.subject) params.subject = state.subject;
    return get("/api/points", params);
  }

  function footer() {
    total.textContent = count ? plural(count, "memory", "memories") : "";
    more.replaceChildren(...(shown < count ? [
      h("button", { type: "button", class: "btn", "data-key": "more", onclick: loadMore }, "Show more"),
    ] : []));
  }

  function gone(card) {
    card.remove();
    shown -= 1;
    count -= 1;
    if (!list.children.length) list.append(h("p", { class: "empty" }, "No memories."));
    footer();
  }

  let loading = false;

  async function loadMore() {
    if (loading) return;
    loading = true;
    try {
      await loadMorePage();
    } finally {
      loading = false;
    }
  }

  async function loadMorePage() {
    let page;
    try {
      page = await fetchPage(shown, MEMORY_PAGE);
    } catch {
      if (mine === epoch) say(alert, "Could not load the memories.");
      return;
    }
    if (mine !== epoch) return;
    say(alert, "");
    list.append(...page.points.map((point) => pointCard(point, refresh, gone)));
    shown += page.points.length;
    count = page.total;
    footer();
  }

  async function refresh(message) {
    const wanted = Math.max(shown, MEMORY_PAGE);
    const found = [];
    try {
      let page;
      do {
        page = await fetchPage(found.length, Math.min(MEMORY_PAGE, wanted - found.length));
        found.push(...page.points);
      } while (found.length < wanted && found.length < page.total);
      count = page.total;
    } catch {
      if (mine === epoch) say(alert, message ? `${message} Could not load the memories.` : "Could not load the memories.");
      return;
    }
    if (mine !== epoch) return;
    say(alert, message || "");
    shown = found.length;
    replaceKeepingFocus(list, ...(found.length
      ? found.map((point) => pointCard(point, refresh, gone))
      : [h("p", { class: "empty" }, "No memories.")]));
    footer();
  }
  await refresh("");
}

async function showMemo(mine) {
  const alert = alertBox(state.notice);
  const empty = h("p", { class: "empty" }, "Nothing to remember here: Claude would receive no memo.");
  const memo = h("pre", { class: "memo" });
  empty.hidden = true;
  memo.hidden = true;
  const select = h("select", { "data-key": "memo-subject", onchange: (event) => pick(event.target.value) },
    subjectOptions(state.subjects, state.memoSubject, "Start in this directory"));
  mount(...heading("Memo",
    "What Claude receives when a session starts in the directory where paul web was launched. For a subject: the index shown when a message names it; the points kept may then differ, since they follow the words of the message."),
  h("label", { class: "field" }, "Memo for", select), alert, empty, memo);

  async function pick(uid) {
    state.memoSubject = uid;
    memo.hidden = true;
    memo.textContent = "";
    empty.hidden = true;
    let result;
    try {
      result = await get("/api/memo", uid ? { subject: uid } : {});
    } catch {
      if (mine === epoch && uid === state.memoSubject) say(alert, "Could not load the memo.");
      return;
    }
    if (mine !== epoch || uid !== state.memoSubject) return;
    say(alert, "");
    memo.textContent = result.text;
    memo.hidden = !result.text;
    empty.hidden = !!result.text;
  }
  await pick(state.memoSubject);
}

function hitCard(hit) {
  const when = new Date(hit.ts);
  const date = when.toLocaleDateString(LOCALE, { day: "numeric", month: "short" });
  return h("article", { class: "card hit" },
    h("div", { class: "hit-head" },
      h("span", {}, `${date}, ${clock(when)}`),
      hit.subjects.length ? h("span", { class: "subjects" }, hit.subjects.join(", ")) : null,
      h("span", {}, ROLES[hit.role] || hit.role)),
    h("div", {}, hit.segments.map(([text, marked]) => (marked ? h("mark", {}, text) : text))));
}

let typing = null;
let searchSeq = 0;

function showSearch(mine) {
  const alert = alertBox(state.notice);
  const count = h("div", { class: "result-count" });
  const results = h("div", { class: "stack" });
  const input = h("input", { type: "search", value: state.query, "data-key": "query", autocomplete: "off" });

  async function run() {
    const text = state.query.trim();
    const ticket = ++searchSeq;
    results.replaceChildren();
    if (!text) {
      count.textContent = "Type a word to search.";
      say(alert, "");
      return;
    }
    const params = { q: text };
    if (state.subject) params.subject = state.subject;
    let hits;
    try {
      hits = await get("/api/search", params);
    } catch {
      if (mine === epoch && ticket === searchSeq) {
        count.textContent = "";
        say(alert, "Could not run the search.");
      }
      return;
    }
    if (mine !== epoch || ticket !== searchSeq || text !== state.query.trim()) return;
    say(alert, "");
    count.textContent = hits.length === 0 ? "No results." : plural(hits.length, "result");
    results.replaceChildren(...hits.map(hitCard));
  }

  input.addEventListener("input", () => {
    state.query = input.value;
    clearTimeout(typing);
    typing = setTimeout(run, SEARCH_DELAY);
  });
  mount(
    ...heading("Search", "Covers all your sessions: you are the one looking."),
    h("label", { class: "search-field" }, "Search for", input),
    subjectFilter(), alert, count, results);
  clearTimeout(typing);
  run();
}

async function change(action, refresh) {
  let message = "";
  try {
    await action();
  } catch (error) {
    message = `Change refused: ${error.message}`;
  }
  await refresh(message);
}

function subjectPanel(node, panel, refresh, redraw) {
  const uid = node.uid;
  const others = state.subjects.filter((s) => s.uid !== uid);
  const close = () => { delete subjectUi.open[uid]; delete subjectUi.draft[uid]; };
  const cancel = h("button", { type: "button", class: "btn", onclick: () => { close(); redraw(); } }, "Cancel");
  const run = (label, strong, send) => h("button", {
    type: "button", class: strong ? "btn strong" : "btn", "data-key": `${panel}-confirm-${uid}`,
    onclick: (event) => {
      if (event.detail > 1) return;
      change(async () => { await send(); close(); }, refresh);
    },
  }, label);
  if (panel === "rename") {
    const input = h("input", {
      type: "text", value: subjectUi.draft[uid] ?? node.name, "data-key": `name-${uid}`,
      oninput: (event) => { subjectUi.draft[uid] = event.target.value; },
    });
    return h("div", { class: "panel" }, h("label", { class: "field" }, "New name", input),
      h("div", { class: "actions" }, run("Save", true, () => post("/api/subjects/rename", { uid, name: input.value })), cancel));
  }
  if (panel === "move") {
    const below = new Set(flatten(node.children, 0, []).map((s) => s.uid));
    const select = h("select", { "data-key": `parent-${uid}` },
      subjectOptions(others.filter((s) => !below.has(s.uid)), node.parent_uid ?? "", "None: at the root"));
    return h("div", { class: "panel" }, h("label", { class: "field" }, "Place under", select),
      h("div", { class: "actions" }, run("Move here", true, () => post("/api/subjects/parent", { uid, parent: select.value })), cancel));
  }
  if (panel === "merge") {
    const select = h("select", { "data-key": `into-${uid}` }, subjectOptions(others, "", null));
    return h("div", { class: "panel" }, h("label", { class: "field" }, "Merge into", select),
      h("p", { class: "sub" }, `"${node.name}" becomes a keyword of the chosen subject, which receives its memories, sessions and links. This is permanent.`),
      h("div", { class: "actions" }, run("Confirm the merge", true, () => post("/api/subjects/merge", { from: uid, into: select.value })), cancel));
  }
  const type = h("select", { "data-key": `link-type-${uid}` },
    Object.entries(LINKS).map(([value, text]) => h("option", { value }, text)));
  const target = h("select", { "data-key": `link-target-${uid}` }, subjectOptions(others, "", null));
  return h("div", { class: "panel" }, h("label", { class: "field" }, "Link", type), h("label", { class: "field" }, "To", target),
    h("div", { class: "actions" }, run("Add the link", true, () => post("/api/subjects/link", { source: uid, type: type.value, target: target.value })), cancel));
}

const SUBJECT_ACTIONS = [["rename", "Rename"], ["move", "Move"], ["merge", "Merge"], ["link", "Link"]];

function subjectCard(node, refresh, redraw) {
  const uid = node.uid;
  const open = subjectUi.open[uid] || "";
  const actions = SUBJECT_ACTIONS.map(([panel, label]) => h("button", {
    type: "button", class: "btn", "aria-expanded": String(open === panel), "data-key": `${panel}-${uid}`,
    onclick: () => { subjectUi.open[uid] = open === panel ? "" : panel; delete subjectUi.draft[uid]; redraw(); },
  }, label));
  return h("article", { class: "card subject", "aria-label": node.name },
    h("div", { class: "hit-head" },
      h("span", { class: "subject-name" }, node.name),
      node.kind === "repo" ? h("span", { class: "badge" }, "repository") : null,
      h("span", {}, plural(node.points, "memory", "memories"))),
    node.aliases.length ? h("div", { class: "sub" }, `Keywords: ${node.aliases.join(", ")}`) : null,
    node.synthesis ? h("p", { class: "synthesis" }, node.synthesis) : null,
    node.links.length ? h("ul", { class: "links" }, node.links.map((l) => h("li", {},
      h("span", {}, `${LINKS[l.type] || l.type} ${l.target_name || l.target}`),
      h("button", {
        type: "button", class: "btn", "aria-label": `Remove the link to ${l.target_name || l.target}`,
        "data-key": `unlink-${uid}-${l.type}-${l.target}`,
        onclick: () => change(() => post("/api/subjects/unlink", { source: uid, type: l.type, target: l.target }), refresh),
      }, "Remove")))) : null,
    h("div", { class: "actions" }, actions, h("button", {
      type: "button", class: "btn link", "data-key": `memories-${uid}`,
      onclick: () => { state.subject = uid; state.view = "souvenirs"; show(); },
    }, "View the memories")),
    open ? subjectPanel(node, open, refresh, redraw) : null);
}

function subjectNode(node, refresh, redraw) {
  return h("div", { class: "node" }, subjectCard(node, refresh, redraw),
    node.children.length ? h("div", { class: "children" }, node.children.map((c) => subjectNode(c, refresh, redraw))) : null);
}

async function showSubjects(mine) {
  const alert = alertBox(state.notice);
  const list = h("div", { class: "tree" });
  mount(...heading("Subjects",
    "The tree of what your sessions talk about, with their links. What you change here, the summarizer does not undo."),
  alert, list);
  let known = null;

  function redraw() {
    replaceKeepingFocus(list, ...(known.length
      ? known.map((node) => subjectNode(node, refresh, redraw))
      : [h("p", { class: "empty" }, "No subjects.")]));
  }

  async function refresh(message) {
    try {
      await loadSubjects();
    } catch {
      if (mine !== epoch) return;
      const failure = "Could not load the subjects.";
      say(alert, message ? `${message} ${failure}` : failure);
      if (known) redraw();
      return;
    }
    if (mine !== epoch) return;
    known = state.tree;
    say(alert, message || "");
    redraw();
    refreshCounts();
  }
  await refresh("");
}

async function boot() {
  renderMenu();
  try {
    await get("/api/refresh");
  } catch {
    state.notice = "The index could not be updated.";
  }
  try {
    await loadSubjects();
  } catch {
    state.tree = [];
    state.subjects = [];
  }
  await refreshCounts();
  show();
}

boot();
