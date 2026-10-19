'use strict';

/**
 * 共享元数据与纯逻辑 —— 主进程 / 预加载 / 渲染进程 / 测试 共用。
 * 所有排序与配色规则集中在此，避免多处实现不一致。
 */

/** 优先级五档：颜色与透明度严格按用户要求
 *  text / textMuted 是叠加在"灰黑 80%"底色之上的可读文字色
 *  accent 是左侧强调条用的高不透明度色
 */
const PRIORITY_META = {
  urgent: {
    key: 'urgent', label: '紧急', rank: 0, rgb: [139, 0, 0], alpha: 0.30,
    css: 'rgba(139, 0, 0, 0.30)', accent: 'rgba(196, 24, 24, 0.95)',
    text: '#FFE8E8', textMuted: 'rgba(255, 232, 232, 0.72)'
  },
  important: {
    key: 'important', label: '重要', rank: 1, rgb: [255, 165, 0], alpha: 0.50,
    css: 'rgba(255, 165, 0, 0.50)', accent: 'rgba(255, 176, 32, 0.95)',
    text: '#FFF4E2', textMuted: 'rgba(255, 244, 226, 0.78)'
  },
  normal: {
    key: 'normal', label: '常规', rank: 2, rgb: [126, 211, 33], alpha: 0.60,
    css: 'rgba(126, 211, 33, 0.60)', accent: 'rgba(150, 226, 62, 0.95)',
    text: '#F4FFE9', textMuted: 'rgba(244, 255, 233, 0.78)'
  },
  minor: {
    key: 'minor', label: '次要', rank: 3, rgb: [135, 206, 235], alpha: 0.70,
    css: 'rgba(135, 206, 235, 0.70)', accent: 'rgba(96, 178, 216, 0.95)',
    text: '#0A2230', textMuted: 'rgba(10, 34, 48, 0.74)'
  },
  shelved: {
    key: 'shelved', label: '搁置', rank: 4, rgb: [232, 232, 232], alpha: 0.70,
    css: 'rgba(232, 232, 232, 0.70)', accent: 'rgba(190, 190, 190, 0.95)',
    text: '#1E1E1E', textMuted: 'rgba(30, 30, 30, 0.72)'
  }
};

/** 从高到低 */
const PRIORITY_ORDER = ['urgent', 'important', 'normal', 'minor', 'shelved'];

/** 待办框底色：灰黑 80% 透明 */
const CARD_BASE_CSS = 'rgba(30, 30, 30, 0.80)';
/** 悬挂层（可拖拽/缩入把手）也用同一族色 */
const PANEL_CSS = 'rgba(24, 24, 24, 0.80)';

const DEFAULT_PRIORITY = 'normal';

function isPriorityKey(k) {
  return Object.prototype.hasOwnProperty.call(PRIORITY_META, k);
}

function normalizePriority(k) {
  return isPriorityKey(k) ? k : DEFAULT_PRIORITY;
}

function priorityRank(k) {
  const m = PRIORITY_META[normalizePriority(k)];
  return m.rank;
}

/**
 * 排序规则（用户要求）：
 * 1) 先按完成时限从上到下（升序）；
 * 2) 无时限 与 时限相同 的事项，按优先度排列。
 * 无时限项统一排在有时限项之后（视为最不紧迫）。
 */
function sortTodos(list) {
  return list.slice().sort((a, b) => {
    const ad = a && a.deadline != null ? a.deadline : null;
    const bd = b && b.deadline != null ? b.deadline : null;
    if (ad != null && bd != null) {
      if (ad !== bd) return ad - bd;
    } else if (ad != null) {
      return -1;
    } else if (bd != null) {
      return 1;
    }
    const pr = priorityRank(a && a.priority) - priorityRank(b && b.priority);
    if (pr !== 0) return pr;
    // 稳定兜底：先生成的在前
    return (a && a.createdAt ? a.createdAt : 0) - (b && b.createdAt ? b.createdAt : 0);
  });
}

function pad2(n) {
  return String(n).padStart(2, '0');
}

/** 生成时间 / 完成时限的展示格式：MM-DD HH:mm（跨年补年份） */
function formatDateTime(ts, nowTs) {
  if (ts == null) return '';
  const d = new Date(ts);
  const n = new Date(nowTs == null ? Date.now() : nowTs);
  const base = `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  if (d.getFullYear() !== n.getFullYear()) return `${d.getFullYear()}-${base}`;
  return base;
}

/** 剩余时间的人话描述 */
function describeRemain(ts, nowTs) {
  if (ts == null) return '无时限';
  const now = nowTs == null ? Date.now() : nowTs;
  let diff = ts - now;
  const overdue = diff < 0;
  diff = Math.abs(diff);
  const mins = Math.floor(diff / 60000);
  const days = Math.floor(mins / 1440);
  const hours = Math.floor((mins % 1440) / 60);
  const remMins = mins % 60;
  let text;
  if (days > 0) text = `${days}天${hours}小时`;
  else if (hours > 0) text = `${hours}小时${remMins}分`;
  else text = `${remMins}分钟`;
  return overdue ? `已超期 ${text}` : `剩 ${text}`;
}

function isOverdue(ts, nowTs) {
  if (ts == null) return false;
  const now = nowTs == null ? Date.now() : nowTs;
  return ts < now;
}

module.exports = {
  PRIORITY_META,
  PRIORITY_ORDER,
  CARD_BASE_CSS,
  PANEL_CSS,
  DEFAULT_PRIORITY,
  isPriorityKey,
  normalizePriority,
  priorityRank,
  sortTodos,
  formatDateTime,
  describeRemain,
  isOverdue,
  pad2
};
