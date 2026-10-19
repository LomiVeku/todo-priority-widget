'use strict';

/* ============================================================
   渲染进程逻辑
   ============================================================ */

const META = window.api.meta.PRIORITY_META;
const ORDER = window.api.meta.PRIORITY_ORDER;
const H = window.api.helpers;

/* ---------------------------------------------------------- 状态 */

const S = {
  todos: [],
  settings: {},
  mediaBase: '',
  showDone: false,
  remindOn: false,          // 到期提醒全局开关（settings.remindOn 持久化）
  lastRemindIds: null,      // 上一次 tick 的命中 id 集合（命中集合变化才允许再弹出一次）
  inlineEdit: null,         // 就地编辑态 { todoId, input, originalEl, original }
  pendingCompleteId: null,
  pendingConfirm: null,
  detailId: null,
  priorityTouched: false,
  deadlineTouched: false,
  dragCardId: null,
  saveBlocked: false        // 读失败（loadOk=false）时冻结保存，防止空虚状态覆盖原文件（2026-10-14）
};

/* ---------------------------------------------------------- 小工具 */

const $ = (id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

function newId() {
  return 'td_' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
}

function mediaUrl(name) {
  return S.mediaBase + '/' + encodeURIComponent(name);
}

function pad2(n) { return String(n).padStart(2, '0'); }

/** ms → datetime-local 的 value */
function toLocalInput(ts) {
  if (ts == null) return '';
  const d = new Date(ts);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

function fromLocalInput(v) {
  if (!v) return null;
  const t = new Date(v).getTime();
  return Number.isNaN(t) ? null : t;
}

let toastTimer = null;
function toast(msg, holdMs) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), holdMs || 1900);
}

/** 鼠标当前是否在窗口内 */
let mouseInside = false;

/** 当前焦点是否在可编辑控件上 */
function isEditing() {
  const ae = document.activeElement;
  const tag = ae && ae.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';
}

/* ---------------------------------------------------------- 持久化 */

let saveTimer = null;
// 保存出口闸门（2026-10-14 修复·下半）：读失败/损坏（loadOk=false → S.saveBlocked）时
// 冻结两条保存出口，否则空虚状态会在编辑或关闭时覆盖原 todos.json。
function doSave() {
  if (S.saveBlocked) return;
  window.api.save({ todos: S.todos, settings: S.settings });
}
function doSaveSync() {
  if (S.saveBlocked) return;
  window.api.saveSync({ todos: S.todos, settings: S.settings });
}
function persist() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(doSave, 140);
}

/* ---------------------------------------------------------- 渲染 */

/** 到期提醒命中：未完成 + 有明确截止时刻 + 已过截止时刻 */
function isRemindHit(t) {
  return !t.completed && t.deadline != null && H.isOverdue(t.deadline);
}

/**
 * 排序：提醒关闭时必须严格等于 H.sortTodos（sort-dom-matches-pure 契约）。
 * 提醒开启时做**渲染层叠加**：命中组排最前，组内保持 sortTodos 原序——
 * 纯逻辑排序契约（meta.js）不动。
 */
function sortedTodos() {
  const sorted = H.sortTodos(S.todos);
  if (!S.remindOn) return sorted;
  const hits = [];
  const rest = [];
  for (const t of sorted) {
    if (isRemindHit(t)) hits.push(t);
    else rest.push(t);
  }
  return hits.concat(rest);
}

function renderStats() {
  const total = S.todos.length;
  const done = S.todos.filter((t) => t.completed).length;
  $('count').textContent = String(total - done);
  $('stat').textContent = `已完成 ${done} / ${total}`;
}

function render() {
  const list = $('list');
  list.innerHTML = '';

  let items = sortedTodos();
  if (!S.showDone) items = items.filter((t) => !t.completed);

  for (const t of items) list.appendChild(cardNode(t));

  $('empty').classList.toggle('show', items.length === 0);
  renderStats();
}

function cardNode(t) {
  const m = META[t.priority] || META.normal;
  const card = el('div', 'card');
  card.dataset.id = t.id;
  card.dataset.pri = t.priority;
  card.style.setProperty('--pri', m.css);
  card.style.setProperty('--pri-accent', m.accent);
  card.style.setProperty('--pri-text', m.text);
  card.style.setProperty('--pri-muted', m.textMuted);
  // 深色底（浅色字）加字影提升可读性；浅色底（深色字）不加。
  // 第四批追加（mimo 方向，用户裁定）：双层字影——紧贴层 0 1px 2px 0.85 等效柔边描边，
  // 外层 0 0 5px 0.5 与卡底拉开层次；三档同一值。**只服务三档浅字**；
  // 浅色壁纸可读性兜底主路径（透明度 0.55 保持不变）。
  card.style.setProperty(
    '--pri-shadow',
    t.priority === 'minor' || t.priority === 'shelved'
      ? 'none'
      : '0 1px 2px rgba(0, 0, 0, 0.85), 0 0 5px rgba(0, 0, 0, 0.5)'
  );
  if (t.completed) card.classList.add('done');
  if (t.deadline != null && H.isOverdue(t.deadline) && !t.completed) card.classList.add('overdue');
  if (S.remindOn && isRemindHit(t)) card.classList.add('remind');

  /* --- 左侧快速完成按钮 --- */
  const check = el('button', 'check');
  check.title = t.completed ? '点击取消完成' : '点击标记完成（会二次确认）';
  check.setAttribute('aria-label', check.title);
  check.innerHTML =
    '<svg viewBox="0 0 12 12" width="15" height="15" aria-hidden="true"><path d="M2.2 6.3 4.7 8.8 9.8 3.2" fill="none" stroke="#fff" ' +
    'stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  check.addEventListener('click', (e) => {
    e.stopPropagation();
    askComplete(t);
  });
  // 双击圆钮不应触发卡片的"打开详情"，避免两个弹窗叠加
  check.addEventListener('dblclick', (e) => {
    e.stopPropagation();
    e.preventDefault();
  });
  card.appendChild(check);

  /* --- 主体 --- */
  const main = el('div', 'card-main');
  main.appendChild(el('div', 'card-content', t.content));

  const metaRow = el('div', 'card-meta');
  metaRow.appendChild(el('span', 'badge', m.label));

  if (t.deadline != null) {
    const overdue = H.isOverdue(t.deadline) && !t.completed;
    const dl = el('span', 'meta-item' + (overdue ? ' meta-overdue' : ''), '时限 ' + H.formatDateTime(t.deadline));
    metaRow.appendChild(dl);
    // 完成态不显示「剩/已超期」文案（mimo 正式建议④并入：降噪，与提醒命中仅未完成一致）
    const remainText = t.completed ? null : H.describeRemain(t.deadline);
    if (remainText) {
      metaRow.appendChild(el('span', 'meta-item' + (overdue ? ' meta-overdue' : ''), remainText));
    }
  } else {
    metaRow.appendChild(el('span', 'meta-item', '无时限'));
  }
  metaRow.appendChild(el('span', 'meta-item', '· 建于 ' + H.formatDateTime(t.createdAt)));

  if (t.images && t.images.length) {
    metaRow.appendChild(el('span', 'meta-item', `· ${t.images.length} 图`));
  }
  if (t.note) {
    metaRow.appendChild(el('span', 'meta-item', '· 有备注'));
  }
  main.appendChild(metaRow);

  /* --- 图片缩略图（适配窗口内展示，单击看原图） --- */
  if (t.images && t.images.length) {
    const thumbs = el('div', 'thumbs');
    for (const name of t.images) {
      const th = el('div', 'thumb');
      th.title = '单击在独立窗口查看（可拖动、滚轮缩放）';
      const img = document.createElement('img');
      img.src = mediaUrl(name);
      img.alt = '附件图片';
      img.decoding = 'async';
      th.appendChild(img);
      th.addEventListener('click', (e) => {
        e.stopPropagation();
        window.api.viewImage(name);   // 卡片缩略图单击 → 独立看图窗（第二批：替换灯箱入口）
      });
      thumbs.appendChild(th);
    }
    main.appendChild(thumbs);
  }

  card.appendChild(main);

  /* --- 右侧快捷动作 --- */
  const acts = el('div', 'card-actions');
  const del = el('button', 'mini-act');
  del.title = '删除这条待办（会二次确认）';
  del.setAttribute('aria-label', del.title);
  del.innerHTML = '<svg viewBox="0 0 16 16" width="17" height="17" aria-hidden="true"><path d="M6 2h4l.5 1H13v1.2H3V3h2.5zM4 5.4h8l-.6 8a1 1 0 0 1-1 .9H5.6a1 1 0 0 1-1-.9z"/></svg>';
  del.addEventListener('click', (e) => {
    e.stopPropagation();
    askDelete(t.id);   // 不可逆操作 → 走二次确认
  });
  acts.appendChild(del);
  card.appendChild(acts);

  /* --- 双击分发：文本区 → 就地编辑；其他区域 → 详情弹窗（点在按钮上不算） --- */
  card.addEventListener('dblclick', (e) => {
    if (e.target.closest && e.target.closest('button')) return;
    e.preventDefault();
    if (e.target.closest && e.target.closest('.card-content')) {
      startInlineEdit(t, card);
      return;
    }
    openDetail(t.id);
  });

  /* --- 右键 → 自绘优先级菜单（阻止系统菜单） --- */
  card.addEventListener('contextmenu', (e) => {
    if (S.inlineEdit) return;   // 编辑态不弹（blur 时序与菜单交互冲突，见交付报告）
    e.preventDefault();
    e.stopPropagation();
    openPriorityMenu(t, e.clientX, e.clientY);
  });

  return card;
}

/* ---------------------------------------------------------- 优先级右键菜单（自绘） */

function openPriorityMenu(t, x, y) {
  // 编辑态不弹菜单（blur 时序与菜单点击冲突的守卫；自检有断言钉住该组合）
  if (S.inlineEdit) return;
  // 有弹窗/灯箱打开时不弹（遮罩下点击语义不清晰）
  if (document.querySelector('.modal.open') || $('lightbox').classList.contains('open')) return;
  const menu = $('ctx-menu');
  menu.innerHTML = '';
  for (const key of ORDER) {
    const meta = META[key];
    const item = el('div', 'ctx-item');
    item.setAttribute('role', 'menuitem');
    item.dataset.key = key;
    // mimo 方向 4 并入：五档项各带配色点（用该档强调条色，与卡片视觉同源）
    item.appendChild(el('span', 'ctx-dot'));
    item.lastChild.style.background = meta.accent;
    item.appendChild(el('span', 'ctx-label', meta.label));
    const mark = el('span', 'ctx-check', key === t.priority ? '✓' : '');
    item.appendChild(mark);
    item.addEventListener('click', () => {
      closePriorityMenu();
      if (key !== t.priority) {
        t.priority = H.normalizePriority(key);
        upsert(t);   // persist + render（顺序跟随 H.sortTodos）
        toast(`已改为「${meta.label}」`);
      }
    });
    menu.appendChild(item);
  }
  menu.hidden = false;
  // 跟随鼠标，超出窗口边缘向内回退（不被裁掉）
  const mw = menu.offsetWidth;
  const mh = menu.offsetHeight;
  menu.style.left = Math.max(4, Math.min(x, window.innerWidth - mw - 6)) + 'px';
  menu.style.top = Math.max(4, Math.min(y, window.innerHeight - mh - 6)) + 'px';
}

function closePriorityMenu() {
  const menu = $('ctx-menu');
  menu.hidden = true;
  menu.innerHTML = '';
}

/* ---------------------------------------------------------- 就地编辑（双击文本区） */

/**
 * 进入就地编辑：把 .card-content 替换为输入框（保持原字体观感），
 * 自动聚焦。keepValue 非空时用于「否 → 回编辑态」，保留已输入内容。
 */
function startInlineEdit(t, card, keepValue, selectAll) {
  if (S.inlineEdit) return;
  const node = card.querySelector('.card-content');
  if (!node) return;
  const original = t.content == null ? '' : String(t.content);
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'inline-edit';
  input.value = keepValue != null ? keepValue : original;
  node.replaceWith(input);
  card.classList.add('editing');       // mimo 方向 3 并入：编辑态卡片抬升 + 阴影强化
  S.inlineEdit = {
    todoId: t.id,
    cardEl: card,
    input,
    originalNode: node,      // 卡片元素仍在 DOM，撤回时可直接换回
    original
  };
  input.addEventListener('blur', () => maybeConfirmEdit());
  input.addEventListener('keydown', (e) => {
    if (e.isComposing) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      maybeConfirmEdit();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      exitInlineEdit();            // Esc：放弃修改还原退出（不变更 t.content）
    }
  });
  input.focus();
  if (selectAll !== false) input.select();
}

/** 退出编辑态并重渲染（restore 撤回 / commit 保存后都走这里重建卡片） */
function exitInlineEdit() {
  S.inlineEdit = null;
  render();
}

/** 退出编辑时判定：文本（trim 后）无变化 → 静默退出；有变化 → 三按钮确认 */
function maybeConfirmEdit() {
  const ed = S.inlineEdit;
  if (!ed) return;
  $('modal-edit-confirm');    // 确保 DOM 存在（防御）
  const nowValue = ed.input.value.trim();
  if (nowValue === ed.original.trim()) {
    exitInlineEdit();         // 无变化：静默退出，不弹窗
    return;
  }
  openEditConfirm(nowValue);  // 有变化：弹三按钮确认
}

/* ---------------------------------------------------------- 三按钮确认弹窗（就地编辑专用） */

let pendingEditConfirm = null;   // { todoId, value }

function openEditConfirm(newText) {
  const ed = S.inlineEdit;
  if (!ed) return;
  if ($('modal-edit-confirm').classList.contains('open')) return;   // 防重入
  pendingEditConfirm = { todoId: ed.todoId, value: newText };
  $('edit-confirm-title').textContent = '确认修改？';
  $('edit-confirm-text').textContent = `「${ed.original}」→「${newText}」`;
  closeModal('modal-detail');
  openModal('modal-edit-confirm');
  setTimeout(() => $('btn-edit-yes').focus(), 30);
}

/** 是：保存修改（更新 t.content → 落盘 → 重渲染） */
function editConfirmYes() {
  const p = pendingEditConfirm;
  pendingEditConfirm = null;
  closeModal('modal-edit-confirm');
  if (!p) return;
  const t = S.todos.find((x) => x.id === p.todoId);
  if (t) {
    t.content = p.value;
    S.inlineEdit = null;
    upsert(t);            // persist + render
  }
}

/** 否：关闭确认弹窗，回到编辑态（保留已输入内容、焦点回编辑框） */
function editConfirmNo() {
  const p = pendingEditConfirm;
  pendingEditConfirm = null;
  closeModal('modal-edit-confirm');
  if (!p) return;
  const keep = p.value;
  const todoId = p.todoId;
  const t = S.todos.find((x) => x.id === todoId);
  S.inlineEdit = null;
  render();    // 重建卡片 DOM
  if (t) {
    const card = document.querySelector(`.card[data-id="${todoId}"]`);
    if (card) startInlineEdit(t, card, keep, false);   // 保留内容，只回焦点不重选
  }
}

/** 撤回：还原为修改前文本，退出编辑态 */
function editConfirmUndo() {
  const p = pendingEditConfirm;
  pendingEditConfirm = null;
  closeModal('modal-edit-confirm');
  S.inlineEdit = null;
  render();   // t.content 从未改动 → 重建即还原
}

/* ---------------------------------------------------------- 到期提醒（全局开关 / 置顶 / 自动弹出） */

async function maybeRemindAlert() {
  if (!S.remindOn) return;
  const ids = S.todos.filter(isRemindHit).map((t) => t.id);
  if (remindHitsChanged(S.lastRemindIds, ids)) {
    S.lastRemindIds = ids;
    // 仅「吸附 + 收起」状态自动弹出（非收起状态不打扰）；同一批命中只弹一次
    try {
      const st = await window.api.win.getState();
      if (st && st.retracted && st.dock && st.dock.side) window.api.win.expand();
    } catch (_) { /* 非致命 */ }
  }
}

/** 命中集合从无到有 / 有新增 → 视为变化（纯减少不弹） */
function remindHitsChanged(prev, now) {
  if (prev == null) return now.length > 0;
  const prevSet = new Set(prev);
  return now.some((id) => !prevSet.has(id));
}

/* ---------------------------------------------------------- 增删改 */

function upsert(todo) {
  const i = S.todos.findIndex((x) => x.id === todo.id);
  if (i >= 0) S.todos[i] = todo;
  else S.todos.push(todo);
  persist();
  render();
}

function removeTodo(id) {
  const t = S.todos.find((x) => x.id === id);
  if (!t) return;
  S.todos = S.todos.filter((x) => x.id !== id);
  if (t.images && t.images.length) {
    for (const n of t.images) window.api.removeImage(n);
  }
  persist();
  render();
  toast('已删除');
}

/* ---------------------------------------------------------- 二次确认（完成 / 删除共用） */

/** 打开确认弹窗。opts: { title, text, okText, danger, focusCancel, onOk }
 * 1.2.0 批：focusCancel=true 时默认焦点落「取消」（关闭二次确认用，慎防回车误退）；缺省行为不变（焦点在主按钮）。 */
function openConfirm(opts) {
  S.pendingConfirm = opts;
  $('confirm-title').textContent = opts.title;
  $('confirm-text').textContent = opts.text;
  const ok = $('btn-confirm-ok');
  ok.textContent = opts.okText || '确认';
  // 危险操作（删除）换危险色；沿用主按钮尺寸，只换色不换高，避免弹窗抖动
  ok.classList.toggle('danger-ok', !!opts.danger);
  closeModal('modal-detail');
  openModal('modal-confirm');
  const focusTarget = opts.focusCancel ? $('btn-confirm-cancel') : ok;
  setTimeout(() => focusTarget.focus(), 30);
}

/** 确认按钮：分派给当前挂起的操作 */
function confirmOk() {
  const p = S.pendingConfirm;
  S.pendingConfirm = null;
  closeModal('modal-confirm');
  if (p && typeof p.onOk === 'function') p.onOk();
}

/** 取消 / 点遮罩：只清状态，不执行任何操作 */
function cancelConfirm() {
  S.pendingConfirm = null;
  S.pendingCompleteId = null;
  closeModal('modal-confirm');
}

function askComplete(t) {
  if (t.completed) {
    // 取消完成：直接改回（可逆操作，不需要确认）
    t.completed = false;
    t.completedAt = null;
    upsert(t);
    toast('已恢复为未完成');
    return;
  }
  S.pendingCompleteId = t.id;
  openConfirm({
    title: '确认完成？',
    text: `确定把「${t.content}」标记为已完成？`,
    okText: '确认完成',
    onOk: confirmComplete
  });
}

function confirmComplete() {
  const t = S.todos.find((x) => x.id === S.pendingCompleteId);
  if (t) {
    t.completed = true;
    t.completedAt = Date.now();
    upsert(t);
    toast('已完成');
  }
  S.pendingCompleteId = null;
}

/** 删除是**不可逆**操作：与完成对齐走二次确认（from='detail' 时先收详情弹窗） */
function askDelete(id, from) {
  const t = S.todos.find((x) => x.id === id);
  if (!t) return;
  openConfirm({
    title: '确认删除？',
    text: `确定删除「${t.content}」？删除后不可恢复。`,
    okText: '确认删除',
    danger: true,
    onOk: () => {
      if (from === 'detail') closeModal('modal-detail');
      removeTodo(id);
    }
  });
}

/* ---------------------------------------------------------- 输入生成 */

let parseTimer = null;

function updatePreview() {
  const text = $('input-text').value.trim();
  const box = $('parse-preview');
  if (!text) {
    box.innerHTML = '';
    // 输入被清空时，同步清掉此前自动填入的时限，避免"生成"时带上陈旧时限
    if (!S.deadlineTouched) $('input-deadline').value = '';
    return;
  }
  window.api.parse(text).then((p) => {
    // 用户没手动改过 → 用解析结果回填（解析不到时清空，避免残留上一条的旧时限）
    if (!S.priorityTouched && p.priority) {
      $('input-priority').value = p.priority;
    }
    if (!S.deadlineTouched) {
      $('input-deadline').value = p.deadline != null ? toLocalInput(p.deadline) : '';
    }
    const m = META[p.priority] || META.normal;
    const dl = p.deadline != null ? H.formatDateTime(p.deadline) : '无时限';
    box.innerHTML = '';
    const b1 = el('b', null, p.content || text);
    box.appendChild(document.createTextNode('将生成：'));
    box.appendChild(b1);
    box.appendChild(document.createTextNode(` ｜ ${m.label} ｜ ${dl}`));
    // 输入里出现多个日期时明确告知采用了哪一个，避免"被吃掉的那个日期"静默丢失
    if (p.dateCount > 1 && p.deadline != null) {
      box.appendChild(el('span', 'parse-note', `（检测到 ${p.dateCount} 个日期，已采用 ${dl}）`));
    }
  });
}

function addFromInput() {
  const text = $('input-text').value.trim();
  if (!text) {
    toast('请先输入内容');
    $('input-text').focus();
    return;
  }
  window.api.parse(text).then((p) => {
    // 以本次解析结果为准，而非表单回填值：
    // ① 输入后立刻回车时，预览去抖（180ms）还没跑，表单里没有解析出的优先级/时限；
    // ② 表单里可能残留上一条输入的旧时限。
    // 仅当用户手动改过对应字段（priorityTouched/deadlineTouched）时才以表单为准。
    const todo = {
      id: newId(),
      content: (p.content || text).trim(),
      priority: S.priorityTouched ? ($('input-priority').value || 'normal') : (p.priority || 'normal'),
      deadline: S.deadlineTouched ? fromLocalInput($('input-deadline').value) : (p.deadline != null ? p.deadline : null),
      createdAt: Date.now(),
      completed: false,
      completedAt: null,
      images: [],
      note: ''
    };
    S.todos.push(todo);
    persist();
    render();

    $('input-text').value = '';
    $('input-deadline').value = '';
    $('input-priority').value = 'normal';
    S.priorityTouched = false;
    S.deadlineTouched = false;
    $('parse-preview').innerHTML = '';
    $('input-text').focus();
    toast('已生成待办');
  });
}

/* ---------------------------------------------------------- 图片导入 */

function readAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(fr.result);
    fr.onerror = () => reject(fr.error);
    fr.readAsDataURL(file);
  });
}

/* 长任务（批量附图）期间禁用触发按钮：try/finally 保证成功或失败都恢复，防重复点击 */
const BUSY_BTNS = ['btn-pick-img', 'd-add-img'];

function setBusy(on) {
  for (const id of BUSY_BTNS) {
    const b = $(id);
    if (b) b.disabled = on;
  }
  document.body.classList.toggle('busy', on);
}

/** 按用户拖入的**原始顺序**逐张导入（不区分路径/dataURL，避免顺序错乱） */
async function importFiles(fileList) {
  const files = Array.from(fileList || []).filter((f) => f.type && f.type.startsWith('image/'));
  const names = [];
  for (const f of files) {
    let p = null;
    try { p = window.api.getPathForFile(f); } catch (_) { p = null; }
    try {
      const results = p
        ? await window.api.addImagePaths([p])
        : await window.api.addImageDataUrls([await readAsDataURL(f)]);
      for (const r of results) if (r && r.ok) names.push(r.name);
    } catch (_) { /* 单张失败不影响其余 */ }
  }
  return names;
}

async function attachFilesToTodo(id, fileList) {
  const t = S.todos.find((x) => x.id === id);
  if (!t) return;
  setBusy(true);
  try {
    const names = await importFiles(fileList);
    if (!names.length) {
      toast('没有可用的图片');
      return;
    }
    t.images = (t.images || []).concat(names);
    upsert(t);
    toast(`已加入 ${names.length} 张图片`);
    if (S.detailId === id) renderDetailImages();
  } finally {
    setBusy(false);
  }
}

async function createTodoWithFiles(fileList) {
  setBusy(true);
  try {
    const names = await importFiles(fileList);
    if (!names.length) {
      toast('没有检测到可用的图片文件');
      return;
    }
    const todo = {
      id: newId(),
      content: '（请补充待办内容）',
      priority: 'normal',
      deadline: null,
      createdAt: Date.now(),
      completed: false,
      completedAt: null,
      images: names,
      note: ''
    };
    S.todos.push(todo);
    persist();
    render();
    toast('已按图片新建待办，请补充内容');
    openDetail(todo.id);
    setTimeout(() => { $('d-content').focus(); $('d-content').select(); }, 120);
  } finally {
    setBusy(false);
  }
}

/* ---------------------------------------------------------- 拖放 */

function findCardAt(x, y) {
  const e = document.elementFromPoint(x, y);
  const card = e && e.closest ? e.closest('.card') : null;
  return card ? card.dataset.id : null;
}

function clearDragHighlight() {
  document.querySelectorAll('.card.dragging-over').forEach((c) => c.classList.remove('dragging-over'));
}

function hasFiles(dt) {
  if (!dt) return false;
  if (dt.types) {
    for (const t of dt.types) if (t === 'Files') return true;
  }
  return false;
}

function bindDnd() {
  let depth = 0;
  const overlay = $('drop-overlay');

  document.addEventListener('dragenter', (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    e.preventDefault();
    depth += 1;
    overlay.classList.add('show');
  });

  document.addEventListener('dragover', (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
    const id = findCardAt(e.clientX, e.clientY);
    clearDragHighlight();
    if (id) {
      const node = document.querySelector(`.card[data-id="${id}"]`);
      if (node) node.classList.add('dragging-over');
    }
  });

  document.addEventListener('dragleave', (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0) {
      overlay.classList.remove('show');
      clearDragHighlight();
    }
  });

  document.addEventListener('drop', async (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    e.preventDefault();
    depth = 0;
    overlay.classList.remove('show');
    clearDragHighlight();
    const id = findCardAt(e.clientX, e.clientY);
    const files = e.dataTransfer.files;
    if (id) await attachFilesToTodo(id, files);
    else await createTodoWithFiles(files);
  });

  const fileInput = $('file-input');
  fileInput.addEventListener('change', async () => {
    if (fileInput.files && fileInput.files.length) await createTodoWithFiles(fileInput.files);
    fileInput.value = '';
  });

  const fileInputDetail = $('file-input-detail');
  fileInputDetail.addEventListener('change', async () => {
    if (fileInputDetail.files && fileInputDetail.files.length && S.detailId) {
      await attachFilesToTodo(S.detailId, fileInputDetail.files);
    }
    fileInputDetail.value = '';
  });
}

/* ---------------------------------------------------------- 详情弹窗 */

function openDetail(id) {
  const t = S.todos.find((x) => x.id === id);
  if (!t) return;
  closeModal('modal-confirm');
  S.pendingCompleteId = null;
  S.detailId = id;
  $('d-content').value = t.content || '';
  $('d-priority').value = t.priority || 'normal';
  $('d-deadline').value = toLocalInput(t.deadline);
  $('d-note').value = t.note || '';
  $('d-meta').textContent =
    `生成时间：${new Date(t.createdAt).toLocaleString('zh-CN', { hour12: false })}` +
    (t.completed && t.completedAt
      ? `　完成时间：${new Date(t.completedAt).toLocaleString('zh-CN', { hour12: false })}`
      : '　状态：未完成');
  renderDetailImages();
  openModal('modal-detail');
}

function renderDetailImages() {
  const t = S.todos.find((x) => x.id === S.detailId);
  const box = $('d-images');
  box.innerHTML = '';
  if (!t || !t.images || !t.images.length) {
    box.appendChild(el('div', 'field-label', '暂无图片'));
    return;
  }
  for (const name of t.images) {
    const th = el('div', 'thumb');
    th.title = '单击在独立窗口查看（可拖动、滚轮缩放），右键移除';
    const img = document.createElement('img');
    img.src = mediaUrl(name);
    img.alt = '附件图片';
    th.appendChild(img);
    th.addEventListener('click', (e) => { e.stopPropagation(); window.api.viewImage(name); }); // 详情缩略图 → 独立看图窗（统一体验）
    th.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      e.stopPropagation();
      t.images = t.images.filter((x) => x !== name);
      window.api.removeImage(name);
      upsert(t);
      renderDetailImages();
      toast('已移除图片');
    });
    box.appendChild(th);
  }
}

function saveDetail() {
  const t = S.todos.find((x) => x.id === S.detailId);
  if (!t) { closeModal('modal-detail'); return; }
  const content = $('d-content').value.trim();
  t.content = content || t.content;
  t.priority = $('d-priority').value || 'normal';
  t.deadline = fromLocalInput($('d-deadline').value);
  t.note = $('d-note').value;
  upsert(t);
  toast('已保存');
  closeModal('modal-detail');
}

function deleteDetail() {
  const id = S.detailId;
  if (id) askDelete(id, 'detail');   // 不可逆操作 → 走二次确认
}

/* ---------------------------------------------------------- 原图查看 */

function openLightbox(name) {
  const img = $('lightbox-img');
  img.src = mediaUrl(name);
  $('lightbox-name').textContent = name;
  $('lightbox-size').textContent = '';
  img.onload = () => {
    $('lightbox-size').textContent = `${img.naturalWidth} × ${img.naturalHeight} px`;
  };
  // 默认按窗口适配展示（用户 2026-10-13 裁决），可切原始大小
  $('lightbox').classList.add('fit');
  $('lightbox').classList.add('open');
  $('btn-lightbox-fit').textContent = '原始大小';
}

function closeLightbox() {
  $('lightbox').classList.remove('open');
  $('lightbox-img').src = '';
}

function toggleLightboxFit() {
  const box = $('lightbox');
  const fit = box.classList.toggle('fit');
  $('btn-lightbox-fit').textContent = fit ? '原始大小' : '适应窗口';
  $('lightbox-stage').scrollTop = 0;
  $('lightbox-stage').scrollLeft = 0;
}

/* ---------------------------------------------------------- 弹窗控制 */

function openModal(id) { $(id).classList.add('open'); }
function closeModal(id) { $(id).classList.remove('open'); }

function closeTopModal() {
  if ($('lightbox').classList.contains('open')) { closeLightbox(); return true; }
  if ($('modal-detail').classList.contains('open')) { closeModal('modal-detail'); return true; }
  if ($('modal-confirm').classList.contains('open')) { closeModal('modal-confirm'); S.pendingCompleteId = null; return true; }
  return false;
}

/* ---------------------------------------------------------- 窗口行为 */

function bindWindow() {
  $('btn-min').addEventListener('click', () => window.api.win.minimize());
  $('btn-close').addEventListener('click', () => window.api.win.close());

  $('btn-top').addEventListener('click', async () => {
    const v = await window.api.win.toggleTop();
    $('btn-top').classList.toggle('active', v);
    toast(v ? '已置顶' : '取消置顶');
  });

  $('btn-dock').addEventListener('click', async () => {
    const on = !$('btn-dock').classList.contains('active');
    await window.api.win.setDockEnabled(on);
    $('btn-dock').classList.toggle('active', on);
    toast(on ? '已开启边缘吸附' : '已关闭边缘吸附');
  });

  $('btn-data-dir').addEventListener('click', () => window.api.win.openDataDir());

  $('dock-handle').addEventListener('click', () => window.api.win.expand());

  // 鼠标进出 → 展开 / 缩回
  // 正在输入时不缩回（避免打字时窗口跑掉）；输入结束且鼠标已在窗外时补一次缩回判定
  document.addEventListener('mouseenter', () => {
    mouseInside = true;
    window.api.win.hover(true);
  });
  document.addEventListener('mouseleave', () => {
    mouseInside = false;
    if (isEditing()) return;
    window.api.win.hover(false);
  });
  document.addEventListener('focusout', () => {
    setTimeout(() => {
      if (!isEditing() && !mouseInside) window.api.win.hover(false);
    }, 200);
  });

  window.api.onWinState((st) => {
    $('btn-top').classList.toggle('active', !!st.alwaysOnTop);
    $('btn-dock').classList.toggle('active', !!st.dockEnabled);
  });

  window.api.onDockState((st) => {
    document.body.classList.toggle('docked', !!st.side);
    document.body.classList.toggle('dock-left', st.side === 'left');
    document.body.classList.toggle('dock-right', st.side === 'right');
    document.body.classList.toggle('dock-top', st.side === 'top');
  });

  // 1.2.0 批：关闭二次确认。X / Alt+F4 / WM_CLOSE → Go 推 win:close-request 到这里；
  // 确认才回传 win:close-confirmed，取消只关弹窗。托盘「退出」保持直关、不弹确认。
  window.api.onCloseRequest(() => {
    openConfirm({
      title: '退出软件？',
      text: '退出后到期提醒将停止，确定退出吗？',
      okText: '退出',
      focusCancel: true, // 默认焦点在「取消」，防回车误退
      onOk: () => window.api.win.closeConfirmed()
    });
  });
}

/* ---------------------------------------------------------- 事件绑定 */

function bindUi() {
  $('btn-add').addEventListener('click', addFromInput);
  $('input-text').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); addFromInput(); }
  });
  $('input-text').addEventListener('input', () => {
    clearTimeout(parseTimer);
    parseTimer = setTimeout(updatePreview, 180);
  });
  $('input-priority').addEventListener('change', () => { S.priorityTouched = true; });
  $('input-deadline').addEventListener('change', () => { S.deadlineTouched = true; });

  $('btn-pick-img').addEventListener('click', () => $('file-input').click());

  $('chk-show-done').addEventListener('change', (e) => {
    S.showDone = e.target.checked;
    S.settings.showDone = S.showDone;
    persist();
    render();
  });

  // 到期提醒开关：状态入 settings 持久化（重启保留）；开启瞬间若已有命中 → 置顶高亮 + 触发一次弹出判定
  $('chk-remind').addEventListener('change', (e) => {
    S.remindOn = e.target.checked;
    S.settings.remindOn = S.remindOn;
    S.lastRemindIds = null;   // 开关切换 = 命中集合重构，允许重新判弹
    persist();
    render();
    maybeRemindAlert();
  });

  // 三按钮确认弹窗（是 / 否 / 撤回）
  $('btn-edit-yes').addEventListener('click', editConfirmYes);
  $('btn-edit-no').addEventListener('click', editConfirmNo);
  $('btn-edit-undo').addEventListener('click', editConfirmUndo);

  // 右键菜单点别处关闭（捕获阶段，先于菜单内 click）
  document.addEventListener('mousedown', (e) => {
    const menu = $('ctx-menu');
    if (!menu.hidden && !(e.target.closest && e.target.closest('#ctx-menu'))) closePriorityMenu();
  }, true);

  $('btn-confirm-ok').addEventListener('click', confirmOk);
  $('btn-confirm-cancel').addEventListener('click', cancelConfirm);

  $('btn-d-save').addEventListener('click', saveDetail);
  $('btn-d-cancel').addEventListener('click', () => closeModal('modal-detail'));
  $('btn-d-delete').addEventListener('click', deleteDetail);
  $('d-add-img').addEventListener('click', () => $('file-input-detail').click());

  $('btn-lightbox-close').addEventListener('click', closeLightbox);
  $('btn-lightbox-fit').addEventListener('click', toggleLightboxFit);

  // 二次确认弹窗可以直接点遮罩取消；
  // 详情弹窗**不允许**点遮罩关闭 —— 避免误点丢掉落编辑内容，只能走「取消 / 保存」
  $('modal-confirm').addEventListener('mousedown', (e) => {
    if (e.target === $('modal-confirm')) cancelConfirm();
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      // 三按钮弹窗 → Esc = 撤回（还原退出编辑）
      if ($('modal-edit-confirm').classList.contains('open')) { editConfirmUndo(); e.preventDefault(); }
      else if (closeTopModal()) e.preventDefault();
      // 右键菜单 → Esc 关闭
      else if (!$('ctx-menu').hidden) { closePriorityMenu(); e.preventDefault(); }
    } else if (e.key === 'Enter') {
      // 三按钮弹窗 → Enter = 是（保存）
      if ($('modal-edit-confirm').classList.contains('open')) { e.preventDefault(); editConfirmYes(); }
      else if (e.ctrlKey || e.metaKey) {
        if ($('modal-detail').classList.contains('open')) { e.preventDefault(); saveDetail(); }
      }
    }
  });

  // 定时刷新"剩余时间" + 到期提醒命中检测（复用本 interval，不新增独立定时器）
  setInterval(async () => {
    if (!document.hidden) {
      const hasDeadline = S.todos.some((t) => !t.completed && t.deadline != null);
      if (hasDeadline) render();
    }
    await maybeRemindAlert();
  }, 60000);
}

/* ---------------------------------------------------------- 自检辅助（仅自检模式调用） */

window.__selftest = function (opts) {
  opts = opts || {};
  const now = Date.now();
  const HOUR = 3600000;
  const DAY = 24 * HOUR;
  const imgs = opts.imgName ? [opts.imgName] : [];

  S.todos = [
    { id: 'a1', content: '提交季度报表给财务', priority: 'urgent', deadline: now + 3 * HOUR, createdAt: now - 26 * HOUR, completed: false, images: [], note: '' },
    { id: 'a2', content: '和客户确认下周排期', priority: 'important', deadline: now + 30 * HOUR, createdAt: now - 5 * HOUR, completed: false, images: [], note: '' },
    { id: 'a3', content: '整理设计稿并归档', priority: 'normal', deadline: now + 3 * DAY, createdAt: now - 2 * HOUR, completed: false, images: imgs, note: '参考上次评审意见' },
    { id: 'a4', content: '补充用户调研问卷', priority: 'normal', deadline: now + 3 * DAY, createdAt: now - 1 * HOUR, completed: false, images: [], note: '' },
    { id: 'a5', content: '回复产品群消息', priority: 'minor', deadline: now + 8 * DAY, createdAt: now - 30 * 60000, completed: false, images: [], note: '' },
    { id: 'a6', content: '重新整理旧项目备份', priority: 'shelved', deadline: null, createdAt: now - 60 * 60000, completed: false, images: [], note: '' },
    { id: 'a7', content: '买咖啡豆', priority: 'urgent', deadline: null, createdAt: now - 10 * 60000, completed: false, images: [], note: '' },
    { id: 'a8', content: '已完成的示例：交周报', priority: 'normal', deadline: null, createdAt: now - 50 * HOUR, completed: true, completedAt: now - 2 * HOUR, images: [], note: '' }
  ];

  S.showDone = true;
  $('chk-show-done').checked = true;
  render();
  return { count: S.todos.length };
};

/* 写回护栏探针（2026-10-14）：验证 S.saveBlocked 对两条保存出口的冻结 + 解锁恢复。
   同步返回，供 evalJSON 直接断言：期望 {"blocked":[0,0],"unblocked":[1,1]}。 */
window.__selftestSaveGuardProbe = function () {
  const oAsync = window.api.save;
  const oSync = window.api.saveSync;
  let nAsync = 0;
  let nSync = 0;
  window.api.save = function () { nAsync += 1; return oAsync.apply(window.api, arguments); };
  window.api.saveSync = function () { nSync += 1; return oSync.apply(window.api, arguments); };
  const prev = S.saveBlocked;
  try {
    S.saveBlocked = true;
    doSave();
    doSaveSync();
    const blocked = [nAsync, nSync];
    S.saveBlocked = false;
    doSave();
    doSaveSync();
    return { blocked: blocked, unblocked: [nAsync, nSync] };
  } finally {
    S.saveBlocked = prev;
    window.api.save = oAsync;
    window.api.saveSync = oSync;
  }
};

window.__selftestDiag = function () {
  const cards = Array.from(document.querySelectorAll('.card'));
  return {
    versionBadge: ($('version') || {}).textContent,
    domOrder: cards.map((c) => c.dataset.id),
    cards: cards.map((c) => {
      const cs = getComputedStyle(c);
      return {
        id: c.dataset.id,
        pri: c.dataset.pri,
        content: (c.querySelector('.card-content') || {}).textContent,
        backgroundColor: cs.backgroundColor,
        backgroundImage: cs.backgroundImage,
        borderLeftColor: cs.borderLeftColor
      };
    }),
    pureSorted: H.sortTodos(S.todos).map((t) => t.id)
  };
};

window.__selftestOpenConfirm = function () {
  const t = S.todos.find((x) => x.id === 'a1');
  askComplete(t);
  return t && t.id;
};

/* 美术优化轮新增：打开"删除二次确认"供自检核对文案与危险色 */
window.__selftestOpenDeleteConfirm = function () {
  const t = S.todos.find((x) => x.id === 'a2');
  if (!t) return null;
  askDelete(t.id);
  return t.id;
};

window.__selftestOpenDetail = function () {
  openDetail('a3');
  return 'a3';
};

window.__selftestOpenLightbox = function () {
  const t = S.todos.find((x) => x.images && x.images.length);
  if (t) openLightbox(t.images[0]);
  return t && t.images[0];
};

/* [GLM 复核] 合成拖放：拖到空白处 = 新建带图待办（实测拖放与图片导入管线） */
window.__selftestDropImage = async function () {
  const before = S.todos.length;
  const bytes = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]);
  const file = new File([bytes], 'selftest-drop.png', { type: 'image/png' });
  const dt = new DataTransfer();
  dt.items.add(file);
  const tb = document.querySelector('.titlebar').getBoundingClientRect();
  const ev = new DragEvent('drop', {
    bubbles: true, cancelable: true, dataTransfer: dt,
    clientX: tb.left + Math.min(20, tb.width / 2), clientY: tb.top + 5
  });
  document.dispatchEvent(ev);
  await new Promise((r) => setTimeout(r, 800));
  const created = S.todos.length === before + 1 ? S.todos[S.todos.length - 1] : null;
  const ok = !!created && Array.isArray(created.images) && created.images.length === 1;
  if (created) {
    S.todos = S.todos.filter((t) => t !== created);
    for (const n of (created.images || [])) window.api.removeImage(n);
    persist();
    render();
  }
  return { ok, created: created ? { content: created.content, images: created.images } : null };
};

/* [GLM 复核] 输入后立刻生成的回归验证（预览去抖未触发 / 表单残留旧时限两种情形） */
window.__selftestAddFromInput = async function () {
  // 场景1：输入后立刻生成（预览去抖 180ms 未触发）→ 应采用解析出的优先级/时限
  $('input-text').value = '明天下午3点交周报 紧急';
  $('input-deadline').value = '';
  addFromInput();
  await new Promise((r) => setTimeout(r, 600));
  const t1 = S.todos[S.todos.length - 1];
  const d1 = t1 && t1.deadline != null ? new Date(t1.deadline) : null;
  const expected = new Date();
  expected.setDate(expected.getDate() + 1);
  const ok1 = !!t1 && t1.priority === 'urgent' && !!d1 &&
    d1.getHours() === 15 && d1.toDateString() === expected.toDateString();

  // 场景2：无日期文本 + 表单残留旧时限 → 不得带上陈旧时限
  $('input-deadline').value = '2020-01-01T10:00';
  $('input-text').value = '买牛奶';
  addFromInput();
  await new Promise((r) => setTimeout(r, 600));
  const t2 = S.todos[S.todos.length - 1];
  const ok2 = !!t2 && t2.content === '买牛奶' && t2.deadline == null;

  // 清理演示数据并复位输入区
  S.todos = S.todos.filter((t) => t !== t1 && t !== t2);
  $('input-text').value = '';
  $('input-deadline').value = '';
  $('input-priority').value = 'normal';
  S.priorityTouched = false;
  S.deadlineTouched = false;
  $('parse-preview').innerHTML = '';
  render();
  return {
    ok1, ok2,
    t1: t1 ? { priority: t1.priority, deadline: t1.deadline, content: t1.content } : null,
    t2: t2 ? { content: t2.content, deadline: t2.deadline } : null
  };
};

/* 本轮新增：优先级右键菜单自检钩子（开菜单 + 读菜单状态） */
window.__selftestOpenPriMenu = function () {
  const card = document.querySelector('.card[data-id="a1"]');
  const t = S.todos.find((x) => x.id === 'a1');
  if (!card || !t) return null;
  openPriorityMenu(t, 60, 80);
  return true;
};

window.__selftestPriMenuInfo = function () {
  const menu = $('ctx-menu');
  const open = !menu.hidden;
  const items = Array.from(menu.querySelectorAll('.ctx-item')).map((it) => it.dataset.key);
  const dots = Array.from(menu.querySelectorAll('.ctx-dot')).map((d) => d.style.background);
  const cur = document.querySelector('.card[data-id="a1"]').dataset.pri;
  const curItem = menu.querySelector(`.ctx-item[data-key="${cur}"]`);
  const curCheck = curItem ? curItem.querySelector('.ctx-check') : null;
  const menuBg = getComputedStyle(menu).backgroundColor;
  return { open, items, current: cur, checkedMark: curCheck ? curCheck.textContent : null,
    checkColor: curCheck ? getComputedStyle(curCheck).color : null, dots, menuBg };
};

window.__selftestClickPriItem = function (key) {
  const item = document.querySelector(`#ctx-menu .ctx-item[data-key="${key}"]`);
  if (!item) return null;
  item.click();
  return key;
};

/* 本轮新增：就地编辑自检钩子（进入 / 读状态 / 触发退出判定） */
window.__selftestStartEdit = function (id, value) {
  const t = S.todos.find((x) => x.id === id);
  const card = document.querySelector(`.card[data-id="${id}"]`);
  if (!t || !card) return null;
  startInlineEdit(t, card, value == null ? null : value, true);
  return { ok: true, original: t.content };
};

window.__selftestEditState = function () {
  const ed = S.inlineEdit;
  if (!ed || !ed.input) return { editing: false, modalOpen: $('modal-edit-confirm').classList.contains('open') };
  const cs = getComputedStyle(ed.input);
  const cardCs = ed.cardEl ? getComputedStyle(ed.cardEl) : null;
  return {
    editing: true,
    todoId: ed.todoId,
    focused: document.activeElement === ed.input,
    value: ed.input.value,
    original: ed.original,
    modalOpen: $('modal-edit-confirm').classList.contains('open'),
    inputBg: cs.backgroundColor,
    inputBorder: cs.borderColor,
    cardEditing: ed.cardEl ? ed.cardEl.classList.contains('editing') : null,
    cardShadow: cardCs ? cardCs.boxShadow : null
  };
};

window.__selftestEndEdit = function () {
  maybeConfirmEdit();
  return window.__selftestEditState();
};

window.__selftestEditModalInfo = function () {
  const m = $('modal-edit-confirm');
  return {
    open: m.classList.contains('open'),
    text: $('edit-confirm-text').textContent,
    yes: $('btn-edit-yes').textContent.trim(),
    no: $('btn-edit-no').textContent.trim(),
    undo: $('btn-edit-undo').textContent.trim(),
    icons: ['btn-edit-yes', 'btn-edit-no', 'btn-edit-undo'].map((id) => !!$(id).querySelector('svg')),
    focused: document.activeElement ? document.activeElement.id : null
  };
};

/* 本轮新增：到期提醒自检钩子（设置开关 + 可选制造过期项，返回当前命中） */
window.__selftestSetRemind = function (on, makeOverdueFor) {
  const t = S.todos.find((x) => x.id === makeOverdueFor);
  if (t) t.deadline = Date.now() - 5000;   // 刚刚过期 → 命中
  S.settings.remindOn = on;
  S.remindOn = on;
  $('chk-remind').checked = on;
  S.lastRemindIds = null;                  // 勾选框语义：开关切换 = 命中集合重构
  render();
  if (on) maybeRemindAlert();              // 与真实勾选框 change 事件一致：开启瞬间判弹
  return { on: S.remindOn, hits: S.todos.filter(isRemindHit).map((x) => x.id) };
};

window.__selftestRemindState = function () {
  const order = Array.from(document.querySelectorAll('.card')).map((c) => c.dataset.id);
  return {
    checked: $('chk-remind').checked,
    hits: S.todos.filter(isRemindHit).map((x) => x.id),
    domOrder: order,
    pureSorted: H.sortTodos(S.todos).map((t) => t.id),
    firstCardRemind: (() => {
      const c = document.querySelector('.card');
      return c ? c.classList.contains('remind') : null;
    })(),
    firstCardIsHit: (() => {
      const c = document.querySelector('.card');
      if (!c) return null;
      const t = S.todos.find((x) => x.id === c.dataset.id);
      return isRemindHit(t);
    })()
  };
};

/* 通用小钩子：读单条待办字段 / 补丁时限 / 提醒去重逻辑直测 */
window.__selftestTodoPri = function (id) {
  const t = S.todos.find((x) => x.id === id);
  return t ? t.priority : null;
};
window.__selftestTodoContent = function (id) {
  const t = S.todos.find((x) => x.id === id);
  return t ? t.content : null;
};
/* hours 为小时数（可负、可小数）；传 '-1' 还原为 null（无时限） */
window.__selftestPatchDeadline = function (id, hours) {
  const t = S.todos.find((x) => x.id === id);
  if (!t) return null;
  t.deadline = hours === '-1' ? null : Date.now() + parseFloat(hours) * 3600000;
  render();
  return t.deadline;
};
window.__selftestRemindChanged = function (prev, now) {
  return remindHitsChanged(prev, now);
};
/* 模拟一次 tick 的提醒检测（不重置已弹标记——用于验证"同一批只弹一次"） */
window.__selftestTickRemind = function () {
  maybeRemindAlert();
  return S.lastRemindIds;
};

/* ---------------------------------------------------------- 启动 */

async function boot() {
  const data = await window.api.load();
  S.todos = data.todos || [];
  S.settings = data.settings || {};
  S.mediaBase = data.mediaBase;
  S.showDone = !!S.settings.showDone;
  $('chk-show-done').checked = S.showDone;
  S.remindOn = !!S.settings.remindOn;
  $('chk-remind').checked = S.remindOn;

  // 清理媒体目录里已无人引用的图片。
  // 安全护栏：数据文件损坏时绝不能回收 —— 否则"加载失败→引用集为空→删光用户图片"。
  if (data.loadOk !== false) {
    try {
      const refs = [];
      for (const t of S.todos) if (Array.isArray(t.images)) refs.push(...t.images);
      await window.api.gcImages(refs);
    } catch (_) { /* 非致命 */ }
  } else {
    console.warn('[boot] 数据文件读取异常，已跳过图片回收');
  }

  // 写回护栏（2026-10-14 修复·下半）：读失败时冻结保存出口，防止空状态覆盖原文件
  S.saveBlocked = (data.loadOk === false);
  if (S.saveBlocked) {
    toast('数据文件读取失败，已暂停保存以保护原文件', 8000);
  }

  // 关闭前同步落盘，避免去抖窗口内的最后一次编辑丢失
  window.addEventListener('beforeunload', () => {
    try {
      clearTimeout(saveTimer);
      doSaveSync();
    } catch (_) { /* ignore */ }
  });

  bindUi();
  bindWindow();
  bindDnd();
  render();

  const st = await window.api.win.getState();
  $('btn-top').classList.toggle('active', !!st.alwaysOnTop);
  $('btn-dock').classList.toggle('active', !!st.dockEnabled);

  // 版本号徽章与 package.json 保持一致
  try {
    const info = await window.api.appInfo();
    if (info && info.version) $('version').textContent = 'v' + info.version;
    S.appInfo = info;
  } catch (_) { /* ignore */ }
}

boot();
