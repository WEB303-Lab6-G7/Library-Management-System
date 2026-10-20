/* Shelfwise Library — single-page website.
 * Public pages:    /  /catalog  /books/:id  /features/:slug  /login  /register
 * Member area:     /dashboard  /loans  /holds  /fines  /notifications  /account
 * Librarian area:  /admin  /admin/catalog  /admin/members  /admin/loans  /admin/holds
 *                  /admin/fines  /admin/notifications  /admin/system
 */
(() => {
"use strict";

const BRAND = "Shelfwise";
const TEAM = ["Yeshey Zhennue", "Ndup Dorji", "Sonam Zangmo"];
const POLL_MS = 4000;

// ---------------------------------------------------------------- helpers
const $ = (sel, el = document) => el.querySelector(sel);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const icon = (name, cls = "") => `<svg class="i ${cls}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${window.ICONS[name] || ""}</svg>`;
const nu = (n) => "Nu. " + Number(n || 0).toFixed(2);
const plural = (n, w) => `${n} ${w}${n === 1 ? "" : "s"}`;
const sentence = (s) => { s = String(s || ""); return s.charAt(0).toUpperCase() + s.slice(1); };
const fmtDate = (s) => (s ? new Date(s).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" }) : "—");
const fmtTime = (s) => new Date(s).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
const initials = (name) => String(name || "?").split(/\s+/).filter(Boolean).slice(0, 2).map((p) => p[0].toUpperCase()).join("");
const DAY = 86400000;
const trimSlashes = (p) => { let end = p.length; while (end > 1 && p[end - 1] === "/") end--; return p.slice(0, end) || "/"; };
const isEmail = (v) => { const at = v.indexOf("@"); const dot = v.lastIndexOf("."); return at > 0 && dot > at + 1 && dot < v.length - 1 && !/\s/.test(v); };

function timeAgo(s) {
  const sec = Math.round((Date.now() - new Date(s)) / 1000);
  if (sec < 45) return "just now";
  const min = Math.round(sec / 60); if (min < 60) return `${min} min ago`;
  const hr = Math.round(min / 60); if (hr < 24) return `${hr} h ago`;
  return fmtDate(s);
}
function dueInfo(due) {
  const days = Math.floor((new Date(due) - Date.now()) / DAY);
  if (days < 0) return { text: `Overdue by ${plural(-days, "day")}`, tone: "bad" };
  if (days === 0) return { text: "Due today", tone: "warn" };
  if (days <= 3) return { text: `Due in ${plural(days, "day")}`, tone: "warn" };
  return { text: `Due ${fmtDate(due)}`, tone: "ok" };
}
const greeting = () => { const h = new Date().getHours(); return h < 12 ? "Good morning" : h < 17 ? "Good afternoon" : "Good evening"; };

// Book covers are generated from the title, so every book gets a consistent look.
const COVER_COLORS = ["#1f4d3f", "#7a2e2a", "#2d3e63", "#6b4e16", "#4b2f55", "#24585e", "#8a4a22", "#3b4a3f", "#5a2e45", "#2f5a7a"];
function hash(s) { let h = 7; for (const c of String(s)) { h = Math.imul(h, 31) + (c.codePointAt(0) ?? 0); } return Math.abs(h); }
function cover(b, size = "md") {
  const c = COVER_COLORS[hash(b.title) % COVER_COLORS.length];
  return `<div class="cover cover-${size}" style="--c:${c}" aria-hidden="true">
    ${b.category ? `<span class="cover-cat">${esc(b.category)}</span>` : ""}
    <span class="cover-title">${esc(b.title)}</span>${b.author ? `<span class="cover-author">${esc(b.author)}</span>` : ""}</div>`;
}

// ---------------------------------------------------------------- API
// Each browser tab keeps its OWN login token (sessionStorage is per tab), so you can
// be a librarian in one tab and a member in another at the same time.
const TOKEN_KEY = "shelfwise.session";
const getToken = () => { try { return sessionStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; } };
const setToken = (t) => { try { t ? sessionStorage.setItem(TOKEN_KEY, t) : sessionStorage.removeItem(TOKEN_KEY); } catch { /* storage blocked */ } };

async function api(method, path, body) {
  const headers = { Authorization: "Bearer " + getToken() };
  if (body) headers["Content-Type"] = "application/json";
  const res = await fetch("/api" + path, {
    method, credentials: "omit", // never use the shared cookie from the browser
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });
  const issued = res.headers.get("X-Session-Token");
  if (issued) setToken(issued); // just signed in or created an account in this tab
  let data = null;
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    const err = new Error(sentence(data?.error || `Request failed (${res.status})`));
    err.status = res.status;
    if (res.status === 401 && state.user) { state.user = null; setToken(""); navigate(`/login?next=${encodeURIComponent(location.pathname)}`, true); }
    throw err;
  }
  return data;
}
const get = (p) => api("GET", p);
const post = (p, b) => api("POST", p, b || {});
const soft = (p, fallback) => get(p).catch(() => fallback); // used where a service may be down

// ---------------------------------------------------------------- state
const state = { user: null, page: null, data: null, dataKey: "", pollTimer: null, navToken: 0 };
const isLibrarian = () => state.user?.role === "librarian";

async function refreshUser() {
  try { state.user = await get("/auth/me"); } catch { state.user = null; }
}

// ---------------------------------------------------------------- toasts & modals
function toast(msg, ok = true) {
  const el = document.createElement("div");
  el.className = "toast " + (ok ? "ok" : "err");
  el.innerHTML = icon(ok ? "check-circle" : "alert") + `<div>${esc(msg)}</div>`;
  $("#toasts").appendChild(el);
  setTimeout(() => el.remove(), ok ? 4000 : 6500);
}

function modal({ icon: ic = "info", tone = "", title, text = "", body = "", confirm = "Confirm", danger = false, onConfirm }) {
  closeModal();
  const wrap = document.createElement("div");
  wrap.className = "modal-backdrop";
  wrap.innerHTML = `<div class="modal" role="dialog" aria-modal="true" aria-labelledby="m-title">
    <form id="modal-form" novalidate>
      <div class="modal-head"><div class="modal-icon ${tone}">${icon(ic)}</div>
        <div><h2 id="m-title">${esc(title)}</h2>${text ? `<p>${text}</p>` : ""}</div></div>
      ${body ? `<div class="modal-body">${body}<div class="form-error" id="m-error" style="margin-top:14px">${icon("alert")}<span></span></div></div>` : `<div class="form-error" id="m-error" style="margin:0 22px">${icon("alert")}<span></span></div>`}
      <div class="modal-foot">
        <button type="button" class="btn btn-secondary" data-close>Cancel</button>
        <button type="submit" class="btn ${danger ? "btn-danger" : "btn-primary"}">${esc(confirm)}</button>
      </div>
    </form></div>`;
  document.body.appendChild(wrap);
  const form = $("#modal-form", wrap);
  wrap.addEventListener("click", (e) => { if (e.target === wrap || e.target.closest("[data-close]")) closeModal(); });
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = form.querySelector('button[type="submit"]');
    btn.disabled = true;
    try {
      await onConfirm(Object.fromEntries(new FormData(form)));
      closeModal();
    } catch (err) {
      const box = $("#m-error", wrap);
      box.querySelector("span").textContent = err.message;
      box.classList.add("show");
      btn.disabled = false;
    }
  });
  setTimeout(() => (form.querySelector("input, select, textarea") || form.querySelector('button[type="submit"]')).focus(), 30);
}
function closeModal() { document.querySelector(".modal-backdrop")?.remove(); }
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });

// ---------------------------------------------------------------- shared UI
const pageHead = (ic, title, desc, actions = "") => `<div class="page-head">
  <div class="page-head-icon">${icon(ic, "i-lg")}</div>
  <div class="page-head-text"><h1>${title}</h1>${desc ? `<p>${desc}</p>` : ""}</div>
  ${actions ? `<div class="page-head-actions">${actions}</div>` : ""}</div>`;

const empty = (ic, title, text, action = "") => `<div class="empty"><div class="empty-icon">${icon(ic, "i-lg")}</div>
  <h3>${title}</h3><p>${text}</p>${action}</div>`;

const stat = (ic, label, value, note = "", tone = "") => `<div class="card stat">
  <div class="stat-top"><span>${label}</span><span class="stat-icon ${tone}">${icon(ic)}</span></div>
  <div class="stat-value">${value}</div>${note ? `<div class="stat-note">${note}</div>` : ""}</div>`;

const badge = (text, tone = "", ic = "") => `<span class="badge ${tone}">${ic ? icon(ic) : ""}${esc(text)}</span>`;
const serviceDown = (name) => `<div class="alert warn">${icon("alert")}<div><b>The ${name} service is not responding</b>Some information on this page may be out of date. Messages for it are being kept in its queue and will be processed when it is back.</div></div>`;

const EVENT_META = {
  MemberRegistered: ["user-plus", "green"], BookAdded: ["book-plus", "green"], BookBorrowed: ["book-open", "info"],
  BookReturned: ["return", "ok"], BookOverdue: ["clock", "warn"], FineIssued: ["coins", "bad"], FinePaid: ["check-circle", "ok"],
  MemberSuspended: ["ban", "bad"], MemberReactivated: ["shield", "ok"], HoldPlaced: ["bookmark", "info"], HoldReady: ["bell", "ok"],
};
const eventIcon = (type) => { const [ic, tone] = EVENT_META[type] || ["activity", ""]; return `<div class="list-icon ${tone}">${icon(ic)}</div>`; };

function loanStatus(l) {
  if (l.returnedAt) return l.daysLate ? badge(`Returned ${plural(l.daysLate, "day")} late`, "warn") : badge("Returned", "");
  const d = dueInfo(l.dueDate);
  return badge(d.text, d.tone, d.tone === "bad" ? "alert" : "clock");
}
function availability(b) {
  if (b.availableCopies > 0) return badge(`${b.availableCopies} of ${b.totalCopies} available`, "ok", "check");
  return badge("All copies out", "warn", "clock");
}
function holdStatus(h) {
  if (h.status === "ready") return badge("Ready for pickup", "ok", "bell");
  if (h.status === "waiting") return badge(`Waiting · #${h.position} in queue`, "info", "clock");
  return badge("Collected", "");
}

// ---------------------------------------------------------------- feature content
const FEATURES = {
  catalogue: {
    icon: "library", name: "Catalogue", path: "/catalog", cta: "Browse the catalogue",
    tagline: "See every title in the collection and exactly how many copies are on the shelf right now.",
    steps: [
      ["search", "Search and filter", "Find books by title or author, or narrow the list down by category."],
      ["book", "Check availability", "Each book shows how many copies are on the shelf, on loan or set aside for a reader."],
      ["book-open", "Borrow or reserve", "Borrow straight away if a copy is free, or join the waitlist if it isn't."],
    ],
    service: "Catalog service", events: ["BookAdded", "BookBorrowed", "BookReturned", "HoldReady"],
    tech: "The Catalog service owns books and copies. It keeps availability up to date by consuming loan and hold events, so it never has to ask other services.",
  },
  borrowing: {
    icon: "book-open", name: "Borrowing", path: "/loans", cta: "View my loans",
    tagline: "Borrow a book in one click, keep track of due dates, and return it when you're done.",
    steps: [
      ["book-open", "Borrow online", "Pick an available book and it's yours for 14 days, up to your borrowing limit."],
      ["calendar", "Track due dates", "Your loans page shows what's due soon and what's overdue."],
      ["return", "Return", "Return a book and the copy is immediately available to the next reader."],
    ],
    service: "Circulation service", events: ["BookBorrowed", "BookReturned", "BookOverdue"],
    tech: "Circulation records loans and checks your membership using its own copy of member data, built only from Member events. No other service's database is read.",
  },
  holds: {
    icon: "bookmark", name: "Holds & waitlist", path: "/holds", cta: "View my holds",
    tagline: "When every copy is out, join the queue and we'll set one aside for you when it comes back.",
    steps: [
      ["bookmark", "Place a hold", "Join the waitlist for any book that has no copies on the shelf."],
      ["clock", "Keep your place", "See your position in the queue at any time."],
      ["bell", "Get notified", "The moment a copy is returned it's reserved for the first person waiting."],
    ],
    service: "Reservation service", events: ["HoldPlaced", "BookReturned", "HoldReady"],
    tech: "This is an event chain: BookReturned reaches Reservation, which publishes HoldReady. Catalog then sets the copy aside and Notification tells the reader.",
  },
  fines: {
    icon: "coins", name: "Fines & payments", path: "/fines", cta: "View my fines",
    tagline: "Late returns are charged fairly per day, and paying restores your borrowing straight away.",
    steps: [
      ["clock", "Late returns", "Books returned after the due date are charged Nu. 10 per day late."],
      ["ban", "Borrowing limit", "If unpaid fines go over Nu. 50 your borrowing is paused until they're paid."],
      ["banknote", "Pay and continue", "Pay online and your account is reactivated automatically."],
    ],
    service: "Fine service", events: ["BookOverdue", "BookReturned", "FineIssued", "FinePaid"],
    tech: "A late BookReturned leads to FineIssued, then to MemberSuspended from the Member service, and Circulation then blocks new loans. Paying runs the same chain in reverse.",
  },
  notifications: {
    icon: "bell", name: "Notifications", path: "/notifications", cta: "Open my inbox",
    tagline: "Receipts, reminders and pickup alerts arrive automatically, so you never miss a due date.",
    steps: [
      ["mail", "Receipts", "Get a confirmation every time you borrow or return a book."],
      ["clock", "Reminders", "Hear from us when a book becomes overdue."],
      ["bell", "Pickup alerts", "Know the moment a book you're waiting for is ready."],
    ],
    service: "Notification service", events: ["All events"],
    tech: "Notification subscribes to every event. Each event has a unique ID, so if the same event is delivered twice you still get only one message.",
  },
  librarians: {
    icon: "shield", name: "Librarian tools", path: "/admin", cta: "Open the staff area",
    tagline: "Everything staff need to run the library: catalogue, members, circulation and a live system monitor.",
    steps: [
      ["book-plus", "Manage the catalogue", "Add titles and copies, and see what's on the shelf."],
      ["users", "Members and loans", "Issue loans at the desk, take returns, and record fine payments."],
      ["activity", "System monitor", "Watch events flow between services, check queue depths and test failure handling."],
    ],
    service: "API Gateway + all services", events: ["Every event on the bus"],
    tech: "The monitor reads RabbitMQ's queue depths, shows the live event log, and can replay an event to show that duplicates are ignored.",
  },
};

// ---------------------------------------------------------------- layouts
function publicHeader() {
  const right = state.user
    ? `<a class="btn btn-primary" href="${isLibrarian() ? "/admin" : "/dashboard"}">${icon("dashboard")} ${isLibrarian() ? "Staff area" : "My library"}</a>`
    : `<a class="btn btn-ghost" href="/login">Sign in</a><a class="btn btn-primary" href="/register">Create account</a>`;
  const links = `<a href="/catalog" data-nav="/catalog">Catalogue</a><a href="/#features">Features</a><a href="/#how">How it works</a>`;
  return `<header class="site-header" id="site-header"><div class="site-header-inner">
      <a class="brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a>
      <nav class="site-nav">${links}</nav>
      <div class="site-actions">${right}</div>
      <button class="btn btn-ghost btn-icon site-menu-btn" data-action="toggle-site-menu" aria-label="Menu">${icon("menu")}</button>
    </div>
    <nav class="mobile-nav" id="mobile-nav">${links}${state.user ? right : `<a class="btn btn-secondary" href="/login">Sign in</a><a class="btn btn-primary" href="/register">Create account</a>`}</nav>
  </header>`;
}
function publicFooter() {
  return `<footer class="site-footer"><div class="container">
    <div class="footer-grid">
      <div style="max-width:300px"><a class="brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a>
        <p style="margin-top:12px">A community library you can use from anywhere: browse, borrow, reserve and stay up to date.</p></div>
      <div><h4>Library</h4><ul><li><a href="/catalog">Catalogue</a></li><li><a href="/features/borrowing">Borrowing</a></li><li><a href="/features/holds">Holds &amp; waitlist</a></li><li><a href="/features/fines">Fines</a></li></ul></div>
      <div><h4>Account</h4><ul><li><a href="/login">Sign in</a></li><li><a href="/register">Create account</a></li><li><a href="/features/notifications">Notifications</a></li><li><a href="/features/librarians">For librarians</a></li></ul></div>
    </div>
    <div class="footer-bottom"><span>WEB303 Lab 6 · Group 7: ${TEAM.join(", ")}</span><span>Event-driven microservices · Go · RabbitMQ · PostgreSQL</span></div>
  </div></footer>`;
}
function layoutPublic() {
  return `${publicHeader()}<main id="page"></main>${publicFooter()}`;
}

const MEMBER_NAV = [
  ["Library", [["/dashboard", "dashboard", "Dashboard"], ["/catalog", "library", "Catalogue"]]],
  ["My account", [["/loans", "book-open", "My loans"], ["/holds", "bookmark", "Holds", "holds"], ["/fines", "coins", "Fines", "fines"], ["/notifications", "bell", "Notifications"], ["/account", "user", "Profile"]]],
];
const LIBRARIAN_NAV = [
  ["Overview", [["/admin", "dashboard", "Overview"]]],
  ["Manage", [["/admin/catalog", "library", "Catalogue"], ["/admin/members", "users", "Members"], ["/admin/loans", "book-open", "Circulation", "overdue"], ["/admin/holds", "bookmark", "Holds"], ["/admin/fines", "coins", "Fines"], ["/admin/notifications", "mail", "Notifications"]]],
  ["System", [["/admin/system", "activity", "System monitor"]]],
];
function layoutApp() {
  const nav = (isLibrarian() ? LIBRARIAN_NAV : MEMBER_NAV).map(([label, items]) => `<div class="nav-group"><div class="nav-label">${label}</div>
    ${items.map(([href, ic, text, countKey]) => `<a class="nav-link" href="${href}" data-nav="${href}">${icon(ic)}<span>${text}</span>${countKey ? `<span class="nav-count" data-count="${countKey}" hidden></span>` : ""}</a>`).join("")}</div>`).join("");
  const u = state.user;
  return `<div class="app">
    <aside class="sidebar" id="sidebar">
      <a class="brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a>
      ${nav}
      <div class="sidebar-user"><div class="avatar">${esc(initials(u.name))}</div>
        <div class="who"><b>${esc(u.name)}</b><span>${esc(u.role)}</span></div>
        <button class="btn btn-ghost btn-icon btn-sm" data-action="logout" title="Sign out" aria-label="Sign out">${icon("log-out")}</button></div>
    </aside>
    <div class="sidebar-scrim" id="scrim" data-action="close-sidebar"></div>
    <div class="app-main">
      <header class="topbar"><button class="btn btn-ghost btn-icon" data-action="open-sidebar" aria-label="Open menu">${icon("menu")}</button>
        <a class="brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a></header>
      <main id="page" class="page"></main>
    </div></div>`;
}
function layoutAuth() { return `<div id="page"></div>`; }

// ---------------------------------------------------------------- pages: public
const home = {
  layout: "public", title: "Home",
  load: async () => ({ books: await soft("/books", []) }),
  render: ({ books }) => {
    const titles = books.length, copies = books.reduce((n, b) => n + b.totalCopies, 0);
    const cats = new Set(books.map((b) => b.category)).size;
    const shelf = books.slice(0, 3);
    const featureCards = Object.entries(FEATURES).map(([slug, f]) => `<a class="card feature" href="/features/${slug}">
      <div class="feature-icon">${icon(f.icon, "i-lg")}</div><h3>${f.name}</h3><p>${f.tagline}</p>
      <span class="link">Learn more ${icon("arrow-right", "i-sm")}</span></a>`).join("");
    return `
    <section class="hero"><div class="container hero-grid">
      <div>
        <span class="eyebrow">${icon("library", "i-sm")} Your community library, online</span>
        <h1>Find it, borrow it, <em>read it.</em></h1>
        <p class="hero-lead">Browse the whole collection, borrow in a click, join the waitlist for popular titles and get notified as soon as your book is ready.</p>
        <div class="hero-cta">
          <a class="btn btn-primary btn-lg" href="/catalog">${icon("search")} Browse the catalogue</a>
          ${state.user ? "" : `<a class="btn btn-secondary btn-lg" href="/register">${icon("user-plus")} Create a free account</a>`}
        </div>
        <div class="hero-meta">
          <div><b>${titles}</b><span>titles</span></div>
          <div><b>${copies}</b><span>copies on our shelves</span></div>
          <div><b>${cats}</b><span>categories</span></div>
        </div>
      </div>
      <div class="hero-art" aria-hidden="true">
        <div class="hero-shelf"><div class="hero-books">${shelf.map((b) => cover(b)).join("")}</div></div>
        <div class="hero-card one"><span class="chip">${icon("check")}</span><div><b>Borrowed</b><span>Due back in 14 days</span></div></div>
        <div class="hero-card two"><span class="chip">${icon("bell")}</span><div><b>Your hold is ready</b><span>Pick it up within 3 days</span></div></div>
      </div>
    </div></section>

    <section class="section" id="features" style="padding-top:24px"><div class="container">
      <div class="section-head"><span class="eyebrow">Features</span><h2>Everything your library card does, now online</h2>
        <p>Each part of the library has its own page. Pick one to see how it works.</p></div>
      <div class="features">${featureCards}</div>
    </div></section>

    <section class="section" id="how" style="background:var(--surface);border-top:1px solid var(--line);border-bottom:1px solid var(--line)"><div class="container">
      <div class="section-head"><span class="eyebrow">How it works</span><h2>From sign-up to your first book in minutes</h2></div>
      <div class="steps">
        ${[["user-plus", "Create an account", "Sign up with your name and email. Your membership is active straight away."],
           ["search", "Find a book", "Search the catalogue and see live availability for every title."],
           ["book-open", "Borrow or reserve", "Borrow a free copy, or join the waitlist and we'll hold the next one for you."],
           ["bell", "Stay informed", "Receipts, reminders and pickup alerts arrive in your inbox automatically."]]
          .map(([ic, t, d], i) => `<div class="card step"><div style="display:flex;justify-content:space-between;align-items:center"><span class="step-num">0${i + 1}</span>${icon(ic, "i-lg")}</div><h3>${t}</h3><p>${d}</p></div>`).join("")}
      </div>
    </div></section>

    <section class="section"><div class="container">
      <div class="section-head"><span class="eyebrow">Under the hood</span><h2>Six independent services, connected by events</h2>
        <p>Each part of the library is a separate microservice with its own database. They communicate by publishing events to RabbitMQ, so one service being down never stops the others.</p></div>
      <div class="tech">${[["library", "Catalog"], ["users", "Member"], ["book-open", "Circulation"], ["bookmark", "Reservation"], ["coins", "Fine"], ["bell", "Notification"], ["layers", "RabbitMQ"], ["server", "PostgreSQL"]]
        .map(([ic, n]) => `<span class="tech-chip">${icon(ic)}${n}</span>`).join("")}</div>
    </div></section>

    <section class="section-sm"><div class="container">
      <div class="band"><div><h2>Ready to start reading?</h2><p>It's free to join. Create an account and borrow your first book today.</p></div>
        <div class="band-actions">${state.user
          ? `<a class="btn btn-light btn-lg" href="${isLibrarian() ? "/admin" : "/dashboard"}">Go to my library ${icon("arrow-right")}</a>`
          : `<a class="btn btn-light btn-lg" href="/register">Create account</a><a class="btn btn-outline-light btn-lg" href="/login">Sign in</a>`}</div></div>
    </div></section>`;
  },
};

const featurePage = {
  layout: "public",
  title: (p) => FEATURES[p.slug]?.name || "Feature",
  load: async () => ({}),
  render: (_, p) => {
    const f = FEATURES[p.slug];
    if (!f) return notFoundHTML();
    const others = Object.entries(FEATURES).filter(([s]) => s !== p.slug);
    return `<section class="feature-hero"><div class="container">
      <a class="back" href="/#features">${icon("arrow-left", "i-sm")} All features</a>
      <div class="feature-hero-grid"><div class="feature-hero-icon">${icon(f.icon, "i-xl")}</div>
        <div><span class="eyebrow">Feature</span><h1>${f.name}</h1><p class="lead">${f.tagline}</p>
          <div class="hero-cta" style="margin-top:26px"><a class="btn btn-primary btn-lg" href="${f.path}">${f.cta} ${icon("arrow-right")}</a>
          ${state.user ? "" : `<a class="btn btn-secondary btn-lg" href="/register">Create account</a>`}</div></div></div>
    </div></section>
    <section class="section-sm"><div class="container">
      <div class="steps" style="grid-template-columns:repeat(3,minmax(0,1fr))">${f.steps.map(([ic, t, d], i) => `<div class="card step">
        <div style="display:flex;justify-content:space-between;align-items:center"><span class="step-num">0${i + 1}</span>${icon(ic, "i-lg")}</div><h3>${t}</h3><p>${d}</p></div>`).join("")}</div>
    </div></section>
    <section class="section-sm"><div class="container">
      <div class="card"><div class="card-head"><h2>${icon("workflow")} Behind the scenes</h2></div>
        <div class="behind"><div><h3>Service</h3><p style="font-weight:600;font-size:16px">${f.service}</p><p class="muted" style="margin-top:8px">${f.tech}</p></div>
          <div><h3>Events involved</h3><div class="tech">${f.events.map((e) => `<span class="event-pill">${esc(e)}</span>`).join("")}</div></div></div></div>
    </div></section>
    <section class="section-sm"><div class="container">
      <div class="section-head" style="margin-bottom:24px"><h2 style="font-size:26px">Explore other features</h2></div>
      <div class="features">${others.map(([slug, o]) => `<a class="card feature" href="/features/${slug}"><div class="feature-icon">${icon(o.icon, "i-lg")}</div><h3>${o.name}</h3><p>${o.tagline}</p></a>`).join("")}</div>
    </div></section>`;
  },
};

const catalogPage = {
  layout: "auto", title: "Catalogue", poll: true,
  load: async () => ({ books: await get("/books") }),
  ui: { q: "", cat: "All" },
  render: ({ books }) => {
    const ui = catalogPage.ui;
    const cats = ["All", ...[...new Set(books.map((b) => b.category))].sort((a, b) => a.localeCompare(b))];
    const q = ui.q.trim().toLowerCase();
    const list = books.filter((b) => (ui.cat === "All" || b.category === ui.cat) && (!q || (b.title + " " + b.author).toLowerCase().includes(q)));
    const grid = list.length ? `<div class="book-grid">${list.map((b) => `<a class="book-card" href="/books/${b.id}">${cover(b)}
        <div><h3>${esc(b.title)}</h3></div><div class="by">${esc(b.author)}</div><div>${availability(b)}</div></a>`).join("")}</div>`
      : `<div class="card">${empty("search", "No books match your search", "Try a different title, author or category.")}</div>`;
    const tools = `<div class="catalog-tools">
        <div class="control">${icon("search")}<input class="input" id="cat-search" type="search" placeholder="Search by title or author" value="${esc(ui.q)}" style="padding-left:38px" aria-label="Search the catalogue"></div>
        <div class="chips">${cats.map((c) => `<button class="chip-btn ${c === ui.cat ? "active" : ""}" data-action="cat-filter" data-cat="${esc(c)}">${esc(c)}</button>`).join("")}</div>
      </div><p class="muted small" style="margin:-8px 0 18px">${plural(list.length, "title")}</p>`;
    if (state.user) return pageHead("library", "Catalogue", "Browse the collection, borrow available books or join the waitlist.") + tools + grid;
    return `<section class="section-sm"><div class="container">
      <div class="section-head" style="margin-bottom:28px"><span class="eyebrow">Catalogue</span><h2>Browse the collection</h2><p>Live availability for every title. Sign in to borrow or reserve.</p></div>
      ${tools}${grid}</div></section>`;
  },
};

const bookPage = {
  layout: "auto", poll: true,
  title: (p, d) => d?.book?.title || "Book",
  load: async (p) => {
    const book = await get(`/books/${p.id}`);
    let loans = [], holds = [];
    if (state.user && !isLibrarian()) [loans, holds] = await Promise.all([soft("/loans", []), soft("/holds", [])]);
    return { book, loans, holds };
  },
  render: ({ book: b, loans, holds }) => {
    const u = state.user;
    const myLoan = loans.find((l) => l.bookId === b.id && !l.returnedAt);
    const myHold = holds.find((h) => h.bookId === b.id && h.status !== "collected");
    let panel;
    if (!u) {
      panel = `<h3>Want to read this?</h3><p>Sign in or create a free account to borrow books and join waitlists.</p>
        <a class="btn btn-primary btn-block" href="/login?next=/books/${b.id}">${icon("log-in")} Sign in to borrow</a>
        <a class="btn btn-secondary btn-block" style="margin-top:8px" href="/register">Create account</a>`;
    } else if (isLibrarian()) {
      panel = `<h3>Desk actions</h3><p>Issue this book to a member at the desk.</p>
        <button class="btn btn-primary btn-block" data-action="issue-loan" data-book="${b.id}" ${b.availableCopies ? "" : "disabled"}>${icon("book-open")} Issue a loan</button>
        ${b.availableCopies ? "" : `<p class="small" style="margin:10px 0 0">No copies are on the shelf right now.</p>`}`;
    } else if (u.status === "suspended") {
      panel = `<div class="alert bad">${icon("ban")}<div><b>Borrowing is paused</b>${esc(u.statusReason || "You have unpaid fines.")}</div></div>
        <a class="btn btn-primary btn-block" style="margin-top:14px" href="/fines">${icon("coins")} Pay fines</a>`;
    } else if (myLoan) {
      const d = dueInfo(myLoan.dueDate);
      panel = `<h3>You have this book</h3><p>${d.text}.</p><a class="btn btn-secondary btn-block" href="/loans">${icon("book-open")} Go to my loans</a>`;
    } else if (myHold?.status === "ready") {
      panel = `<h3>Your copy is ready</h3><p>A copy has been set aside for you. Borrow it now to collect it.</p>
        <button class="btn btn-primary btn-block" data-action="borrow" data-book="${b.id}">${icon("check")} Borrow my reserved copy</button>`;
    } else if (myHold) {
      panel = `<h3>You're on the waitlist</h3><p>You're number ${myHold.position} in the queue. We'll notify you as soon as a copy is returned.</p>
        <a class="btn btn-secondary btn-block" href="/holds">${icon("bookmark")} View my holds</a>`;
    } else if (b.availableCopies > 0) {
      panel = `<h3>Available now</h3><p>Borrow it today and return it within 14 days.</p>
        <button class="btn btn-primary btn-block" data-action="borrow" data-book="${b.id}">${icon("book-open")} Borrow this book</button>`;
    } else {
      panel = `<h3>All copies are out</h3><p>Join the waitlist and the next returned copy will be set aside for you.</p>
        <button class="btn btn-primary btn-block" data-action="hold" data-book="${b.id}">${icon("bookmark")} Join the waitlist</button>`;
    }
    const inner = `<a class="link" href="/catalog" style="margin-bottom:22px">${icon("arrow-left", "i-sm")} Back to catalogue</a>
      <div class="book-detail" style="margin-top:18px">
        ${cover(b, "lg")}
        <div><span class="badge green">${esc(b.category)}</span><h1>${esc(b.title)}</h1><p class="by">by ${esc(b.author)}</p>
          ${b.description ? `<p class="desc">${esc(b.description)}</p>` : ""}
          <div class="copies"><div><b>${b.availableCopies}</b><span>On the shelf</span></div><div><b>${b.onLoan}</b><span>On loan</span></div><div><b>${b.onHold}</b><span>Set aside</span></div></div></div>
        <div class="card action-panel">${panel}</div>
      </div>`;
    return state.user ? inner : `<section class="section-sm"><div class="container">${inner}</div></section>`;
  },
};

// ---------------------------------------------------------------- pages: auth
function authShell(title, sub, form, side) {
  return `<div class="auth">
    <aside class="auth-side">
      <a class="brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a>
      <div class="auth-side-body"><h2>${side.title}</h2><ul>${side.points.map((p) => `<li>${icon("check-circle")}<span>${p}</span></li>`).join("")}</ul></div>
      <div class="auth-side-books">${["Folktales of Bhutan", "Building Microservices", "The Circle of Karma"].map((t) => cover({ title: t, author: "" })).join("")}</div>
      <div class="auth-foot">WEB303 Lab 6 · Group 7</div>
    </aside>
    <main class="auth-main"><div class="auth-form">
      <a class="brand auth-mobile-brand" href="/"><span class="brand-mark">${icon("library")}</span>${BRAND}</a>
      <h1>${title}</h1><p class="sub">${sub}</p>${form}</div></main></div>`;
}
const passwordField = (name, label, autocomplete, hint = "") => `<div class="field"><label for="f-${name}">${label}</label>
  <div class="control">${icon("lock")}<input id="f-${name}" name="${name}" type="password" required minlength="6" autocomplete="${autocomplete}">
  <button type="button" class="reveal" data-action="reveal" aria-label="Show password">${icon("eye")}</button></div>${hint ? `<span class="hint">${hint}</span>` : ""}</div>`;

const loginPage = {
  layout: "auth", title: "Sign in",
  render: () => authShell("Welcome back", "Sign in to manage your loans, holds and fines.", `
    <form class="form-grid" data-form="login" novalidate>
      <div class="form-error" id="form-error">${icon("alert")}<span></span></div>
      <div class="field"><label for="f-email">Email address</label><div class="control">${icon("mail")}<input id="f-email" name="email" type="email" required autocomplete="email" placeholder="you@example.com"></div></div>
      ${passwordField("password", "Password", "current-password")}
      <button class="btn btn-primary btn-lg btn-block" type="submit">${icon("log-in")} Sign in</button>
    </form>
    <p class="auth-switch">New to ${BRAND}? <a class="link" href="/register">Create an account</a></p>
    <div class="demo-box">${icon("info")}<div>Staff demo account: <b>librarian@library.bt</b> / <b>librarian123</b>. <button type="button" data-action="fill-demo">Use it</button></div></div>`,
    { title: "Pick up where you left off.", points: ["See what's due back and when", "Get notified the moment a hold is ready", "Pay fines online and keep borrowing"] }),
};

const registerPage = {
  layout: "auth", title: "Create account",
  render: () => authShell("Create your account", "Membership is free and active straight away.", `
    <form class="form-grid" data-form="register" novalidate>
      <div class="form-error" id="form-error">${icon("alert")}<span></span></div>
      <div class="field"><label for="f-name">Full name</label><div class="control">${icon("user")}<input id="f-name" name="name" required autocomplete="name" placeholder="Karma Wangmo"></div></div>
      <div class="field"><label for="f-email">Email address</label><div class="control">${icon("mail")}<input id="f-email" name="email" type="email" required autocomplete="email" placeholder="you@example.com"></div></div>
      ${passwordField("password", "Password", "new-password", "At least 6 characters.")}
      ${passwordField("confirm", "Confirm password", "new-password")}
      <button class="btn btn-primary btn-lg btn-block" type="submit">${icon("user-plus")} Create account</button>
    </form>
    <p class="auth-switch">Already a member? <a class="link" href="/login">Sign in</a></p>`,
    { title: "Join the library in under a minute.", points: ["Borrow up to 3 books at a time", "Reserve popular titles and keep your place in the queue", "Receipts and reminders sent automatically"] }),
};

// ---------------------------------------------------------------- pages: member
const dashboardPage = {
  layout: "app", role: "member", title: "Dashboard", poll: true,
  load: async () => {
    const [me, loans, holds, fines, notes] = await Promise.all([get("/auth/me"), soft("/loans", null), soft("/holds", null), soft("/fines", null), soft("/notifications", null)]);
    state.user = { ...state.user, ...me };
    return { me, loans, holds, fines, notes };
  },
  render: ({ me, loans, holds, fines, notes }) => {
    const active = (loans || []).filter((l) => !l.returnedAt).sort((a, b) => new Date(a.dueDate) - new Date(b.dueDate));
    const overdue = active.filter((l) => new Date(l.dueDate) < Date.now()).length;
    const openHolds = (holds || []).filter((h) => h.status !== "collected");
    const ready = openHolds.filter((h) => h.status === "ready");
    const owed = (fines || []).filter((f) => f.status === "unpaid").reduce((s, f) => s + f.amount, 0);
    const alerts = [];
    if (me.status === "suspended") alerts.push(`<div class="alert bad">${icon("ban")}<div><b>Your borrowing is paused</b>${esc(me.statusReason)}. Pay your fines to continue borrowing.</div><a class="btn btn-danger btn-sm alert-action" href="/fines">Pay now</a></div>`);
    if (ready.length) alerts.push(`<div class="alert ok">${icon("bell")}<div><b>${plural(ready.length, "book")} ready for pickup</b>${ready.map((h) => esc(h.title)).join(", ")}</div><a class="btn btn-secondary btn-sm alert-action" href="/holds">View</a></div>`);
    if (overdue) alerts.push(`<div class="alert warn">${icon("clock")}<div><b>${plural(overdue, "book")} overdue</b>Late returns are charged Nu. 10 per day.</div><a class="btn btn-secondary btn-sm alert-action" href="/loans">Return</a></div>`);
    return `<div class="page-head"><div class="page-head-text"><p style="margin:0 0 4px">${greeting()},</p><h1 class="welcome">${esc(me.name)}</h1></div>
        <div class="page-head-actions"><a class="btn btn-primary" href="/catalog">${icon("search")} Find a book</a></div></div>
      <div class="stack">
        ${alerts.join("")}
        ${loans === null ? serviceDown("circulation") : ""}
        <div class="grid grid-4">
          ${stat("book-open", "On loan", `${active.length}<span class="muted" style="font-size:18px"> / ${me.borrowLimit}</span>`, "your borrowing limit")}
          ${stat("clock", "Overdue", overdue, overdue ? "return these soon" : "all on time", overdue ? "bad" : "")}
          ${stat("bookmark", "Holds", openHolds.length, ready.length ? `${ready.length} ready for pickup` : "in the queue", "info")}
          ${stat("coins", "Fines owed", nu(owed), me.status === "suspended" ? "borrowing paused" : "account in good standing", owed ? "warn" : "")}
        </div>
        <div class="grid grid-2">
          <div class="card"><div class="card-head"><h2>${icon("book-open")} Currently borrowed</h2><a class="link small" href="/loans">All loans ${icon("chevron-right", "i-sm")}</a></div>
            ${active.length ? `<div class="list">${active.slice(0, 5).map((l) => `<a class="list-item" href="/books/${l.bookId}">${cover({ title: l.title }, "xs")}
              <div class="grow"><b>${esc(l.title)}</b><span class="sub">Borrowed ${fmtDate(l.borrowedAt)}</span></div>${loanStatus(l)}</a>`).join("")}</div>`
              : empty("book-open", "Nothing on loan", "Books you borrow will appear here with their due dates.", `<a class="btn btn-secondary btn-sm" href="/catalog">Browse the catalogue</a>`)}</div>
          <div class="card"><div class="card-head"><h2>${icon("bell")} Recent notifications</h2><a class="link small" href="/notifications">Inbox ${icon("chevron-right", "i-sm")}</a></div>
            ${notes === null ? `<div class="card-body">${serviceDown("notification")}</div>` : notes.length ? `<div class="list">${notes.slice(0, 5).map((n) => `<div class="list-item">${eventIcon(n.eventType)}
              <div class="grow"><b>${esc(n.subject)}</b><span class="sub">${esc(n.message)}</span></div><span class="time">${timeAgo(n.createdAt)}</span></div>`).join("")}</div>`
              : empty("inbox", "No notifications yet", "Receipts, reminders and pickup alerts will show up here.")}</div>
        </div>
      </div>`;
  },
};

const loansPage = {
  layout: "app", role: "member", title: "My loans", poll: true, ui: { tab: "current" },
  load: async () => ({ loans: await get("/loans") }),
  render: ({ loans }) => {
    const tab = loansPage.ui.tab;
    const current = loans.filter((l) => !l.returnedAt), past = loans.filter((l) => l.returnedAt);
    const list = tab === "current" ? current : past;
    const rows = list.map((l) => `<tr>
      <td><a class="cell-book" href="/books/${l.bookId}">${cover({ title: l.title }, "xs")}<div><b>${esc(l.title)}</b><span>Copy #${l.copyId}</span></div></a></td>
      <td>${fmtDate(l.borrowedAt)}</td><td>${fmtDate(l.dueDate)}</td><td>${l.returnedAt ? fmtDate(l.returnedAt) : "—"}</td><td>${loanStatus(l)}</td>
      <td class="right">${l.returnedAt ? "" : `<button class="btn btn-secondary btn-sm" data-action="return" data-id="${l.id}" data-title="${esc(l.title)}" data-due="${l.dueDate}">${icon("return")} Return</button>`}</td></tr>`).join("");
    return pageHead("book-open", "My loans", "Books you have borrowed, with their due dates.", `<a class="btn btn-primary" href="/catalog">${icon("plus")} Borrow a book</a>`) + `
      <div class="toolbar"><div class="tabs">
        <button class="tab ${tab === "current" ? "active" : ""}" data-action="tab" data-page="loans" data-tab="current">On loan <span class="count">${current.length}</span></button>
        <button class="tab ${tab === "history" ? "active" : ""}" data-action="tab" data-page="loans" data-tab="history">History <span class="count">${past.length}</span></button></div></div>
      <div class="card">${list.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Book</th><th>Borrowed</th><th>Due</th><th>Returned</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : tab === "current" ? empty("book-open", "You have no books on loan", "Find something to read in the catalogue.", `<a class="btn btn-primary btn-sm" href="/catalog">Browse the catalogue</a>`)
        : empty("clock", "No borrowing history yet", "Books you return will be listed here.")}</div>`;
  },
};

const holdsPage = {
  layout: "app", role: "member", title: "Holds", poll: true,
  load: async () => ({ holds: await get("/holds") }),
  render: ({ holds }) => {
    const rows = holds.map((h) => `<tr>
      <td><a class="cell-book" href="/books/${h.bookId}">${cover({ title: h.title }, "xs")}<div><b>${esc(h.title)}</b><span>Placed ${fmtDate(h.createdAt)}</span></div></a></td>
      <td>${holdStatus(h)}</td><td>${h.status === "ready" ? fmtDate(h.expiresAt) : "—"}</td>
      <td class="right">${h.status === "ready" ? `<button class="btn btn-primary btn-sm" data-action="borrow" data-book="${h.bookId}">${icon("check")} Borrow now</button>` : ""}</td></tr>`).join("");
    return pageHead("bookmark", "Holds", "Your place in the queue for books that are currently out.") + `
      <div class="alert info" style="margin-bottom:18px">${icon("info")}<div>When a copy is returned it's set aside for the first person in the queue, and you'll get a notification. Collect it within 3 days.</div></div>
      <div class="card">${holds.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Book</th><th>Status</th><th>Pick up by</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : empty("bookmark", "No holds", "When every copy of a book is out, you can join its waitlist from the book's page.", `<a class="btn btn-secondary btn-sm" href="/catalog">Browse the catalogue</a>`)}</div>`;
  },
};

const finesPage = {
  layout: "app", role: "member", title: "Fines", poll: true,
  load: async () => {
    const [fines, me] = await Promise.all([get("/fines"), get("/auth/me")]);
    state.user = { ...state.user, ...me };
    return { fines, me };
  },
  render: ({ fines, me }) => {
    const unpaid = fines.filter((f) => f.status === "unpaid");
    const owed = unpaid.reduce((s, f) => s + f.amount, 0);
    const rows = fines.map((f) => `<tr><td><div class="cell-book">${cover({ title: f.title }, "xs")}<div><b>${esc(f.title)}</b><span>Loan #${f.loanId}</span></div></div></td>
      <td>${plural(f.daysLate, "day")}</td><td class="num">${nu(f.amount)}</td><td>${f.status === "paid" ? badge(`Paid ${fmtDate(f.paidAt)}`, "ok", "check") : badge("Unpaid", "bad")}</td>
      <td class="right">${f.status === "unpaid" ? `<button class="btn btn-primary btn-sm" data-action="pay" data-id="${f.id}" data-amount="${f.amount}" data-title="${esc(f.title)}">${icon("banknote")} Pay</button>` : ""}</td></tr>`).join("");
    return pageHead("coins", "Fines", "Late fees are Nu. 10 per day. Borrowing pauses when unpaid fines go over Nu. 50.") + `
      <div class="card balance" style="margin-bottom:18px">
        <div class="stat-icon ${owed ? "warn" : ""}" style="width:48px;height:48px;border-radius:12px">${icon("coins", "i-lg")}</div>
        <div style="flex:1;min-width:180px"><div class="muted small">Outstanding balance</div><div class="balance-value">${nu(owed)}</div></div>
        ${me.status === "suspended" ? badge("Borrowing paused", "bad", "ban") : badge("Good standing", "ok", "check")}
        ${unpaid.length > 1 ? `<button class="btn btn-primary" data-action="pay-all">${icon("banknote")} Pay all</button>` : ""}
      </div>
      <div class="card">${fines.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Book</th><th>Late by</th><th>Amount</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : empty("check-circle", "No fines", "Return books on time and this page stays empty.")}</div>`;
  },
};

const notificationsPage = {
  layout: "app", title: "Notifications", poll: true,
  load: async () => ({ notes: await get("/notifications") }),
  render: ({ notes }) => {
    const lib = isLibrarian();
    return pageHead(lib ? "mail" : "bell", "Notifications", lib ? "Every message sent to members, newest first." : "Receipts, reminders and alerts about your account.") + `
      <div class="card">${notes.length ? `<div class="list">${notes.map((n) => `<div class="list-item">${eventIcon(n.eventType)}
        <div class="grow"><b>${esc(n.subject)}</b><span class="sub">${esc(n.message)}</span>${lib ? `<div class="small muted" style="margin-top:2px">To ${esc(n.recipient)}</div>` : ""}</div>
        <span class="time" title="${esc(new Date(n.createdAt).toLocaleString())}">${timeAgo(n.createdAt)}</span></div>`).join("")}</div>`
        : empty("inbox", "Your inbox is empty", "Messages appear here when you borrow, return or reserve books.")}</div>`;
  },
};

const accountPage = {
  layout: "app", role: "member", title: "Profile",
  load: async () => ({ me: await get("/auth/me") }),
  render: ({ me }) => pageHead("user", "Profile", "Your membership details.") + `
    <div class="grid grid-2" style="align-items:start">
      <div class="card"><div class="card-head"><h2>${icon("user")} Membership</h2>${me.status === "active" ? badge("Active", "ok", "check") : badge("Suspended", "bad", "ban")}</div>
        <div class="kv"><div>Name</div><div>${esc(me.name)}</div><div>Email</div><div>${esc(me.email)}</div><div>Member number</div><div class="mono">#${String(me.id).padStart(5, "0")}</div>
          <div>Borrowing limit</div><div>${plural(me.borrowLimit, "book")} at a time</div><div>Outstanding fines</div><div>${nu(me.outstandingFines)}</div>
          ${me.status !== "active" ? `<div>Reason</div><div>${esc(me.statusReason)}</div>` : ""}</div></div>
      <div class="card card-pad"><h2 style="font-size:15.5px;display:flex;gap:8px;align-items:center">${icon("log-out")} Sign out</h2>
        <p class="muted" style="margin:8px 0 16px">Sign out of ${BRAND} on this device.</p>
        <button class="btn btn-secondary" data-action="logout">${icon("log-out")} Sign out</button></div>
    </div>`,
};

// ---------------------------------------------------------------- pages: librarian
const adminHome = {
  layout: "app", role: "librarian", title: "Overview", poll: true,
  load: async () => {
    const [members, books, loans, holds, fines, status, events] = await Promise.all([
      soft("/members", []), soft("/books", []), soft("/loans", []), soft("/holds", []), soft("/fines", []), soft("/system/status", null), soft("/system/events?limit=6", [])]);
    return { members, books, loans, holds, fines, status, events };
  },
  render: ({ members, books, loans, holds, fines, status, events }) => {
    const active = loans.filter((l) => !l.returnedAt);
    const overdue = active.filter((l) => l.overdue);
    const owed = fines.filter((f) => f.status === "unpaid").reduce((s, f) => s + f.amount, 0);
    const waiting = holds.filter((h) => h.status === "waiting").length;
    const down = status ? Object.entries(status.services).filter(([, up]) => !up).map(([n]) => n) : [];
    return pageHead("dashboard", `${greeting()}, ${esc(state.user.name)}`, "Here's what's happening at the library today.",
      `<button class="btn btn-secondary" data-action="add-book">${icon("book-plus")} Add book</button><button class="btn btn-primary" data-action="issue-loan">${icon("book-open")} Issue loan</button>`) + `
      <div class="stack">
        ${down.length ? `<div class="alert bad">${icon("server")}<div><b>${plural(down.length, "service")} down: ${down.join(", ")}</b>Their events are waiting in RabbitMQ and will be processed when they restart.</div><a class="btn btn-secondary btn-sm alert-action" href="/admin/system">Monitor</a></div>` : ""}
        <div class="grid grid-4">
          ${stat("users", "Members", members.length, `${members.filter((m) => m.status === "suspended").length} suspended`)}
          ${stat("library", "Titles", books.length, `${books.reduce((n, b) => n + b.totalCopies, 0)} copies`, "info")}
          ${stat("book-open", "On loan", active.length, overdue.length ? `${overdue.length} overdue` : "none overdue", overdue.length ? "bad" : "")}
          ${stat("coins", "Unpaid fines", nu(owed), `${waiting} ${waiting === 1 ? "hold" : "holds"} waiting`, owed ? "warn" : "")}
        </div>
        <div class="grid grid-2">
          <div class="card"><div class="card-head"><h2>${icon("clock")} Overdue loans</h2><a class="link small" href="/admin/loans">Circulation ${icon("chevron-right", "i-sm")}</a></div>
            ${overdue.length ? `<div class="list">${overdue.slice(0, 6).map((l) => `<div class="list-item">${cover({ title: l.title }, "xs")}<div class="grow"><b>${esc(l.title)}</b><span class="sub">${esc(l.memberName)}</span></div>${loanStatus(l)}</div>`).join("")}</div>`
              : empty("check-circle", "Nothing overdue", "All borrowed books are within their loan period.")}</div>
          <div class="card"><div class="card-head"><h2>${icon("activity")} Latest events</h2><a class="link small" href="/admin/system">System monitor ${icon("chevron-right", "i-sm")}</a></div>
            ${events.length ? `<div class="list">${events.map((e) => `<div class="list-item">${eventIcon(e.eventType)}<div class="grow"><b class="mono" style="font-weight:500">${esc(e.eventType)}</b><span class="sub">${esc(eventSummary(e))}</span></div><span class="time">${timeAgo(e.occurredAt)}</span></div>`).join("")}</div>`
              : empty("activity", "No events yet", "Events appear here as members use the library.")}</div>
        </div>
      </div>`;
  },
};

const adminCatalog = {
  layout: "app", role: "librarian", title: "Catalogue", poll: true, ui: { q: "" },
  load: async () => ({ books: await get("/books") }),
  render: ({ books }) => {
    const q = adminCatalog.ui.q.trim().toLowerCase();
    const list = books.filter((b) => !q || (b.title + " " + b.author + " " + b.category).toLowerCase().includes(q));
    return pageHead("library", "Catalogue", "Titles and copies held by the library.", `<button class="btn btn-primary" data-action="add-book">${icon("book-plus")} Add book</button>`) + `
      <div class="toolbar"><div class="control" style="flex:1;max-width:380px">${icon("search")}<input class="input" id="admin-search" type="search" placeholder="Search titles, authors, categories" value="${esc(adminCatalog.ui.q)}" style="padding-left:38px"></div>
        <span class="muted small">${plural(list.length, "title")}</span></div>
      <div class="card"><div class="table-wrap"><table class="table"><thead><tr><th>Title</th><th>Category</th><th>On shelf</th><th>On loan</th><th>Set aside</th><th></th></tr></thead><tbody>
      ${list.map((b) => `<tr><td><a class="cell-book" href="/books/${b.id}">${cover(b, "xs")}<div><b>${esc(b.title)}</b><span>${esc(b.author)}</span></div></a></td>
        <td>${esc(b.category)}</td><td>${availability(b)}</td><td class="num">${b.onLoan}</td><td class="num">${b.onHold}</td>
        <td class="right"><button class="btn btn-secondary btn-sm" data-action="issue-loan" data-book="${b.id}" ${b.availableCopies ? "" : "disabled"}>Issue</button></td></tr>`).join("")
        || `<tr><td colspan="6">${empty("search", "No titles found", "Try another search.")}</td></tr>`}
      </tbody></table></div></div>`;
  },
};

const adminMembers = {
  layout: "app", role: "librarian", title: "Members", poll: true,
  load: async () => { const [members, view] = await Promise.all([get("/members"), soft("/member-view", null)]); return { members, view }; },
  render: ({ members, view }) => {
    const byId = Object.fromEntries((view || []).map((v) => [v.memberId, v]));
    const sync = (m) => {
      if (view === null) return badge("Unknown", "", "server");
      const v = byId[m.id];
      if (!v) return badge("Pending", "warn", "clock");
      return v.status === m.status ? badge("In sync", "ok", "check") : badge(`Updating (${v.status})`, "warn", "clock");
    };
    return pageHead("users", "Members", "Everyone registered with the library.") + `
      <div class="alert info" style="margin-bottom:18px">${icon("workflow")}<div><b>Circulation sync</b>Circulation keeps its own copy of each member's status, built only from Member events. This column shows that copy catching up (eventual consistency).</div></div>
      <div class="card">${members.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Member</th><th>Status</th><th>Limit</th><th>Fines owed</th><th>Circulation sync</th></tr></thead><tbody>
      ${members.map((m) => `<tr><td><div class="cell-user"><div class="avatar sm">${esc(initials(m.name))}</div><div><b>${esc(m.name)}</b><div class="small muted">${esc(m.email)}</div></div></div></td>
        <td>${m.status === "active" ? badge("Active", "ok") : `<span title="${esc(m.statusReason)}">${badge("Suspended", "bad", "ban")}</span>`}</td>
        <td class="num">${m.borrowLimit}</td><td class="num">${nu(m.outstandingFines)}</td><td>${sync(m)}</td></tr>`).join("")}
      </tbody></table></div>` : empty("users", "No members yet", "Members appear here when they create an account.")}</div>`;
  },
};

const adminLoans = {
  layout: "app", role: "librarian", title: "Circulation", poll: true, ui: { tab: "active" },
  load: async () => ({ loans: await get("/loans") }),
  render: ({ loans }) => {
    const tab = adminLoans.ui.tab;
    const sets = { active: loans.filter((l) => !l.returnedAt), overdue: loans.filter((l) => !l.returnedAt && l.overdue), returned: loans.filter((l) => l.returnedAt), all: loans };
    const list = sets[tab];
    const t = (k, label) => `<button class="tab ${tab === k ? "active" : ""}" data-action="tab" data-page="adminLoans" data-tab="${k}">${label} <span class="count">${sets[k].length}</span></button>`;
    return pageHead("book-open", "Circulation", "Issue loans at the desk and record returns.", `<button class="btn btn-primary" data-action="issue-loan">${icon("plus")} Issue loan</button>`) + `
      <div class="toolbar"><div class="tabs">${t("active", "On loan")}${t("overdue", "Overdue")}${t("returned", "Returned")}${t("all", "All")}</div></div>
      <div class="card">${list.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Book</th><th>Member</th><th>Borrowed</th><th>Due</th><th>Status</th><th></th></tr></thead><tbody>
      ${list.map((l) => `<tr><td><div class="cell-book">${cover({ title: l.title }, "xs")}<div><b>${esc(l.title)}</b><span>Loan #${l.id} · copy #${l.copyId}</span></div></div></td>
        <td>${esc(l.memberName)}</td><td>${fmtDate(l.borrowedAt)}</td><td>${fmtDate(l.dueDate)}</td><td>${loanStatus(l)}</td>
        <td class="right">${l.returnedAt ? "" : `<button class="btn btn-secondary btn-sm" data-action="return" data-id="${l.id}" data-title="${esc(l.title)}" data-due="${l.dueDate}">${icon("return")} Return</button>`}</td></tr>`).join("")}
      </tbody></table></div>` : empty("book-open", "Nothing here", "No loans match this filter.")}</div>`;
  },
};

const adminHolds = {
  layout: "app", role: "librarian", title: "Holds", poll: true,
  load: async () => ({ holds: await get("/holds") }),
  render: ({ holds }) => pageHead("bookmark", "Holds", "Waitlists for books that are currently out.") + `
    <div class="card">${holds.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Book</th><th>Member</th><th>Placed</th><th>Status</th><th>Pick up by</th></tr></thead><tbody>
    ${holds.map((h) => `<tr><td><div class="cell-book">${cover({ title: h.title }, "xs")}<div><b>${esc(h.title)}</b><span>Hold #${h.id}</span></div></div></td>
      <td>${esc(h.memberName)}</td><td>${fmtDate(h.createdAt)}</td><td>${holdStatus(h)}</td><td>${h.status === "ready" ? fmtDate(h.expiresAt) : "—"}</td></tr>`).join("")}
    </tbody></table></div>` : empty("bookmark", "No holds", "Holds appear when members join the waitlist for a book.")}</div>`,
};

const adminFines = {
  layout: "app", role: "librarian", title: "Fines", poll: true,
  load: async () => ({ fines: await get("/fines") }),
  render: ({ fines }) => {
    const owed = fines.filter((f) => f.status === "unpaid").reduce((s, f) => s + f.amount, 0);
    const collected = fines.filter((f) => f.status === "paid").reduce((s, f) => s + f.amount, 0);
    return pageHead("coins", "Fines", "Late fees charged automatically when books come back late.") + `
      <div class="grid grid-2" style="margin-bottom:18px">${stat("coins", "Outstanding", nu(owed), `${fines.filter((f) => f.status === "unpaid").length} unpaid`, owed ? "warn" : "")}${stat("check-circle", "Collected", nu(collected), `${fines.filter((f) => f.status === "paid").length} paid`)}</div>
      <div class="card">${fines.length ? `<div class="table-wrap"><table class="table"><thead><tr><th>Member</th><th>Book</th><th>Late by</th><th>Amount</th><th>Status</th><th></th></tr></thead><tbody>
      ${fines.map((f) => `<tr><td>${esc(f.memberName)}</td><td>${esc(f.title)}</td><td>${plural(f.daysLate, "day")}</td><td class="num">${nu(f.amount)}</td>
        <td>${f.status === "paid" ? badge("Paid", "ok", "check") : badge("Unpaid", "bad")}</td>
        <td class="right">${f.status === "unpaid" ? `<button class="btn btn-secondary btn-sm" data-action="pay" data-id="${f.id}" data-amount="${f.amount}" data-title="${esc(f.title)}" data-member="${esc(f.memberName)}">${icon("banknote")} Record payment</button>` : ""}</td></tr>`).join("")}
      </tbody></table></div>` : empty("check-circle", "No fines", "Fines appear here when books are returned late.")}</div>`;
  },
};

function eventSummary(e) {
  const d = e.data || {};
  switch (e.eventType) {
    case "MemberRegistered": return `${d.name} joined`;
    case "BookAdded": return `“${d.title}” × ${d.copies}`;
    case "BookBorrowed": return `${d.memberName} borrowed “${d.title}”`;
    case "BookReturned": return `${d.memberName} returned “${d.title}”${d.daysLate ? ` (${plural(d.daysLate, "day")} late)` : ""}`;
    case "BookOverdue": return `“${d.title}” is ${plural(d.daysLate, "day")} overdue`;
    case "FineIssued": return `${d.memberName} fined ${nu(d.amount)} · owes ${nu(d.totalOutstanding)}`;
    case "FinePaid": return `${d.memberName} paid ${nu(d.amount)}`;
    case "MemberSuspended": return `${d.name} suspended`;
    case "MemberReactivated": return `${d.name} reactivated`;
    case "HoldPlaced": return `${d.memberName} is #${d.position} for “${d.title}”`;
    case "HoldReady": return `“${d.title}” set aside for ${d.memberName}`;
  }
  return "";
}

const adminSystem = {
  layout: "app", role: "librarian", title: "System monitor", poll: true, pollMs: 2500,
  load: async () => {
    const [status, events, failure] = await Promise.all([get("/system/status"), soft("/system/events?limit=50", null), soft("/system/failure-mode", null)]);
    return { status, events, failure };
  },
  render: ({ status, events, failure }) => {
    const ICON = { catalog: "library", member: "users", circulation: "book-open", reservation: "bookmark", fine: "coins", notification: "bell" };
    const svcs = Object.keys(ICON).map((n) => { const up = status.services[n]; return `<div class="card svc ${up ? "" : "down"}">
      <div class="svc-top">${icon(ICON[n])}<span class="state ${up ? "up" : "down"}"><span class="dot"></span>${up ? "Running" : "Down"}</span></div><b>${n}</b></div>`; }).join("");
    const by = {};
    for (const q of status.queues) {
      const m = q.name.match(/^(.+)\.events(\.retry|\.dlq)?$/); if (!m) continue;
      const r = by[m[1]] ||= { ready: 0, unacked: 0, retry: 0, dlq: 0, consumers: 0 };
      if (!m[2]) Object.assign(r, { ready: q.messages_ready, unacked: q.messages_unacknowledged, consumers: q.consumers });
      else if (m[2] === ".retry") r.retry = q.messages_ready; else r.dlq = q.messages_ready;
    }
    const qrows = Object.keys(by).sort((a, b) => a.localeCompare(b)).map((k) => { const r = by[k]; const cls = r.dlq ? "q-dlq" : r.ready && !status.services[k] ? "q-alert" : "";
      return `<tr class="${cls}"><td class="mono">${esc(k)}.events</td><td class="num">${r.ready}</td><td class="num">${r.unacked}</td><td class="num">${r.retry}</td><td class="num">${r.dlq}</td><td class="num">${r.consumers}</td></tr>`; }).join("");
    const on = failure?.enabled;
    return pageHead("activity", "System monitor", "Services, message queues and the live event stream.", `<span class="live"><span class="dot"></span>Live</span>`) + `
      <div class="stack">
        <div class="svc-grid">${svcs}</div>
        <div class="sys-grid">
          <div class="stack">
            <div class="card"><div class="card-head"><h2>${icon("layers")} Message queues</h2><span class="muted small">RabbitMQ</span></div>
              ${qrows ? `<div class="table-wrap"><table class="table compact"><thead><tr><th>Queue</th><th title="Waiting to be processed">Waiting</th><th title="Delivered, not yet acknowledged">In flight</th><th title="Waiting to be retried">Retry</th><th title="Dead-lettered after 3 failed retries">DLQ</th><th title="Connected consumers">Consumers</th></tr></thead><tbody>${qrows}</tbody></table></div>`
                : `<div class="card-body">${status.queueError ? serviceDown("RabbitMQ management") : empty("layers", "No queues yet", "Queues are created when services start.")}</div>`}
              <div class="card-body small muted" style="border-top:1px solid var(--line-2)">Yellow: events waiting while a service is down. Red: messages moved to the dead-letter queue after 3 failed retries.</div></div>
            <div class="card card-pad"><div style="display:flex;gap:16px;align-items:flex-start">
              <div class="list-icon ${on ? "bad" : "warn"}">${icon("zap")}</div>
              <div style="flex:1"><b>Simulate processing failures</b><p class="muted small" style="margin-top:4px">When on, the Fine service rejects every event. Return a late book to watch it retry 3 times (5 seconds apart) and then move to the dead-letter queue.</p></div>
              <button class="switch" role="switch" aria-checked="${on ? "true" : "false"}" aria-label="Simulate processing failures" data-action="failure" ${failure === null ? "disabled" : ""}></button></div></div>
          </div>
          <div class="card"><div class="card-head"><h2>${icon("radio")} Event stream</h2><span class="muted small">Replay re-sends an event with the same ID</span></div>
            ${events === null ? `<div class="card-body">${serviceDown("notification")}</div>` : events.length ? `<div class="feed">${events.map((e) => `<div class="event-row">${eventIcon(e.eventType)}
              <div class="grow"><div class="etype">${esc(e.eventType)} ${e.duplicates ? badge(`${plural(e.duplicates, "duplicate")} ignored`, "warn") : ""}</div>
                <div class="esum">${esc(eventSummary(e))}</div><div class="emeta">${esc(e.source)} · ${fmtTime(e.occurredAt)} · <span class="mono">${esc(e.eventId.slice(0, 8))}</span></div></div>
              <button class="btn btn-ghost btn-sm" data-action="replay" data-id="${esc(e.eventId)}" title="Deliver this event again">${icon("replay")} Replay</button></div>`).join("")}</div>`
              : empty("radio", "No events yet", "Events appear here as they flow between services.")}</div>
        </div>
      </div>`;
  },
};

function notFoundHTML() {
  return `<section class="section"><div class="container">${empty("search", "Page not found", "The page you're looking for doesn't exist.", `<a class="btn btn-primary" href="/">Go home</a>`)}</div></section>`;
}
const notFound = { layout: "public", title: "Not found", render: notFoundHTML };

// ---------------------------------------------------------------- routing
const ROUTES = [
  [/^\/$/, home], [/^\/catalog$/, catalogPage], [/^\/books\/(?<id>\d+)$/, bookPage], [/^\/features\/(?<slug>[\w-]+)$/, featurePage],
  [/^\/login$/, loginPage], [/^\/register$/, registerPage],
  [/^\/dashboard$/, dashboardPage], [/^\/loans$/, loansPage], [/^\/holds$/, holdsPage], [/^\/fines$/, finesPage],
  [/^\/notifications$/, notificationsPage], [/^\/account$/, accountPage],
  [/^\/admin$/, adminHome], [/^\/admin\/catalog$/, adminCatalog], [/^\/admin\/members$/, adminMembers], [/^\/admin\/loans$/, adminLoans],
  [/^\/admin\/holds$/, adminHolds], [/^\/admin\/fines$/, adminFines], [/^\/admin\/notifications$/, notificationsPage], [/^\/admin\/system$/, adminSystem],
];
const PAGES = { loans: loansPage, adminLoans };

function match(path) {
  for (const [re, page] of ROUTES) { const m = path.match(re); if (m) return { page, params: m.groups || {} }; }
  return { page: notFound, params: {} };
}
const homeFor = () => (isLibrarian() ? "/admin" : "/dashboard");

function navigate(url, replace = false) {
  if (replace) history.replaceState({}, "", url); else history.pushState({}, "", url);
  void route();
}

async function route() {
  const token = ++state.navToken;
  clearTimeout(state.pollTimer);
  closeModal();
  const path = trimSlashes(location.pathname);
  let { page, params } = match(path);

  // Access control (the gateway enforces it too; this just sends people to the right place).
  if (page.layout === "auth" && state.user) return navigate(new URLSearchParams(location.search).get("next") || homeFor(), true);
  if (page.layout === "app" && !state.user) return navigate(`/login?next=${encodeURIComponent(path)}`, true);
  if (page.role && state.user && state.user.role !== page.role) {
    if (page === notificationsPage) { /* shared */ } else return navigate(homeFor(), true);
  }
  if (path.startsWith("/admin") && state.user && !isLibrarian()) return navigate(homeFor(), true);
  if (page === notificationsPage && state.user && isLibrarian() && path === "/notifications") return navigate("/admin/notifications", true);

  const layout = page.layout === "auto" ? (state.user ? "app" : "public") : page.layout;
  const root = $("#root");
  root.innerHTML = layout === "app" ? layoutApp() : layout === "auth" ? layoutAuth() : layoutPublic();
  markActiveNav(path);
  window.scrollTo(0, 0);
  state.page = { page, params, layout };
  state.dataKey = "";

  const pageEl = $("#page");
  if (!page.load) { pageEl.innerHTML = page.render({}, params); setTitle(page, params); return; }
  pageEl.innerHTML = `<div class="page-loading"><div class="spinner"></div></div>`;
  await refresh(token, true);
}

function setTitle(page, params, data) {
  const t = typeof page.title === "function" ? page.title(params, data) : page.title;
  document.title = t ? `${t} · ${BRAND}` : BRAND;
}

// Loads the page's data and re-renders only when it changed and the user isn't typing.
async function refresh(token = state.navToken, first = false) {
  const cur = state.page; if (!cur) return;
  const { page, params } = cur;
  let data;
  try { data = await page.load(params); }
  catch (err) {
    if (token !== state.navToken) return;
    if (first) $("#page").innerHTML = page.layout === "app" || state.user
      ? `<div class="alert bad">${icon("alert")}<div><b>This page couldn't be loaded</b>${esc(err.message)}</div></div>`
      : `<section class="section"><div class="container"><div class="alert bad">${icon("alert")}<div><b>This page couldn't be loaded</b>${esc(err.message)}</div></div></div></section>`;
    schedule(page, token);
    return;
  }
  if (token !== state.navToken) return;
  const key = JSON.stringify(data);
  const busy = document.querySelector(".modal-backdrop") || (document.activeElement?.closest("#page") && /INPUT|SELECT|TEXTAREA/.test(document.activeElement.tagName) && document.activeElement.type !== "search");
  if (first || (key !== state.dataKey && !busy)) {
    state.data = data; state.dataKey = key;
    renderPage();
    setTitle(page, params, data);
  }
  updateCounts(data);
  schedule(page, token);
}
function renderPage() {
  const { page, params } = state.page;
  const el = $("#page");
  const focusId = document.activeElement?.id, sel = document.activeElement?.selectionStart;
  el.innerHTML = page.render(state.data, params);
  if (focusId) { const f = document.getElementById(focusId); if (f) { f.focus(); try { f.setSelectionRange(sel, sel); } catch { /* not a text input */ } } }
}
function schedule(page, token) {
  if (!page.poll) return;
  clearTimeout(state.pollTimer);
  state.pollTimer = setTimeout(() => { if (token === state.navToken && !document.hidden) void refresh(token); else schedule(page, token); }, page.pollMs || POLL_MS);
}
function markActiveNav(path) {
  document.querySelectorAll("[data-nav]").forEach((a) => {
    const href = a.dataset.nav;
    a.classList.toggle("active", href === path || (href !== "/admin" && href !== "/" && path.startsWith(href + "/")) || (href === "/catalog" && path.startsWith("/books/")));
  });
}
function updateCounts(data) {
  const set = (k, n) => { const el = document.querySelector(`[data-count="${k}"]`); if (el) { el.hidden = !n; el.textContent = n; } };
  if (data?.holds && !isLibrarian()) set("holds", data.holds.filter((h) => h.status === "ready").length);
  if (data?.fines && !isLibrarian()) set("fines", data.fines.filter((f) => f.status === "unpaid").length);
  if (data?.loans && isLibrarian()) set("overdue", data.loans.filter((l) => !l.returnedAt && l.overdue).length);
}

// ---------------------------------------------------------------- actions
async function doLogout() {
  try { await post("/auth/logout"); } catch { /* ignore */ }
  setToken(""); // signs out this tab only
  state.user = null;
  toast("You've been signed out.");
  navigate("/");
}

function issueLoanModal(bookId) {
  Promise.all([get("/members"), get("/books")]).then(([members, books]) => {
    const active = members.filter((m) => m.status === "active");
    modal({
      icon: "book-open", title: "Issue a loan", text: "Lend a book to a member at the desk.", confirm: "Issue loan",
      body: `<div class="form-grid">
        <div class="field"><label for="m-member">Member</label><select id="m-member" name="memberId" required>
          ${active.length ? active.map((m) => `<option value="${m.id}">${esc(m.name)} · ${esc(m.email)}</option>`).join("") : `<option value="">No active members</option>`}</select>
          ${members.length > active.length ? `<span class="hint">Suspended members are not listed.</span>` : ""}</div>
        <div class="field"><label for="m-book">Book</label><select id="m-book" name="bookId" required>
          ${books.map((b) => `<option value="${b.id}" ${b.availableCopies ? "" : "disabled"} ${String(b.id) === String(bookId) ? "selected" : ""}>${esc(b.title)} (${b.availableCopies} available)</option>`).join("")}</select></div>
        <div class="field"><label for="m-days">Loan period (days)</label><input id="m-days" name="dueInDays" type="number" value="14" required>
          <span class="hint">For testing, a negative number creates a loan that is already overdue (e.g. −5).</span></div></div>`,
      onConfirm: async (f) => {
        if (!f.memberId) throw new Error("Choose a member.");
        const l = await post("/loans", { memberId: Number(f.memberId), bookId: Number(f.bookId), dueInDays: Number(f.dueInDays) });
        toast(`“${l.title}” issued to ${l.memberName}. Due ${fmtDate(l.dueDate)}.`);
        void refresh();
      },
    });
  }).catch((e) => toast(e.message, false));
}

function addBookModal() {
  const cats = [...new Set((state.data?.books || []).map((b) => b.category))].sort((a, b) => a.localeCompare(b));
  modal({
    icon: "book-plus", title: "Add a book", text: "New titles appear in the catalogue straight away.", confirm: "Add book",
    body: `<div class="form-grid">
      <div class="field"><label for="m-title">Title</label><input id="m-title" name="title" required></div>
      <div class="form-row"><div class="field"><label for="m-author">Author</label><input id="m-author" name="author" required></div>
        <div class="field"><label for="m-cat">Category</label><input id="m-cat" name="category" list="m-cats" placeholder="e.g. History"><datalist id="m-cats">${cats.map((c) => `<option value="${esc(c)}">`).join("")}</datalist></div></div>
      <div class="field"><label for="m-copies">Number of copies</label><input id="m-copies" name="copies" type="number" min="1" max="50" value="1" required></div>
      <div class="field"><label for="m-desc">Short description <span class="muted">(optional)</span></label><textarea id="m-desc" name="description" rows="3"></textarea></div></div>`,
    onConfirm: async (f) => {
      if (!f.title.trim() || !f.author.trim()) throw new Error("Title and author are required.");
      const b = await post("/books", { ...f, copies: Number(f.copies) });
      toast(`Added “${b.title}” with ${b.totalCopies} ${b.totalCopies === 1 ? "copy" : "copies"}.`);
      void refresh();
    },
  });
}

const ACTIONS = {
  "toggle-site-menu": () => $("#mobile-nav")?.classList.toggle("open"),
  "open-sidebar": () => { $("#sidebar")?.classList.add("open"); $("#scrim")?.classList.add("open"); },
  "close-sidebar": () => { $("#sidebar")?.classList.remove("open"); $("#scrim")?.classList.remove("open"); },
  logout: doLogout,
  reveal: (el) => {
    const input = el.parentElement.querySelector("input");
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    el.innerHTML = icon(show ? "eye-off" : "eye");
    el.setAttribute("aria-label", show ? "Hide password" : "Show password");
  },
  // Demo staff account shown on the sign-in page (same as LIBRARIAN_PASSWORD in docker-compose.yml).
  "fill-demo": () => { $("#f-email").value = "librarian@library.bt"; $("#f-password").value = "librarian123"; }, // NOSONAR: demo credentials
  "cat-filter": (el) => { catalogPage.ui.cat = el.dataset.cat; renderPage(); },
  tab: (el) => { PAGES[el.dataset.page].ui.tab = el.dataset.tab; renderPage(); },
  borrow: async (el) => {
    el.disabled = true;
    try {
      const l = await post("/loans", { bookId: Number(el.dataset.book) });
      toast(`You borrowed “${l.title}”. Please return it by ${fmtDate(l.dueDate)}.`);
      await refreshUser(); void refresh();
    } catch (e) { toast(e.message, false); el.disabled = false; }
  },
  hold: async (el) => {
    el.disabled = true;
    try {
      const h = await post("/holds", { bookId: Number(el.dataset.book) });
      toast(`You're number ${h.position} in the queue for “${h.title}”.`);
      void refresh();
    } catch (e) { toast(e.message, false); el.disabled = false; }
  },
  return: (el) => {
    const late = new Date(el.dataset.due) < Date.now();
    modal({
      icon: "return", tone: late ? "bad" : "", title: `Return “${el.dataset.title}”?`,
      text: late ? "This book is overdue, so a late fee will be charged automatically." : "The copy will be available to the next reader straight away.",
      confirm: "Return book",
      onConfirm: async () => {
        const r = await post(`/loans/${el.dataset.id}/return`);
        toast(r.daysLate ? `Returned “${r.title}”, ${plural(r.daysLate, "day")} late. A fine will follow.` : `Returned “${r.title}”. Thank you!`);
        void refresh();
      },
    });
  },
  pay: (el) => {
    const who = el.dataset.member ? ` from ${el.dataset.member}` : "";
    modal({
      icon: "banknote", title: `${el.dataset.member ? "Record payment" : "Pay fine"} of ${nu(el.dataset.amount)}`,
      text: `For the late return of “${esc(el.dataset.title)}”${esc(who)}.`, confirm: el.dataset.member ? "Record payment" : "Pay now",
      onConfirm: async () => {
        const p = await post(`/fines/${el.dataset.id}/pay`);
        toast(`Payment of ${nu(p.amount)} received. Remaining balance ${nu(p.totalOutstanding)}.`);
        await refreshUser(); void refresh();
      },
    });
  },
  "pay-all": () => {
    const unpaid = (state.data?.fines || []).filter((f) => f.status === "unpaid");
    const total = unpaid.reduce((s, f) => s + f.amount, 0);
    modal({
      icon: "banknote", title: `Pay all fines (${nu(total)})?`, text: `${plural(unpaid.length, "fine")} will be marked as paid.`, confirm: "Pay all",
      onConfirm: async () => {
        await Promise.all(unpaid.map((f) => post(`/fines/${f.id}/pay`)));
        toast(`Paid ${nu(total)}. Your borrowing will be restored in a moment.`);
        await refreshUser(); void refresh();
      },
    });
  },
  "issue-loan": (el) => issueLoanModal(el.dataset.book),
  "add-book": addBookModal,
  failure: async (el) => {
    const enabled = el.getAttribute("aria-checked") !== "true";
    try {
      await post("/system/failure-mode", { enabled });
      toast(enabled ? "Failure simulation on: the Fine service will reject every event." : "Failure simulation off.");
      void refresh();
    } catch (e) { toast(e.message, false); }
  },
  replay: async (el) => {
    const ev = (state.data?.events || []).find((x) => x.eventId === el.dataset.id);
    if (!ev) return;
    const { receivedAt, duplicates, ...envelope } = ev;
    try {
      await post("/system/replay", envelope);
      toast(`${ev.eventType} delivered again with the same ID. Consumers should ignore it.`);
      setTimeout(refresh, 1200);
    } catch (e) { toast(e.message, false); }
  },
};

document.addEventListener("click", (e) => {
  const act = e.target.closest("[data-action]");
  if (act && ACTIONS[act.dataset.action]) {
    e.preventDefault();
    ACTIONS[act.dataset.action](act);
    return;
  }
  const a = e.target.closest("a[href]");
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || a.target === "_blank") return;
  const url = new URL(a.href, location.href);
  if (url.origin !== location.origin || url.pathname.startsWith("/api")) return;
  e.preventDefault();
  if (url.pathname === location.pathname && url.hash) { document.getElementById(url.hash.slice(1))?.scrollIntoView({ behavior: "smooth" }); return; }
  history.pushState({}, "", url.pathname + url.search);
  void route().then(() => { if (url.hash) setTimeout(() => document.getElementById(url.hash.slice(1))?.scrollIntoView({ behavior: "smooth" }), 60); });
});
window.addEventListener("popstate", route);
window.addEventListener("scroll", () => $("#site-header")?.classList.toggle("scrolled", window.scrollY > 4), { passive: true });

document.addEventListener("input", (e) => {
  if (e.target.id === "cat-search") { catalogPage.ui.q = e.target.value; renderPage(); }
  if (e.target.id === "admin-search") { adminCatalog.ui.q = e.target.value; renderPage(); }
});

document.addEventListener("submit", async (e) => {
  const form = e.target.closest("form[data-form]");
  if (!form) return;
  e.preventDefault();
  const f = Object.fromEntries(new FormData(form));
  const errBox = $("#form-error", form);
  const fail = (msg) => { errBox.querySelector("span").textContent = msg; errBox.classList.add("show"); };
  errBox.classList.remove("show");
  if (form.dataset.form === "register") {
    if (!f.name.trim()) return fail("Please enter your name.");
    if (!isEmail(f.email)) return fail("Please enter a valid email address.");
    if (f.password.length < 6) return fail("Your password must be at least 6 characters.");
    if (f.password !== f.confirm) return fail("The passwords don't match.");
  } else if (!f.email || !f.password) return fail("Please enter your email and password.");
  const btn = form.querySelector('button[type="submit"]');
  btn.disabled = true;
  try {
    const user = form.dataset.form === "register"
      ? await post("/auth/register", { name: f.name.trim(), email: f.email.trim(), password: f.password })
      : await post("/auth/login", { email: f.email.trim(), password: f.password });
    state.user = user;
    toast(form.dataset.form === "register" ? `Welcome to ${BRAND}, ${user.name.split(" ")[0]}!` : `Welcome back, ${user.name.split(" ")[0]}.`);
    const next = new URLSearchParams(location.search).get("next");
    const allowed = next && next.startsWith("/") && !next.startsWith("//") && (isLibrarian() ? next.startsWith("/admin") || next.startsWith("/books") || next === "/catalog" : !next.startsWith("/admin"));
    navigate(allowed ? next : homeFor(), true);
  } catch (err) {
    fail(err.message);
    btn.disabled = false;
  }
});

// ---------------------------------------------------------------- start
void refreshUser().then(route);
})();