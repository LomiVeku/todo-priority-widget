'use strict';

/**
 * 自然语言待办解析器（纯函数，无外部依赖，可单测）
 *
 * 输入："明天下午3点前交周报 紧急"
 * 输出：{ content: '交周报', priority: 'urgent', deadline: <ts>, matched: [...] }
 */

const { DEFAULT_PRIORITY } = require('../shared/meta');

/* ---------------------------------------------------------------- 中文数字 */

const CN_DIGIT = { 零: 0, 〇: 0, 一: 1, 二: 2, 两: 2, 三: 3, 四: 4, 五: 5, 六: 6, 七: 7, 八: 8, 九: 9 };
const CN_NUM_SRC = '[零〇一二两三四五六七八九十]';

function cnToNum(raw) {
  if (raw == null) return NaN;
  const s = String(raw).trim();
  if (s === '') return NaN;
  if (/^\d+$/.test(s)) return parseInt(s, 10);
  let total = 0;
  let section = 0;
  for (const ch of s) {
    if (ch === '十') {
      section = (section === 0 ? 1 : section) * 10;
      total += section;
      section = 0;
    } else if (CN_DIGIT[ch] != null) {
      section = CN_DIGIT[ch];
    } else {
      return NaN;
    }
  }
  return total + section;
}

/* ---------------------------------------------------------------- 优先级 */

/**
 * 优先级关键词表。
 * 注意：英文关键词只保留语义明确的（asap/urgent），并强制词边界；
 * 不收录 high/low/later 这类过于泛化的词，否则会误吃正文（如 "high level design"）。
 */
const PRIORITY_KEYWORDS = [
  { key: 'urgent', words: ['十万火急', '火烧眉毛', '刻不容缓', '紧急', '加急', '急件', '急事', '立刻', '立即', '马上', '尽快', 'asap', 'urgent', '急'] },
  { key: 'important', words: ['重要', '关键', '优先', '重点', '要紧'] },
  { key: 'minor', words: ['次要', '可选', '顺手', '不着急', '别着急', '低优先', '有空再说', '有空'] },
  { key: 'shelved', words: ['搁置', '暂缓', '延后', '待定', '以后再说', '再说', '挂起', '暂停'] },
  { key: 'normal', words: ['常规', '普通', '一般'] }
];

/** 纯 ASCII 关键词必须落在词边界上，避免 highland 命中 high 之类 */
const ASCII_WORD_RE = /^[A-Za-z]+$/;

function isBoundaryOk(text, start, end, word) {
  if (!ASCII_WORD_RE.test(word)) return true;
  const isWordChar = (c) => c !== '' && /[A-Za-z0-9_-]/.test(c);
  const before = start > 0 ? text[start - 1] : '';
  const after = end < text.length ? text[end] : '';
  return !isWordChar(before) && !isWordChar(after);
}

/**
 * 易误伤关键词的上下文守卫。
 * 例：单字「急」在「我很着急」里不是优先级标记，不能剔除，否则正文被吃成「我很着」。
 */
const CONTEXT_GUARD = {
  // notBefore 比对的是「紧邻的前一个字」：'不急'/'别急' 里的「急」不是优先级标记。
  // （旧实现写了 2 字的 '不急'，但比对窗口只有 1 个字，属于死代码，导致"不急"被误判为紧急）
  急: { notBefore: ['着', '不', '别'], notAfter: ['诊', '躁', '促', '性子'] }
};

/* ---------------------------------------------------------------- 位置感知（修复批，用户 2026-10-13 裁决）

   依据用户实机反馈：「不紧急」被抽走「紧急」误判档；「暂时搁置」抽走「搁置」剩悬空「暂时」；
   「较重要」抽走「重要」剩悬空「较」。关键词不能一刀切，要按**紧邻前缀**分治：

   1) 否定前缀 → 命中作废（档位、正文都不动：「不紧急」整词保留，与旧有「不急」语义一致）；
      同时同区间内的**子串命中一并作废**（「不加急」里「加急」被守卫后，「急」不得单独复活）。
   2) 程度副词前缀 → 剔除范围向左扩包含前缀（「较重要」整段剔走，档位仍 important）。
   3) 搁置族时间前缀 → 剔除范围向左扩包含前缀（「暂时搁置设备修理」→「设备修理」+ shelved）。
   前缀表最长优先匹配（否则「较」截胡「比较」）。 */
const NEGATION_PREFIXES = ['不用', '不要', '无需', '不必', '不太', '不是很', '不', '别', '没', '未', '非'];
const DEGREE_PREFIXES = ['比较', '有点', '有些', '稍微', '略微', '较', '更', '挺', '略'];
const SHELVE_TIME_PREFIXES = ['暂时', '暂且', '临时', '先'];

/** 取紧邻前缀：若 start 前的文本以 prefixes 中某项结尾则返回该项（最长优先），否则 '' */
function immediatePrefix(text, start, prefixes) {
  const head = text.slice(0, start);
  let best = '';
  for (const p of prefixes) {
    if (p.length > best.length && head.endsWith(p)) best = p;
  }
  return best;
}

function isFirstCharCJK(word) {
  return !ASCII_WORD_RE.test(word);
}

/** 扫描优先级关键词；长词优先，返回主命中与全部命中（供全量剔除）。
 *  修复批：位置感知三规则（否定作废/程度前缀左扩/搁置时间前缀左扩）。
 *  **否定的覆盖语义**：命中被否定前缀作废时记录作废区间[start-neg, end]，
 *  落在该区间内的**任何**命中（含子串，如「不加急」里的「急」）一律不得存活/当选——
 *  否定是整段语义，不允许子串换个位置复活。 */
function scanPriority(text) {
  const hits = [];
  const negatedRanges = [];
  for (const group of PRIORITY_KEYWORDS) {
    for (const w of group.words) {
      let from = 0;
      for (;;) {
        const idx = text.indexOf(w, from);
        if (idx < 0) break;
        from = idx + 1;
        const end = idx + w.length;
        if (!isBoundaryOk(text, idx, end, w)) continue;
        const guard = CONTEXT_GUARD[w];
        if (guard) {
          const before = idx > 0 ? text[idx - 1] : '';
          const after = end < text.length ? text[end] : '';
          if (guard.notBefore && guard.notBefore.includes(before)) { negatedRanges.push([idx, end]); continue; }
          if (guard.notAfter && guard.notAfter.includes(after)) { negatedRanges.push([idx, end]); continue; }
        }
        // —— 位置感知：仅中文词参与（ASCII 词已有词边界保护）——
        if (isFirstCharCJK(w)) {
          // 规则 1：否定前缀 → 作废并记录区间（同区间子串不再复活）
          const neg = immediatePrefix(text, idx, NEGATION_PREFIXES);
          if (neg) {
            negatedRanges.push([idx - neg.length, end]);
            continue;
          }
          // 规则 2：程度副词 → 剔除区间左扩包含前缀
          const deg = immediatePrefix(text, idx, DEGREE_PREFIXES);
          if (deg) {
            hits.push({ key: group.key, word: deg + w, start: idx - deg.length, end, absorbed: deg });
            continue;
          }
          // 规则 3：搁置族时间前缀 → 剔除区间左扩包含前缀（仅 shelved 组词）
          if (group.key === 'shelved') {
            const tm = immediatePrefix(text, idx, SHELVE_TIME_PREFIXES);
            if (tm) {
              hits.push({ key: group.key, word: tm + w, start: idx - tm.length, end, absorbed: tm });
              continue;
            }
          }
        }
        hits.push({ key: group.key, word: w, start: idx, end });
      }
    }
  }
  if (!hits.length) return null;
  // 作废区间内的命中一律剔除（防子串复活）
  const inNegated = (h) => negatedRanges.some(([s, e]) => h.start >= s && h.end <= e);
  const alive = hits.filter((h) => !inNegated(h));
  if (!alive.length) return null;
  alive.sort((a, b) => {
    if (b.word.length !== a.word.length) return b.word.length - a.word.length;
    return a.start - b.start;
  });
  const chosen = alive[0];
  // 与主命中不重叠的其它存活命中也要剔除，避免 "紧急重要" 残留 "重要"
  const rest = alive.filter((h) => h !== chosen && (h.end <= chosen.start || h.start >= chosen.end));
  return { key: chosen.key, hit: chosen, allHits: [chosen, ...rest] };
}

/* ---------------------------------------------------------------- 时间片段 */

const PERIOD_HOUR_SHIFT = {
  凌晨: 'asis',
  清晨: 'asis',
  早上: 'asis',
  早晨: 'asis',
  一早: 'asis',
  上午: 'asis',
  中午: 'noon',
  正午: 'noon',
  下午: 'pm',
  傍晚: 'pm',
  晚上: 'pm',
  夜里: 'pm',
  夜晚: 'pm'
};

const PERIOD_SRC = Object.keys(PERIOD_HOUR_SHIFT).join('|');

/**
 * 把时段名（或已归一化的位移标识）转成位移类型：
 *  'asis' 原样 / 'noon' 正午 / 'pm' 下午晚上（<12 则 +12）
 */
function periodToShift(p) {
  if (!p) return '';
  if (p === 'pm' || p === 'noon' || p === 'asis') return p;
  return PERIOD_HOUR_SHIFT[p] || '';
}

/** HH:MM 形式 */
const RE_TIME_COLON = new RegExp(`(${PERIOD_SRC})?\\s*(\\d{1,2})\\s*[:：]\\s*(\\d{2})`);
/** N点(半|一刻|三刻|N分)? 形式 */
const RE_TIME_DIAN = new RegExp(
  `(${PERIOD_SRC})?\\s*(\\d{1,2}|${CN_NUM_SRC}{1,3})\\s*(?:点|时)\\s*(半|一刻|三刻|\\d{1,2}分|${CN_NUM_SRC}{1,3}分)?`
);
/** 今天 / 明天 ...  days = 相对天数，shift = 该措辞隐含的时段 */
const REL_DAY = {
  今天: { days: 0 }, 今日: { days: 0 },
  今晚: { days: 0, shift: 'pm' }, 今早: { days: 0, shift: 'asis' }, 今晨: { days: 0, shift: 'asis' },
  明天: { days: 1 }, 明日: { days: 1 },
  明早: { days: 1, shift: 'asis' }, 明晚: { days: 1, shift: 'pm' }, 明晨: { days: 1, shift: 'asis' },
  后天: { days: 2 }, 后日: { days: 2 },
  大后天: { days: 3 }, 大后日: { days: 3 }
};
const REL_DAY_SRC = Object.keys(REL_DAY).sort((a, b) => b.length - a.length).join('|');

const WEEKDAY_MAP = { 一: 1, 二: 2, 三: 3, 四: 4, 五: 5, 六: 6, 日: 0, 天: 0, 末: 0 };

const CONNECTORS = ['之前', '以前', '以内', '之内', '前完成', '内完成', '为止', '前', '内', '截止', '前完成'];

/* ---------------------------------------------------------------- 日期候选 */

function startOfDay(d) {
  const x = new Date(d.getTime());
  x.setHours(0, 0, 0, 0);
  return x;
}

function endOfDay(d) {
  const x = new Date(d.getTime());
  x.setHours(23, 59, 0, 0);
  return x;
}

function addDays(d, n) {
  const x = new Date(d.getTime());
  x.setDate(x.getDate() + n);
  return x;
}

function lastDayOfMonth(year, monthIndex) {
  return new Date(year, monthIndex + 1, 0);
}

function lastDayOfWeek(d) {
  // 以周一为一周起点，返回本周周日
  const wd = d.getDay(); // 0=Sun
  const delta = wd === 0 ? 0 : 7 - wd;
  return addDays(d, delta);
}

/**
 * 收集所有"日期级"候选（不含纯时间），返回 { spec, start, end, date }
 */
function collectDateCandidates(text, now) {
  const out = [];

  // 1) 完整年月日 2026-10-12 / 2026/10/12 / 2026年10月12日
  {
    const re = /(\d{4})\s*[-\/年.]\s*(\d{1,2})\s*[-\/月.]\s*(\d{1,2})\s*[日号]?/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const y = +m[1], mo = +m[2], d = +m[3];
      if (mo >= 1 && mo <= 12 && d >= 1 && d <= 31) {
        out.push({ spec: 100, start: m.index, end: m.index + m[0].length, date: endOfDay(new Date(y, mo - 1, d)) });
      }
    }
  }

  // 2) 月日 M月D日 / M-D / M/D
  {
    const re = /(\d{1,2})\s*[-\/月]\s*(\d{1,2})\s*[日号]?/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      // 排除被完整年份覆盖的（2026-10-12 中的 10-12）
      const covered = out.some((c) => m.index >= c.start && m.index + m[0].length <= c.end);
      if (covered) continue;
      // 防误伤："2-3天内"、"10-20小时后" 这类**数量区间**不是日期，
      // 紧跟时间单位词时跳过，交由"N天后/小时内"规则处理
      const afterCh = text[m.index + m[0].length] || '';
      if (/[天周小时分秒年内]/.test(afterCh)) continue;
      const mo = +m[1], d = +m[2];
      if (mo < 1 || mo > 12 || d < 1 || d > 31) continue;
      let year = now.getFullYear();
      let cand = endOfDay(new Date(year, mo - 1, d));
      if (cand.getTime() < startOfDay(now).getTime()) {
        cand = endOfDay(new Date(year + 1, mo - 1, d));
      }
      out.push({ spec: 80, start: m.index, end: m.index + m[0].length, date: cand });
    }
  }

  // 3) N天后 / N日内 / N小时内 / N个月内 / 半小时后（支持 "2-3天" 数量区间，取上限）
  {
    const re = new RegExp(`(半|(?:\\d{1,3})(?:\\s*[-~～至到]\\s*\\d{1,3})?|${CN_NUM_SRC}{1,3})\\s*(天|日|周|个?星期|个月|月|小时|钟头)\\s*(?:之?[后内]|以[后内])`);
    const m = re.exec(text);
    if (m) {
      const half = m[1] === '半';
      const rm = /^(\d{1,3})\s*[-~～至到]\s*(\d{1,3})$/.exec(m[1]);
      const n = half ? 0.5 : (rm ? Math.max(+rm[1], +rm[2]) : cnToNum(m[1]));
      if (!Number.isNaN(n)) {
        const unit = m[2];
        let date = null;
        if (unit === '小时' || unit === '钟头') {
          date = new Date(now.getTime() + n * 3600 * 1000);
        } else if (unit === '天' || unit === '日') {
          date = half ? new Date(now.getTime() + 12 * 3600 * 1000) : endOfDay(addDays(now, n));
        } else if (!half) {
          if (unit === '个月' || unit === '月') {
            date = endOfDay(new Date(now.getFullYear(), now.getMonth() + n, now.getDate()));
          } else {
            date = endOfDay(addDays(now, n * 7));
          }
        }
        if (date) out.push({ spec: 75, start: m.index, end: m.index + m[0].length, date });
      }
    }
  }

  // 3b) N分钟后（同样支持区间，取上限）
  {
    const re = new RegExp(`(半|(?:\\d{1,3})(?:\\s*[-~～至到]\\s*\\d{1,3})?|${CN_NUM_SRC}{1,3})\\s*(?:分钟|分)\\s*(?:之?[后内]|以[后内])`);
    const m = re.exec(text);
    if (m) {
      const rm = /^(\d{1,3})\s*[-~～至到]\s*(\d{1,3})$/.exec(m[1]);
      const n = m[1] === '半' ? 30 : (rm ? Math.max(+rm[1], +rm[2]) : cnToNum(m[1]));
      if (!Number.isNaN(n)) {
        out.push({ spec: 76, start: m.index, end: m.index + m[0].length, date: new Date(now.getTime() + n * 60000) });
      }
    }
  }

  // 2b) 只写日：N号 / N日（当月；已过则顺延到下月）
  //     注意：必须排在 3)/3b) 之后 —— "7日内" 会同时命中"7日"与"7日内"，
  //     靠下面的覆盖剔除把子串候选丢掉，避免产生双日期误报
  {
    const re = new RegExp(`(\\d{1,2}|${CN_NUM_SRC}{1,3})\\s*([日号])`);
    const m = re.exec(text);
    if (m) {
      const covered = out.some((c) => m.index >= c.start && m.index + m[0].length <= c.end);
      if (!covered) {
        const day = cnToNum(m[1]);
        if (!Number.isNaN(day) && day >= 1 && day <= 31) {
          let cand = endOfDay(new Date(now.getFullYear(), now.getMonth(), day));
          if (cand.getTime() < startOfDay(now).getTime()) {
            cand = endOfDay(new Date(now.getFullYear(), now.getMonth() + 1, day));
          }
          out.push({ spec: 78, start: m.index, end: m.index + m[0].length, date: cand });
        }
      }
    }
  }

  // 4) 相对日：今天/明天/后天/大后天（含"今晚/明晚"隐含时段）
  {
    const re = new RegExp(`(${REL_DAY_SRC})`);
    const m = re.exec(text);
    if (m) {
      const info = REL_DAY[m[1]];
      out.push({
        spec: 70,
        start: m.index,
        end: m.index + m[1].length,
        date: endOfDay(addDays(now, info.days)),
        impliedShift: info.shift || ''
      });
    }
  }

  // 5) 周X / 星期X / 礼拜X（可带 本/这/下 前缀）
  {
    const re = /(本|这|下|下个|次)?\s*(周|星期|礼拜|周天)\s*([一二三四五六日天末]|\d)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const prefix = m[1] || '';
      const wdRaw = m[3];
      const wd = /\d/.test(wdRaw) ? (parseInt(wdRaw, 10) % 7) : WEEKDAY_MAP[wdRaw];
      if (wd == null || Number.isNaN(wd)) continue;
      const todayWd = now.getDay();
      let delta = (wd - todayWd + 7) % 7;
      if (prefix === '下' || prefix === '下个' || prefix === '次') delta += 7;
      else if (prefix === '本' || prefix === '这') {
        // 本周内已过则顺延到下周同一天
        if (delta === 0 && endOfDay(now).getTime() < now.getTime()) delta += 7;
      }
      const date = endOfDay(addDays(now, delta));
      out.push({ spec: 65, start: m.index, end: m.index + m[0].length, date });
    }
  }

  // 6) 月底 / 月末 / 本月底 / 下月底
  {
    const re = /(本|下|这个|下个)?\s*(月|个月)\s*(底|末)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const shift = m[1] === '下' || m[1] === '下个' ? 1 : 0;
      const base = new Date(now.getFullYear(), now.getMonth() + shift, 1);
      const date = endOfDay(lastDayOfMonth(base.getFullYear(), base.getMonth()));
      out.push({ spec: 60, start: m.index, end: m.index + m[0].length, date });
    }
  }

  // 7) 本周 / 这周 / 本周内
  {
    const re = /(本|这)\s*(周|星期|礼拜)\s*(?:内|以内)?/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      // 若紧跟"周X"已被上面消费，则跳过（上面 spec 更高，取最高即可）
      const date = endOfDay(lastDayOfWeek(now));
      out.push({ spec: 45, start: m.index, end: m.index + m[0].length, date });
    }
  }

  // 8) 年底 / 今年内
  {
    const re = /(本|今|这)?\s*年\s*(底|末|内)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const date = endOfDay(new Date(now.getFullYear(), 11, 31));
      out.push({ spec: 30, start: m.index, end: m.index + m[0].length, date });
    }
  }

  return out;
}

/* ---------------------------------------------------------------- 纯时间候选 */

function collectTimeCandidate(text) {
  let best = null;

  {
    const m = RE_TIME_COLON.exec(text);
    if (m) {
      const h = +m[2];
      const mi = +m[3];
      if (h <= 23 && mi <= 59) {
        best = { period: m[1] || '', hour: h, minute: mi, start: m.index, end: m.index + m[0].length, spec: 90 };
      }
    }
  }

  if (!best) {
    const m = RE_TIME_DIAN.exec(text);
    if (m) {
      const rawH = m[2];
      const h = cnToNum(rawH);
      if (!Number.isNaN(h) && h <= 24) {
        const moRaw = m[3] || '';
        let mi = 0;
        if (moRaw === '半') mi = 30;
        else if (moRaw === '一刻') mi = 15;
        else if (moRaw === '三刻') mi = 45;
        else if (moRaw) mi = cnToNum(moRaw.replace('分', ''));
        if (Number.isNaN(mi)) mi = 0;
        best = { period: m[1] || '', hour: h, minute: mi, start: m.index, end: m.index + m[0].length, spec: 60 };
      }
    }
  }

  if (!best) return null;
  best.shift = periodToShift(best.period);
  return best;
}

/** 依据 shift 把"小时:分钟"落到具体日期上 */
function applyTime(date, time) {
  const d = new Date(date.getTime());
  if (!time) return d;
  let h = time.hour;
  const shift = time.shift || '';
  if (shift === 'pm') {
    if (h < 12) h += 12;
  } else if (shift === 'noon') {
    // 「中午12点」= 12:00；「中午1点」口语上等于 13:00（与「下午1点」一致）
    if (h === 1) h = 13;
    else if (h === 0) h = 12;
  }
  let extraDay = 0;
  if (h === 24) { h = 0; extraDay = 1; }
  d.setHours(h, time.minute || 0, 0, 0);
  d.setDate(d.getDate() + extraDay);
  return d;
}

/* ---------------------------------------------------------------- 主入口 */

function mergeRanges(ranges) {
  const sorted = ranges.slice().sort((a, b) => a[0] - b[0]);
  const out = [];
  for (const r of sorted) {
    if (out.length && r[0] <= out[out.length - 1][1]) {
      out[out.length - 1][1] = Math.max(out[out.length - 1][1], r[1]);
    } else out.push([r[0], r[1]]);
  }
  return out;
}

function extendOverConnectors(text, end) {
  let e = end;
  let grew = true;
  while (grew) {
    grew = false;
    for (const c of CONNECTORS) {
      if (text.startsWith(c, e)) { e += c.length; grew = true; break; }
    }
  }
  return e;
}

function cleanContent(text, ranges) {
  const merged = mergeRanges(ranges.map(([s, e]) => [s, extendOverConnectors(text, e)]));
  let out = '';
  let cursor = 0;
  for (const [s, e] of merged) {
    if (s > cursor) out += text.slice(cursor, s);
    cursor = Math.max(cursor, e);
  }
  out += text.slice(cursor);
  return out
    .replace(/\s+/g, ' ')
    .replace(/^[\s,，。;；、:：\-—_|!！?？.。~～*#]+/, '')
    .replace(/[\s,，。;；、:：\-—_|!！?？~～*#]+$/, '')
    // 修复批 3-4：剔除命中（如「紧急」）后留下的连续分隔符压成一个，不再出现 "，，" / "；；"
    .replace(/([,，;；、:：])\1+/g, '$1')
    .replace(/\s{2,}/g, ' ')
    .trim();
}

/**
 * @param {string} text 用户原始输入
 * @param {Date|number} [nowInput] 参照时间（测试用）
 * @returns {{content:string, priority:string, deadline:number|null, matched:string[]}}
 */
function parseTodoInput(text, nowInput) {
  const raw = (text == null ? '' : String(text)).trim();
  const now = nowInput == null ? new Date() : (nowInput instanceof Date ? nowInput : new Date(nowInput));

  const pri = scanPriority(raw);
  const dateCands = collectDateCandidates(raw, now);
  const timeCand = collectTimeCandidate(raw);

  const ranges = [];
  const matched = [];

  let priority = DEFAULT_PRIORITY;
  if (pri) {
    priority = pri.key;
    // 1.2.0 批（用户 2026-10-15）：优先级判定不变，命中词不再从正文剔除——content 保留用户原词；
    // matched 仍收全部命中（预览与提示语义不变）。日期/时段剔除逻辑一律不动。
    for (const h of pri.allHits) {
      matched.push(h.word);
    }
  }

  // 选日期候选：spec 最高，其次最靠前
  let bestDate = null;
  for (const c of dateCands) {
    if (!bestDate || c.spec > bestDate.spec || (c.spec === bestDate.spec && c.start < bestDate.start)) bestDate = c;
  }

  let deadline = null;
  if (bestDate) {
    // 所有日期候选都从正文剔除，避免落选的日期残留（如 "明天…2026-11-01" 里的 "明天"）
    for (const c of dateCands) {
      ranges.push([c.start, c.end]);
    }
    matched.push(raw.slice(bestDate.start, bestDate.end));
    if (timeCand) {
      const coveredByDate = timeCand.start >= bestDate.start && timeCand.end <= bestDate.end;
      // 无显式时段时，沿用"今晚/明晚"这类措辞隐含的时段
      const timeForApply = Object.assign({}, timeCand, { shift: timeCand.shift || bestDate.impliedShift || '' });
      const dt = applyTime(bestDate.date, timeForApply);
      deadline = dt.getTime();
      if (!coveredByDate) {
        ranges.push([timeCand.start, timeCand.end]);
        matched.push(raw.slice(timeCand.start, timeCand.end));
      }
    } else if (bestDate.impliedShift === 'pm') {
      // 只有"今晚/明晚"，没有具体钟点 → 默认 20:00
      deadline = applyTime(bestDate.date, { hour: 20, minute: 0, shift: 'asis' }).getTime();
    } else {
      deadline = bestDate.date.getTime();
    }
  } else if (timeCand) {
    ranges.push([timeCand.start, timeCand.end]);
    matched.push(raw.slice(timeCand.start, timeCand.end));
    let base = startOfDay(now);
    let dt = applyTime(base, timeCand);
    if (dt.getTime() <= now.getTime()) dt = applyTime(addDays(base, 1), timeCand);
    deadline = dt.getTime();
  }

  const content = cleanContent(raw, ranges);

  // 「检测到 N 个日期」只统计真正独立的候选：被更高优先候选完整覆盖的不算
  // （如「本周五」里的「本周」），否则用户只写了一个日期也会看到多日期提示
  const distinctDates = dateCands.filter(
    (c) => !dateCands.some((o) => o !== c && o.spec >= c.spec && c.start >= o.start && c.end <= o.end)
  );

  return {
    content: content || raw,
    priority,
    deadline,
    matched,
    // 命中的日期候选个数；>1 时调用方可提示"检测到多个日期，已采用 X"
    dateCount: distinctDates.length,
    parsed: Boolean(content) && (Boolean(pri) || deadline != null)
  };
}

module.exports = {
  parseTodoInput,
  cnToNum,
  scanPriority,
  collectDateCandidates,
  collectTimeCandidate
};
