"use strict";
// Canteen dashboard: renders /api/today, refreshes on server-sent "changed" events.

const KIOSK_IDLE_MS = 20000;
let D = null;            // last /api/today payload
let BL = null;           // last /api/blocking payload
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
function avatar(p) {
  if (p.photo) return `<div class="avatar"><img src="/api/photo/${p.id}" alt=""></div>`;
  const ini = p.fio.split(" ").slice(0, 2).map(s => s[0] || "").join("");
  return `<div class="avatar" style="background:${COLORS[p.id % COLORS.length]}">${esc(ini)}</div>`;
}
const role = p => [p.pos, p.dep].filter(Boolean).join(", ") + (p.tab ? ` · таб. ${esc(p.tab)}` : "");
const tplText = p => p.meals.length ? p.meals.map(k => `<span class="${mealCls(k)}">${esc(mealName(k))}</span>`).join(", ") : "—";

const passes = () => D.events.filter(e => e.kind === "pass");
const swipes = () => D.events.filter(e => e.kind === "pass" || e.kind === "deny");
const ofUser = (list, uid) => list.filter(e => e.user_id === uid);
function chips(list) {
  return list.map(e => `<span class="chip ${e.kind === "deny" ? "deny" : mealCls(e.meal)}" title="${e.kind === "deny" ? "Отказ контроллера" : esc(mealName(e.meal))}">${hm(e.ts)}</span>`).join("");
}
function grouped() {
  const m = new Map();
  swipes().forEach(e => { if (!m.has(e.user_id)) m.set(e.user_id, []); m.get(e.user_id).push(e); });
  return [...m.entries()].map(([uid, list]) => ({p: person(uid, list[0].fio), list}))
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
    const p = person(last.user_id, last.fio), v = verdict(last);
    c.innerHTML = `<div class="state ${v.cls}">${esc(v.text)}</div>
      <div class="person">${avatar(p)}<div><p class="pname">${esc(p.fio)}</p><p class="prole">${role(p)}</p>
      <p class="prole">Шаблон: ${tplText(p)}</p></div></div>
      <div class="big"><div class="mono ${v.cls === "warn" ? "warn" : ""}">${v.n}</div><span>${esc(v.label)}</span></div>${chips(ofUser(swipes(), last.user_id))}`;
    if (flash) { c.classList.remove("flash"); void c.offsetWidth; c.classList.add("flash"); }
  }
  const q = $("aSearch").value.trim().toLowerCase();
  $("aRows").innerHTML = grouped().filter(r => !q || r.p.fio.toLowerCase().includes(q)).map(r => {
    const n = r.list.filter(e => e.kind === "pass").length, rep = r.list.some(e => e.repeat);
    return `<tr class="${rep ? "rep" : ""}"><td><b>${esc(r.p.fio)}</b><br><small style="color:var(--muted)">${role(r.p)}</small></td>
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
  const p = person(last.user_id, last.fio), v = verdict(last);
  k.className = "kiosk " + (v.cls === "idle" ? "" : v.cls);
  k.innerHTML = `<div><div class="verdict">${esc(v.text)}</div>${avatar(p)}
    <p class="kname">${esc(p.fio)}</p><p class="krole">${role(p) || "&nbsp;"}</p>
    <div class="kn">${v.n}</div><div class="klbl">${esc(v.label)}</div><div>${chips(ofUser(swipes(), last.user_id))}</div></div>`;
}

function badge(e) {
  if (e.kind === "deny") return `<span class="badge warn" title="${esc(e.name)}">Отказ: ${esc(denyReason(e))}</span>`;
  if (!e.meal) return '<span class="badge">Вне окна</span>';
  return e.repeat ? `<span class="badge warn">${e.seq}-й раз</span>` : '<span class="badge ok">1-й раз</span>';
}

function rC(newIds) {
  const evs = D.events.filter(e => e.kind !== "door").reverse().slice(0, 60); // "door not closed" is noise here: no door
  $("feed").innerHTML = evs.map(e => {
    const p = person(e.user_id, e.fio);
    return `<div class="ev ${newIds.has(e.id) ? "new" : ""}"><div class="tm">${hms(e.ts)}</div>${avatar(p)}
      <div><div class="who">${esc(p.fio)}</div><div class="dep">${esc(p.dep)}${p.dep ? " · " : ""}<span class="${mealCls(e.meal)}">${esc(mealName(e.meal))}</span></div></div>
      ${badge(e)}</div>`;
  }).join("") || '<div class="empty">С 00:00 проходов ещё не было.</div>';

  const g = grouped(), reps = g.filter(r => r.list.some(e => e.repeat));
  const denied = D.events.filter(e => e.kind === "deny").length;
  $("cStats").innerHTML = `<div class="kv"><span>Проходов</span><b>${passes().length}</b></div>
    <div class="kv"><span>Сотрудников</span><b>${new Set(passes().map(e => e.user_id)).size}</b></div>
    <div class="kv"><span>С повторами</span><b style="color:var(--warn)">${reps.length}</b></div>
    <div class="kv"><span>Отказов контроллера</span><b style="color:var(--warn)">${denied}</b></div>`;
  $("cRep").innerHTML = reps.map(r => `<div><span>${esc(r.p.fio)}</span><span>${r.list.filter(e => e.repeat).length}</span></div>`).join("")
    || '<div style="color:var(--muted)">Повторов нет</div>';
}

function rD(newUsers) {
  $("board").innerHTML = D.meals.map(m => {
    const by = new Map();
    swipes().filter(e => e.meal === m.key).forEach(e => { if (!by.has(e.user_id)) by.set(e.user_id, []); by.get(e.user_id).push(e); });
    const rows = [...by.entries()].sort((a, b) => b[1].at(-1).ts.localeCompare(a[1].at(-1).ts));
    const expected = D.staff.filter(s => s.meals.includes(m.key)).length;
    const came = rows.filter(([, l]) => l.some(e => e.kind === "pass")).length;
    return `<div class="col"><div class="col-head ${mealCls(m.key)}"><div><h3>${esc(m.name)}</h3><small>${m.begin}–${m.end}</small></div>
      <div class="mono">${came}<small style="font-size:14px;color:var(--muted)"> / ${expected}</small></div></div>
      <div class="col-body">${rows.map(([uid, l]) => {
        const rep = l.some(e => e.repeat), n = l.filter(e => e.kind === "pass").length, d = l.length - n;
        const note = [n > 1 ? `${n} ${plural(n, "раз", "раза", "раз")}` : "", d ? `отказ ×${d}` : ""].filter(Boolean).join(", ");
        return `<div class="row ${rep ? "rep" : ""} ${newUsers.has(uid) ? "flash" : ""}"><span class="rn">${esc(person(uid, l[0].fio).fio)}</span>
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
  const newUsers = new Set(D.events.filter(e => newIds.has(e.id)).map(e => e.user_id));
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
const LINK_TEXT = {ok: "PERCo: на связи", syncing: "PERCo: передача…", offline: "Контроллер: нет связи",
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
  panel.innerHTML = `<h3><span>Заблокировано сейчас — <b class="blk-total ${total > 0 ? "some" : ""}">${total}</b></span>${BL.open.length ? '<button class="mini" data-release="all">Снять все</button>' : ""}</h3>
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
    $("dlgText").textContent = "Будут разблокированы все, кого заблокировала служба. Блокировки, поставленные операторами, не трогаются. Режим не меняется.";
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
load();
