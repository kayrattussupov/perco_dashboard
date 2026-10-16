"use strict";
// Canteen dashboard: renders /api/today, refreshes on server-sent "changed" events.

const KIOSK_IDLE_MS = 20000;
let D = null;            // last /api/today payload
let BL = null;           // last /api/blocking payload
let SH = null;           // last /api/health payload: the service's own state
let staffById = new Map();
let lastSeenId = 0;      // newest event id already rendered
let view = "a";
let lastSwipeAt = 0;

const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const hm = ts => ts.slice(11, 16);
const hms = ts => ts.slice(11, 19);
function plural(n, one, few, many) {
  const a = n % 10, b = n % 100;
  if (a === 1 && b !== 11) return one;
  if (a >= 2 && a <= 4 && (b < 10 || b >= 20)) return few;
  return many;
}
const passWord = n => plural(n, "проход", "прохода", "проходов");
const mealName = k => (D.meals.find(m => m.key === k) || {}).name || "вне окна питания";
const mealCls = k => "meal-" + (k ? k[0] : "x"); // n1/n2/n3 share the colour of "n"
// Russian case forms: "на завтраке" / "на завтрак"; other windows are quoted by name.
const onMeal = k => ({b: "на завтраке", l: "на обеде", d: "на ужине"}[k] || `в окне «${mealName(k)}»`);
const toMeal = k => ({b: "на завтрак", l: "на обед", d: "на ужин"}[k] || `в окне «${mealName(k)}»`);

const COLORS = ["#2D6A8F","#7A4F9A","#A0522D","#3B7A57","#5A5F9E","#9A4F6B","#2F7F86","#8A6A1F","#4D6C8A","#7F5A3C","#356F5A","#6B5B95"];
function person(id, fio) {
  const s = staffById.get(id);
  return s || {id, fio: fio || "Неизвестный", dep: "", pos: "", tab: "", photo: false, meals: []};
}
// Code 1 "Идентификатор не зарегистрирован": the controller does not know the card. Without an employee it is a
// new card nobody registered in PERCo; with one, PERCo knows the card but the controller lost it — re-registering
// the card sends it again.
const UNREGISTERED = 1;
const cardDeny = e => e.kind === "deny" && e.code === UNREGISTERED;
// The card number stays the same when the card is registered, so the first event of the day that carries both
// the number and an employee names the owner of that card's earlier "unknown" refusals.
let owners = new Map(); // card → first event with an employee; rebuilt on every render
function linkCards() {
  owners = new Map();
  for (const e of D.events) if (e.card && e.user_id && !owners.has(e.card)) owners.set(e.card, e);
}
const ownerOf = e => !e.user_id && e.card ? owners.get(e.card) : undefined;
const uidOf = e => e.user_id || ownerOf(e)?.user_id || 0;
// When a card refusal stopped being a problem: an unknown card got registered, a lost card passed again; "" while open.
function fixedAt(e) {
  if (!cardDeny(e)) return "";
  if (!e.user_id) return e.card && D.events.find(x => x.card === e.card && x.user_id && x.ts > e.ts)?.ts || "";
  return D.events.find(x => x.kind === "pass" && x.user_id === e.user_id && x.ts > e.ts)?.ts || "";
}
const unknownCard = e => cardDeny(e) && !e.user_id && !fixedAt(e);
const lostCard = e => cardDeny(e) && !!e.user_id && !fixedAt(e);
const fixedCard = e => cardDeny(e) && !!fixedAt(e);
// The person behind a swipe; an unregistered card has none until it is registered, so it is told apart by its number.
function who(e) {
  const uid = uidOf(e);
  if (uid) return person(uid, e.fio || ownerOf(e)?.fio);
  return {id: 0, fio: "Неизвестная карта", dep: "", pos: "", tab: "", card: e.card, photo: false, meals: []};
}
const keyOf = e => uidOf(e) ? "u" + uidOf(e) : "c" + (e.card || e.id);
// A line for the person's card when their card was registered today after refusals.
function regNote(list) {
  const e = list.find(x => fixedCard(x) && !x.user_id);
  return e ? `Карта зарегистрирована сегодня, опознана в ${hm(fixedAt(e))}` : "";
}
const same = (list, e) => list.filter(x => keyOf(x) === keyOf(e));
function avatar(p) {
  if (p.photo) return `<div class="avatar"><img src="/api/photo/${p.id}" alt=""></div>`;
  if (!p.id) return '<div class="avatar unknown">?</div>';
  const ini = p.fio.split(" ").slice(0, 2).map(s => s[0] || "").join("");
  return `<div class="avatar" style="background:${COLORS[p.id % COLORS.length]}">${esc(ini)}</div>`;
}
const role = p => [p.pos, p.dep].filter(Boolean).join(", ") + (p.tab ? ` · таб. ${esc(p.tab)}` : "") + (p.card ? `карта № ${esc(p.card)}` : "");
const tplText = p => p.meals.length ? p.meals.map(k => `<span class="${mealCls(k)}">${esc(mealName(k))}</span>`).join(", ") : "—";

const passes = () => D.events.filter(e => e.kind === "pass");
const swipes = () => D.events.filter(e => e.kind === "pass" || e.kind === "deny");
const ofUser = (list, uid) => list.filter(e => e.user_id === uid);
const fixedText = e => e.user_id ? "До перерегистрации карты" : "До регистрации карты";
function chips(list) {
  return list.map(e => {
    const cls = fixedCard(e) ? "deny done" : lostCard(e) ? "deny lost" : e.kind === "deny" ? "deny" : mealCls(e.meal);
    const title = fixedCard(e) ? fixedText(e) : cardDeny(e) ? "Карта не прошла: " + e.name : e.kind === "deny" ? "Отказ контроллера" : mealName(e.meal);
    return `<span class="chip ${cls}" title="${esc(title)}">${hm(e.ts)}</span>`;
  }).join("");
}
function grouped() {
  const m = new Map();
  swipes().forEach(e => { const k = keyOf(e); if (!m.has(k)) m.set(k, []); m.get(k).push(e); });
  return [...m.values()].map(list => ({p: who(list[0]), list}))
    .sort((a, b) => b.list.at(-1).ts.localeCompare(a.list.at(-1).ts));
}

// Why the controller refused: antipass (already inside) vs. time criteria (not this person's window).
const ANTIPASS_CODES = [264, 273, 776, 785];
const TIME_CODES = [529, 776, 785];
// Code 8 "Запрет прохода" is what a blocked card gets; if the service blocked this person for this meal, say why.
// The block outlives the window (held until the person's next window), so outside windows any service block still in force counts.
const blockedByService = e => BL && BL.today.some(b => b.user_id === e.user_id && b.blocked_at && b.blocked_at <= e.ts && (e.meal ? b.meal === e.meal : b.restore_at > e.ts));
function denyReason(e) {
  if (ANTIPASS_CODES.includes(e.code) || (e.code === 8 && blockedByService(e))) return e.meal ? `уже был ${onMeal(e.meal)}` : "уже поел, ждите своё следующее окно";
  if (TIME_CODES.includes(e.code)) return "нет доступа в это время";
  return e.name.replace(/^Запрет прохода:?\s*/i, "") || "запрет прохода";
}

// Verdict for the latest swipe of a person.
function verdict(e) {
  if (e.kind === "deny" && e.code === UNREGISTERED) {
    const n = same(swipes(), e).filter(x => x.kind === "deny" && x.code === UNREGISTERED).length, label = `${plural(n, "попытка", "попытки", "попыток")} сегодня`;
    if (!e.user_id) return {cls: "warn", text: "Карта не зарегистрирована — оформите нового сотрудника в PERCo", n, label};
    return {cls: "lost", text: "Карта не прописана в контроллере — перерегистрируйте карту в PERCo", n, label};
  }
  const inMeal = ofUser(passes(), e.user_id).filter(x => x.meal && x.meal === e.meal).length;
  if (e.kind === "deny") return {cls: "warn", text: `Отказ — ${denyReason(e)}`, n: inMeal, label: e.meal ? `${passWord(inMeal)} ${toMeal(e.meal)}` : `${passWord(ofUser(passes(), e.user_id).length)} сегодня`};
  if (!e.meal) return {cls: "idle", text: "Проход вне окна питания", n: ofUser(passes(), e.user_id).length, label: `${passWord(ofUser(passes(), e.user_id).length)} сегодня`};
  if (e.repeat) return {cls: "warn", text: `Повторный проход — ${e.seq}-й ${toMeal(e.meal)}`, n: inMeal, label: `${passWord(inMeal)} ${toMeal(e.meal)}`};
  return {cls: "ok", text: `${mealName(e.meal)}: проход разрешён`, n: inMeal, label: `${passWord(inMeal)} ${toMeal(e.meal)}`};
}

function rA(flash) {
  const c = $("aCard"), last = swipes().at(-1);
  if (!last) c.innerHTML = '<div class="state idle">Ожидание прохода по карте</div>';
  else {
    const p = who(last), v = verdict(last);
    c.innerHTML = `<div class="state ${v.cls}">${esc(v.text)}</div>
      <div class="person">${avatar(p)}<div><p class="pname">${esc(p.fio)}</p><p class="prole">${role(p)}</p>
      <p class="prole">Шаблон: ${tplText(p)}</p>${regNote(same(swipes(), last)) ? `<p class="prole">${esc(regNote(same(swipes(), last)))}</p>` : ""}</div></div>
      <div class="big"><div class="mono ${v.cls === "warn" || v.cls === "lost" ? v.cls : ""}">${v.n}</div><span>${esc(v.label)}</span></div>${chips(same(swipes(), last))}`;
    if (flash) { c.classList.remove("flash"); void c.offsetWidth; c.classList.add("flash"); }
  }
  const q = $("aSearch").value.trim().toLowerCase();
  $("aRows").innerHTML = grouped().filter(r => !q || r.p.fio.toLowerCase().includes(q) || (r.p.card || "").includes(q)).map(r => {
    const n = r.list.filter(e => e.kind === "pass").length, rep = r.list.some(e => e.repeat && !cardDeny(e));
    return `<tr class="${r.list.some(lostCard) ? "lost" : rep ? "rep" : ""}"><td><b>${esc(r.p.fio)}</b><br><small style="color:var(--muted)">${role(r.p)}</small></td>
      <td>${tplText(r.p)}</td><td>${chips(r.list)}</td><td class="num ${rep ? "warn" : ""}">${n}</td></tr>`;
  }).join("") || `<tr><td colspan="4" style="color:var(--muted)">${q ? "Никого не найдено." : "С 00:00 проходов ещё не было."}</td></tr>`;
}

function rB() {
  const k = $("kiosk"), last = swipes().at(-1);
  if (!last || Date.now() - lastSwipeAt > KIOSK_IDLE_MS) {
    k.className = "kiosk";
    const cur = D.current ? `<b>${esc(mealName(D.current))}</b>` : "";
    k.innerHTML = `<div class="idle">${cur}Приложите карту к считывателю</div>`;
    return;
  }
  const p = who(last), v = verdict(last);
  k.className = "kiosk " + (v.cls === "idle" ? "" : v.cls);
  k.innerHTML = `<div><div class="verdict">${esc(v.text)}</div>${avatar(p)}
    <p class="kname">${esc(p.fio)}</p><p class="krole">${role(p) || "&nbsp;"}${regNote(same(swipes(), last)) ? "<br>" + esc(regNote(same(swipes(), last))) : ""}</p>
    <div class="kn">${v.n}</div><div class="klbl">${esc(v.label)}</div><div>${chips(same(swipes(), last))}</div></div>`;
}

function badge(e) {
  if (fixedCard(e)) return `<span class="badge" title="${esc(e.name)}">${fixedText(e)}</span>`;
  if (unknownCard(e)) return `<span class="badge warn" title="${esc(e.name)}">Карта не зарегистрирована</span>`;
  if (lostCard(e)) return `<span class="badge lost" title="${esc(e.name)}">Перерегистрировать карту</span>`;
  if (e.kind === "deny") return `<span class="badge warn" title="${esc(e.name)}">Отказ: ${esc(denyReason(e))}</span>`;
  if (!e.meal) return '<span class="badge">Вне окна</span>';
  return e.repeat ? `<span class="badge warn">${e.seq}-й раз</span>` : '<span class="badge ok">1-й раз</span>';
}

function rC(newIds) {
  const evs = D.events.filter(e => e.kind !== "door").reverse().slice(0, 60); // "door not closed" is noise here: no door
  $("feed").innerHTML = evs.map(e => {
    const p = who(e), cls = unknownCard(e) ? "unknown" : lostCard(e) ? "lost" : "";
    const where = p.card ? `карта № ${esc(p.card)}` : `${esc(p.dep)}${p.dep ? " · " : ""}<span class="${mealCls(e.meal)}">${esc(mealName(e.meal))}</span>`;
    return `<div class="ev ${cls} ${newIds.has(e.id) ? "new" : ""}"><div class="tm">${hms(e.ts)}</div>${avatar(p)}
      <div><div class="who">${esc(p.fio)}</div><div class="dep">${where}</div></div>
      ${badge(e)}</div>`;
  }).join("") || '<div class="empty">С 00:00 проходов ещё не было.</div>';

  const g = grouped(), reps = g.filter(r => r.list.some(e => e.repeat && e.code !== UNREGISTERED)); // card trouble is counted apart
  const denied = D.events.filter(e => e.kind === "deny" && e.code !== UNREGISTERED).length;
  const cards = g.filter(r => r.list.some(e => unknownCard(e) || lostCard(e))).length;
  $("cStats").innerHTML = `<div class="kv"><span>Проходов</span><b>${passes().length}</b></div>
    <div class="kv"><span>Сотрудников</span><b>${new Set(passes().map(e => e.user_id)).size}</b></div>
    <div class="kv"><span>С повторами</span><b style="color:var(--warn)">${reps.length}</b></div>
    <div class="kv"><span>Отказов контроллера</span><b style="color:var(--warn)">${denied}</b></div>
    <div class="kv" title="Неизвестные карты и карты, которых нет в контроллере"><span>Карта не прошла</span><b style="color:var(--lost)">${cards}</b></div>`;
  $("cRep").innerHTML = reps.map(r => `<div><span>${esc(r.p.fio)}</span><span>${r.list.filter(e => e.repeat && e.code !== UNREGISTERED).length}</span></div>`).join("")
    || '<div style="color:var(--muted)">Повторов нет</div>';
}

function rD(newUsers) {
  $("board").innerHTML = D.meals.map(m => {
    const by = new Map();
    swipes().filter(e => e.meal === m.key).forEach(e => { const k = keyOf(e); if (!by.has(k)) by.set(k, []); by.get(k).push(e); });
    const rows = [...by.values()].map(l => [uidOf(l[0]), l]).sort((a, b) => b[1].at(-1).ts.localeCompare(a[1].at(-1).ts));
    const expected = D.staff.filter(s => s.meals.includes(m.key)).length;
    const came = rows.filter(([, l]) => l.some(e => e.kind === "pass")).length;
    return `<div class="col"><div class="col-head ${mealCls(m.key)}"><div><h3>${esc(m.name)}</h3><small>${m.begin}–${m.end}</small></div>
      <div class="mono">${came}<small style="font-size:14px;color:var(--muted)"> / ${expected}</small></div></div>
      <div class="col-body">${rows.map(([uid, l]) => {
        const rep = l.some(e => e.repeat && !cardDeny(e)), n = l.filter(e => e.kind === "pass").length, d = l.length - n;
        const note = [n > 1 ? `${n} ${plural(n, "раз", "раза", "раз")}` : "", d ? `отказ ×${d}` : ""].filter(Boolean).join(", ");
        return `<div class="row ${rep ? "rep" : ""} ${newUsers.has(uid) ? "flash" : ""}"><span class="rn">${esc(who(l[0]).fio)}</span>
          <span class="rt">${note ? note + " · " : ""}${l.map(e => hm(e.ts)).join(", ")}</span></div>`;
      }).join("") || '<div class="empty">Пока никто не прошёл</div>'}</div></div>`;
  }).join("");

  const ny = $("notyet"), cur = D.current;
  if (!cur) { ny.innerHTML = '<h3>Ещё не были</h3><span style="color:var(--muted)">Сейчас вне окна питания — список появится во время завтрака, обеда или ужина.</span>'; return; }
  const been = new Set(passes().filter(e => e.meal === cur).map(e => e.user_id));
  const miss = D.staff.filter(s => s.meals.includes(cur) && !been.has(s.id));
  ny.innerHTML = `<h3>Ещё не были ${esc(onMeal(cur))} — ${miss.length}</h3>` +
    (miss.map(s => `<span class="chip">${esc(s.fio)}</span>`).join("") || '<span style="color:var(--muted)">Все прошли</span>');
}

function renderAll(newIds = new Set()) {
  linkCards();
  const newUsers = new Set(D.events.filter(e => newIds.has(e.id)).map(uidOf));
  rA(newIds.size > 0); rB(); rC(newIds); rD(newUsers);
  const w = D.meals.find(m => m.key === D.current);
  $("window").innerHTML = w ? `<span class="${mealCls(w.key)}">${esc(w.name)}</span> до ${w.end}` : '<span style="color:var(--muted)">Вне окна питания</span>';
  $("alerts").innerHTML = D.alerts.map(a => `<div class="alert">${esc(a)}</div>`).join("");
}

let loading = false, again = false;
async function load() {
  if (loading) { again = true; return; }
  loading = true;
  try {
    const r = await fetch("/api/today", {cache: "no-store"});
    if (!r.ok) throw new Error(r.status);
    const rb = await fetch("/api/blocking", {cache: "no-store"});
    BL = rb.ok ? await rb.json() : null;
    D = await r.json();
    if (BL) D.alerts = D.alerts.concat(BL.warnings);
    if (SH) D.alerts = D.alerts.concat(SH.warnings);
    staffById = new Map(D.staff.map(s => [s.id, s]));
    const newIds = new Set(lastSeenId ? D.events.filter(e => e.id > lastSeenId).map(e => e.id) : []);
    if ([...newIds].some(id => { const e = D.events.find(x => x.id === id); return e.kind === "pass" || e.kind === "deny"; })) lastSwipeAt = Date.now();
    lastSeenId = D.events.reduce((m, e) => Math.max(m, e.id), lastSeenId);
    renderAll(newIds);
    renderBlocking();
    $("dot").classList.toggle("bad", D.alerts.length > 0);
  } catch (err) {
    $("dot").classList.add("bad");
    $("alerts").innerHTML = '<div class="alert">Нет связи со службой учёта питания — повторяем попытку…</div>';
    setTimeout(load, 3000);
  } finally {
    loading = false;
    if (again) { again = false; load(); }
  }
}

function setView(v) {
  view = v;
  document.querySelectorAll(".tab").forEach(x => x.setAttribute("aria-selected", x.dataset.v === v));
  document.querySelectorAll(".view").forEach(s => s.classList.toggle("on", s.id === "v-" + v));
  if (location.hash !== "#" + v) history.replaceState(null, "", location.pathname + location.search + "#" + v);
}
document.querySelectorAll(".tab").forEach(t => t.onclick = () => setView(t.dataset.v));
$("aSearch").oninput = () => D && rA(false);
if (new URLSearchParams(location.search).has("kiosk")) document.body.classList.add("kiosk");
const viewFromHash = () => setView(["a", "b", "c", "d"].includes(location.hash.slice(1)) ? location.hash.slice(1) : "a");
window.addEventListener("hashchange", viewFromHash);
viewFromHash();

// ---- Blocking of repeat passes: mode switch, active blocks, PIN dialog ----
const MODE_TEXT = {off: "ВЫКЛ", test: "ТЕСТ", on: "ВКЛ"};
const STATE_TEXT = {pending: "применяется…", blocked: "заблокирован", operator: "заблокирован оператором"};
// PERCo ↔ controller link: shown in the header so an outage is visible at once, with details in the tooltip.
const LINK_TEXT = {ok: "PERCo: на связи", syncing: "PERCo: передача…", settling: "PERCo: связь восстановлена…", offline: "Контроллер: нет связи",
  unreachable: "PERCo: нет связи", unknown: "PERCo: нет данных"};
function renderLink() {
  const pill = $("prcPill"), c = BL && BL.controller;
  if (!c) { pill.className = "pill static"; pill.textContent = "PERCo: ?"; pill.title = "Нет данных от службы"; return; }
  pill.className = "pill static st-" + c.state;
  pill.textContent = LINK_TEXT[c.state] || "PERCo: ?";
  pill.title = `${c.text}
С ${c.since.slice(11, 16)} · проверено ${c.checked_at.slice(11, 19)}`;
}

function renderBlocking() {
  renderLink();
  const pill = $("blkPill"), panel = $("blkPanel");
  if (!BL) { pill.textContent = "Блокировка: ?"; panel.innerHTML = ""; return; }
  pill.className = "pill " + BL.effective + (BL.force_off ? " forced" : "");
  pill.textContent = `Блокировка: ${MODE_TEXT[BL.effective]}`;
  pill.title = BL.force_off ? "Отключено в config.yaml (blocking.force_off)" : "Блокировка повторных проходов — нажмите, чтобы изменить";
  const rows = BL.open.map(b => `<div class="row"><span><b>${esc(person(b.user_id, b.fio).fio)}</b><br>
      <small><span class="${mealCls(b.meal)}">${esc(mealName(b.meal))}</span> · ${esc(STATE_TEXT[b.state] || b.state)}${b.state === "blocked" ? " до " + b.restore_at.slice(11, 16) : ""}${b.note ? " · " + esc(b.note) : ""}</small>
      ${b.last_error ? `<div class="err">Не снимается: ${esc(b.last_error)} (попыток: ${b.attempts})</div>` : ""}</span>
      <button class="mini" data-release="${b.id}">Разблокировать</button></div>`).join("");
  const done = BL.today.filter(b => b.state === "released").length;
  // Total blocked in PERCo at this moment (whoever blocked them); falls back to the service's own rows.
  const bn = BL.blocked_now, total = bn && bn.known ? bn.count : "—"; // never show 0 when PERCo has not answered
  const sub = bn && bn.known
    ? `<div><small>в PERCo на ${esc(bn.checked_at.slice(11, 19))} · службой: ${bn.service} · вручную: ${bn.manual}${bn.error ? " · данные устарели: " + esc(bn.error) : ""}</small></div>
       ${bn.manual ? `<div class="err">Вручную: ${bn.names.map(esc).join(", ")}</div>` : ""}`
    : `<div><small>${bn && bn.error ? "Нет данных PERCo: " + esc(bn.error) : "Данные PERCo загружаются…"}</small></div>`;
  panel.innerHTML = `<h3><span>Заблокировано сейчас — <b class="blk-total ${total > 0 ? "some" : ""}">${total}</b></span><button class="mini" data-release="all" title="Снять все блокировки, поставленные службой (нужен PIN)">Разблокировать всех</button></h3>
    ${sub}
    ${rows || `<small>${BL.effective === "off" ? "Блокировка выключена — работает старый метод, PERCo не меняется." : "Сейчас никто не заблокирован."}</small>`}
    ${done ? `<div style="margin-top:8px"><small>Сегодня снято по расписанию: ${done}</small></div>` : ""}`;
  panel.querySelectorAll("[data-release]").forEach(btn => btn.onclick = () => openDialog(btn.dataset.release));
}

let dlgAction = null, dlgMode = null;
function openDialog(action) {
  dlgAction = action; dlgMode = BL ? BL.mode : "off";
  const isMode = action === "mode";
  $("dlgModes").style.display = isMode ? "" : "none";
  if (isMode) {
    $("dlgTitle").textContent = "Блокировка повторных проходов";
    $("dlgText").textContent = "ВЫКЛ — старый метод: только дашборд, PERCo не меняется; при выключении все блокировки службы снимаются сразу. ТЕСТ — только сотрудники из test_user_ids. ВКЛ — все.";
  } else if (action === "all") {
    $("dlgTitle").textContent = "Снять все блокировки службы";
    $("dlgText").textContent = "Будут разблокированы все, кого заблокировала служба" + (BL && BL.open.length ? ` (${BL.open.length})` : ", сейчас таких нет") + ". Блокировки, поставленные операторами, не трогаются. Режим не меняется. Нужен PIN.";
  } else {
    const b = BL.open.find(x => String(x.id) === action);
    $("dlgTitle").textContent = "Разблокировать";
    $("dlgText").textContent = `${b ? person(b.user_id, b.fio).fio : ""} будет разблокирован в PERCo сейчас.${b && b.state === "operator" ? " Внимание: этого сотрудника заблокировал оператор." : ""}`;
  }
  document.querySelectorAll("#dlgModes button").forEach(x => x.classList.toggle("sel", x.dataset.mode === dlgMode));
  $("dlgPin").value = ""; $("dlgMsg").textContent = ""; $("dlgMsg").className = "dlg-msg";
  $("dlg").showModal(); $("dlgPin").focus();
}
document.querySelectorAll("#dlgModes button").forEach(x => x.onclick = () => {
  dlgMode = x.dataset.mode;
  document.querySelectorAll("#dlgModes button").forEach(y => y.classList.toggle("sel", y === x));
});
$("blkPill").onclick = () => openDialog("mode");
$("dlgCancel").onclick = () => $("dlg").close();
$("dlgPin").onkeydown = ev => { if (ev.key === "Enter") $("dlgOk").click(); };
$("dlgOk").onclick = async () => {
  const msg = $("dlgMsg"), pin = $("dlgPin").value;
  const url = dlgAction === "mode" ? "/api/blocking/mode" : dlgAction === "all" ? "/api/blocking/release-all" : `/api/blocking/release/${dlgAction}`;
  msg.className = "dlg-msg"; msg.textContent = "Выполняется…"; $("dlgOk").disabled = true;
  try {
    const r = await fetch(url, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({pin, mode: dlgMode})});
    const j = await r.json();
    if (!r.ok) { msg.className = "dlg-msg bad"; msg.textContent = j.error || "Ошибка"; return; }
    msg.className = "dlg-msg good";
    msg.textContent = j.released !== undefined ? `Готово. Разблокировано: ${j.released}${j.failed ? `, не удалось: ${j.failed}` : ""}` : "Готово";
    load();
    setTimeout(() => $("dlg").open && $("dlg").close(), 1500);
  } catch (e) {
    msg.className = "dlg-msg bad"; msg.textContent = "Нет связи со службой";
  } finally { $("dlgOk").disabled = false; }
};

function tick() {
  const d = new Date(), p = n => String(n).padStart(2, "0");
  $("clock").textContent = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  if (D && view === "b") rB(); // drop back to idle after KIOSK_IDLE_MS
}
setInterval(tick, 1000); tick();
setInterval(load, 60000); // window changes and day rollover even without events
new EventSource("/api/stream").onmessage = () => load();

// ---- Service health: data freshness, load on PERCo, local disk and backup ----
async function loadHealth() {
  try {
    const r = await fetch("/api/health", {cache: "no-store"});
    SH = r.ok ? await r.json() : null;
  } catch { SH = null; }
  renderHealth();
}
function renderHealth() {
  const p = $("svcPanel");
  if (!SH) { p.innerHTML = "<h3><span>Состояние службы</span></h3><small>Нет данных от службы</small>"; return; }
  const pc = SH.perco, age = SH.data_age_s;
  const fresh = age < 0 ? "ещё не загружались" : age < 60 ? `${Math.round(age)} с назад` : `${Math.round(age / 60)} мин назад`;
  const slow = (pc.kinds || []).filter(k => k.p95_s > 0).sort((a, b) => b.p95_s - a.p95_s).slice(0, 3)
    .map(k => `${esc(k.kind)} — p95 ${k.p95_s.toFixed(2)} с, p99 ${k.p99_s.toFixed(2)} с`).join("<br>");
  p.innerHTML = `<h3><span>Состояние службы</span></h3>
    <div class="row"><span>Проходы обновлены</span><b>${fresh}</b></div>
    ${SH.poll_error && !pc.breaker_open ? `<div class="err">${esc(SH.poll_error)}</div>` : ""}
    <div class="row"><span>Запросов к PERCo</span><b>${pc.last_minute} за мин · ${pc.last_hour} за час</b></div>
    <div class="row"><span>Ошибок PERCo за час</span><b>${pc.errors_hour}</b></div>
    ${pc.breaker_open ? `<div class="err">Запросы к PERCo приостановлены до ${esc(pc.open_until)}</div>` : ""}
    ${slow ? `<div style="margin-top:6px"><small>Самые медленные запросы за час:<br>${slow}</small></div>` : ""}
    <div class="row"><span>Свободно на диске</span><b>${SH.disk_free_gb < 0 ? "—" : SH.disk_free_gb.toFixed(1) + " ГБ"}</b></div>
    <div class="row"><span>Резервная копия</span><b>${SH.backup_at ? esc(SH.backup_at.slice(0, 16)) : "ещё нет"}</b></div>`;
}
loadHealth();
setInterval(loadHealth, 10000);
load();
