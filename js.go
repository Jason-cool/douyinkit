package douyinkit

// 页面探测 JS —— 与 Python 版 dy_reply.py 中完全一致（复刻，不改逻辑）

const LIST_CONV_JS = `
() => {
  // 普通会话项 + 「陌生人消息」入口（非互关私信会折叠在这里，必须单独认）
  const normal = [...document.querySelectorAll('div[class*="conversationConversationItemwrapper"]')];
  const folder = [...document.querySelectorAll('div[class*="conversationStrangerBoxwrapper"]')];

  let nodes = normal.map(e => ({ el: e, kind: 'conv' }));
  folder.forEach(e => nodes.push({ el: e, kind: 'stranger_folder' }));

  if (!nodes.length) {
    // 兜底：class 改名时按位置猜
    [...document.querySelectorAll('div,li,a')].filter(e => {
      const r = e.getBoundingClientRect();
      return r.x >= 0 && r.x < 300 && r.width > 150 && r.width < 470 &&
             r.height >= 46 && r.height <= 130 && (e.innerText || '').trim().length > 1;
    }).forEach(e => nodes.push({ el: e, kind: 'conv' }));
  }
  const out = [];
  nodes.forEach(({ el: e, kind }) => {
    const r = e.getBoundingClientRect();
    if (r.width < 80 || r.height < 28) return;

    // 未读小红点：会话项里尺寸很小的红色元素
    let unread = false, badge = '';
    e.querySelectorAll('*').forEach(c => {
      const cr = c.getBoundingClientRect();
      if (cr.width > 0 && cr.width <= 24 && cr.height > 0 && cr.height <= 24) {
        const bg = getComputedStyle(c).backgroundColor || '';
        const m = bg.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
        if (m && +m[1] > 170 && +m[2] < 120 && +m[3] < 130) {
          unread = true;
          const t = (c.innerText || '').trim();
          if (t) badge = t;
        }
      }
    });

    const lines = (e.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
    out.push({
      cx: Math.round(r.x + r.width / 2),
      cy: Math.round(r.y + r.height / 2),
      y: Math.round(r.y),
      kind: kind,
      name: (lines[0] || '').slice(0, 40),
      preview: (lines.slice(1).join(' ') || '').slice(0, 60),
      raw: (e.innerText || '').replace(/\s+/g, ' ').slice(0, 200),
      unread: unread,
      badge: badge,
      active: String(e.className || '').includes('curConversation')
    });
  });
  out.sort((a, b) => a.y - b.y);
  return out;
}
`

const HEADER_JS = `
() => {
  const el = document.querySelector('div[class*="RightPanelHeaderuser"]');
  if (!el) return null;
  const t = (el.innerText || '').replace(/\s+/g, ' ').trim();
  // 群聊标题形如「群名(9)」，末尾括号里是成员数
  return { text: t, isGroup: /\(\d+\)\s*$/.test(t) };
}
`

const INPUT_JS = `
() => {
  const sel = 'div[class*="messageEditorinputArea"], div[contenteditable="true"], textarea';
  // 在候选容器内（含自身）定位真正可编辑的节点，避免点到只读的外层容器
  const pickEditable = (e) => {
    if (e.tagName === 'TEXTAREA' || e.tagName === 'INPUT') return e;
    if (e.getAttribute('contenteditable') === 'true') return e;
    return e.querySelector('[contenteditable="true"], textarea') || e;
  };
  const cands = [];
  const seen = new Set();
  document.querySelectorAll(sel).forEach(e => {
    const t = pickEditable(e);
    if (seen.has(t)) return;
    seen.add(t);
    const r = t.getBoundingClientRect();
    if (r.width > 150 && r.height > 10 && r.top > window.innerHeight * 0.3) {
      cands.push({ cx: Math.round(r.x + r.width / 2), cy: Math.round(r.y + r.height / 2),
                   w: Math.round(r.width), h: Math.round(r.height) });
    }
  });
  if (!cands.length) return null;
  cands.sort((a, b) => b.w - a.w);
  return cands[0];
}
`

// 读输入框里当前残留的文字，用来判断「发出去了没有」
// 注意：抖音输入框空状态是零宽空格 \u200b，trim() 不会去掉它，必须手动清掉
const INPUT_TEXT_JS = `
() => {
  const clean = (s) => (s || '').replace(/[\u200b\u200c\u200d\ufeff]/g, '').trim();
  const sel = 'div[class*="messageEditorinputArea"], div[contenteditable="true"], textarea';
  const cands = [];
  document.querySelectorAll(sel).forEach(e => {
    const r = e.getBoundingClientRect();
    if (r.width > 150 && r.height > 10 && r.top > window.innerHeight * 0.3) {
      cands.push({ w: r.width, text: clean(e.innerText || e.value || '') });
    }
  });
  if (!cands.length) return null;
  cands.sort((a, b) => b.w - a.w);
  return cands[0].text;
}
`

// 主动聚焦输入框，并回读它的可编辑/禁用状态。
// 用途：仅靠坐标点击有时焦点进不去，InsertText 会写空；先 focus() 一次，
// 并在失败时判断是不是被禁用（抖音「需对方先回复/仅能发一条」限制常表现为不可编辑）。
const FOCUS_INPUT_JS = `
() => {
  const sel = 'div[class*="messageEditorinputArea"], div[contenteditable="true"], textarea';
  const pickEditable = (e) => {
    if (e.tagName === 'TEXTAREA' || e.tagName === 'INPUT') return e;
    if (e.getAttribute('contenteditable') === 'true') return e;
    return e.querySelector('[contenteditable="true"], textarea') || e;
  };
  let best = null, bestW = 0;
  const seen = new Set();
  document.querySelectorAll(sel).forEach(e => {
    const t = pickEditable(e);
    if (seen.has(t)) return;
    seen.add(t);
    const r = t.getBoundingClientRect();
    if (r.width > 150 && r.height > 10 && r.top > window.innerHeight * 0.3 && r.width > bestW) {
      bestW = r.width; best = t;
    }
  });
  if (!best) return { focused: false, editable: false, disabled: true };
  const tag = best.tagName;
  const ce = best.getAttribute('contenteditable');
  const editable = (ce === 'true' || ce === '' || tag === 'TEXTAREA' || tag === 'INPUT');
  const disabled = !editable || ce === 'false' || best.hasAttribute('disabled')
    || best.getAttribute('aria-disabled') === 'true';
  try { best.focus(); } catch (e) {}
  const ae = document.activeElement;
  const focused = !!ae && (ae === best || (best.contains && best.contains(ae)));
  return { focused: focused, editable: editable, disabled: disabled };
}
`

// 点开陌生人消息后，找「返回」按钮（回到主列表）
const BACK_JS = `
() => {
  const hints = ['返回', '全部消息', '消息'];
  const cands = [];
  document.querySelectorAll('div,span,button').forEach(e => {
    const t = (e.innerText || '').trim();
    if (!t || t.length > 6) return;
    if (!hints.some(h => t.includes(h))) return;
    const r = e.getBoundingClientRect();
    if (r.width > 10 && r.height > 10 && r.y < window.innerHeight * 0.45) {
      cands.push({ cx: Math.round(r.x + r.width / 2), cy: Math.round(r.y + r.height / 2), text: t });
    }
  });
  if (!cands.length) return null;
  return cands[0];
}
`

// 读聊天区最后一条文字消息，并用左右位置判断是谁发的
// 实测：对方气泡 left≈366，我方气泡 left≈1320（视口 1440）
const LAST_MSG_JS = `
() => {
  const bs = [...document.querySelectorAll('div[class*="MessageItemTextbubbleContent"]')];
  let last = null;
  bs.forEach(b => {
    const r = b.getBoundingClientRect();
    if (r.width < 4 || r.height < 4) return;
    if (!last || r.top > last.top) {
      last = { top: r.top, left: r.left, text: (b.innerText || '').trim().slice(0, 80) };
    }
  });
  if (!last) return null;
  return {
    text: last.text,
    side: (last.left + 20) < (window.innerWidth / 2) ? 'left' : 'right'
  };
}
`

// 发送按钮：实测是 svg.messageMsgInputpublishBtn.e2e-send-msg-btn
const SEND_BTN_JS = `
() => {
  const btn = document.querySelector('svg[class*="publishBtn"], [class*="e2e-send-msg-btn"]');
  if (btn) {
    const r = btn.getBoundingClientRect();
    if (r.width > 8 && r.height > 8) {
      return { cx: Math.round(r.x + r.width / 2), cy: Math.round(r.y + r.height / 2) };
    }
  }
  // 兜底：输入区右侧最后一个图标
  const bar = document.querySelector('div[class*="MsgInputinputAction"]');
  if (!bar) return null;
  const kids = [...bar.children];
  for (let i = kids.length - 1; i >= 0; i--) {
    const r = kids[i].getBoundingClientRect();
    if (r.width >= 20 && r.width <= 60 && r.height >= 20) {
      return { cx: Math.round(r.x + r.width / 2), cy: Math.round(r.y + r.height / 2) };
    }
  }
  return null;
}
`

const LOGIN_PAGE_JS = `
() => {
  const t = document.body.innerText || '';
  return t.includes('扫码登录') || t.includes('验证码登录') || t.includes('密码登录');
}
`

// ---------------------------------------------------------------- DOM 标记（替代坐标点击）
//
// 背景：Linux 服务器（headless / 无头容器）里，JS 算出的 getBoundingClientRect 坐标
// 和 CDP 鼠标事件落点可能不一致（视口缩放/滚动/容器尺寸差异），表现为「点了没反应、点错会话」。
// 解法：先给目标元素打 data 标记，再用元素级 Click（浏览器自己 scrollIntoView + 算中心点），
// 完全不在 Go 侧做坐标运算。

// MARK_CONV_JS 按会话名给列表项打标记。返回 exact / contains / none
const MARK_CONV_JS = `(name) => {
  document.querySelectorAll('[data-dy-target]').forEach(e => e.removeAttribute('data-dy-target'));
  const rows = [...document.querySelectorAll('div[class*="conversationConversationItemwrapper"]')];
  const nameOf = (e) => {
    const l = (e.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
    return (l[0] || '').slice(0, 40);
  };
  let hit = rows.find(e => nameOf(e) === name);
  if (hit) { hit.setAttribute('data-dy-target', '1'); return 'exact'; }
  hit = rows.find(e => { const n = nameOf(e); return n && (n.includes(name) || name.includes(n)); });
  if (hit) { hit.setAttribute('data-dy-target', '1'); return 'contains'; }
  return 'none';
}`

// MARK_STRANGER_JS 给「陌生人消息」入口打标记
const MARK_STRANGER_JS = `() => {
  document.querySelectorAll('[data-dy-stranger]').forEach(e => e.removeAttribute('data-dy-stranger'));
  const e = document.querySelector('div[class*="conversationStrangerBoxwrapper"]');
  if (!e) return false;
  e.setAttribute('data-dy-stranger', '1');
  return true;
}`

// MARK_INPUT_JS 给真正可编辑的输入框打标记
const MARK_INPUT_JS = `() => {
  document.querySelectorAll('[data-dy-input]').forEach(e => e.removeAttribute('data-dy-input'));
  const sel = 'div[class*="messageEditorinputArea"], div[contenteditable="true"], textarea';
  const pickEditable = (e) => {
    if (e.tagName === 'TEXTAREA' || e.tagName === 'INPUT') return e;
    if (e.getAttribute('contenteditable') === 'true') return e;
    return e.querySelector('[contenteditable="true"], textarea') || e;
  };
  let best = null, bestW = 0;
  const seen = new Set();
  document.querySelectorAll(sel).forEach(e => {
    const t = pickEditable(e);
    if (seen.has(t)) return;
    seen.add(t);
    const r = t.getBoundingClientRect();
    if (r.width > 150 && r.height > 10 && r.top > window.innerHeight * 0.3 && r.width > bestW) {
      bestW = r.width; best = t;
    }
  });
  if (!best) return false;
  best.setAttribute('data-dy-input', '1');
  return true;
}`

// MARK_SEND_JS 给发送按钮打标记（实测 svg.messageMsgInputpublishBtn.e2e-send-msg-btn）
const MARK_SEND_JS = `() => {
  document.querySelectorAll('[data-dy-send]').forEach(e => e.removeAttribute('data-dy-send'));
  let btn = document.querySelector('svg[class*="publishBtn"], [class*="e2e-send-msg-btn"]');
  if (!btn) {
    const bar = document.querySelector('div[class*="MsgInputinputAction"]');
    if (bar) {
      const kids = [...bar.children];
      for (let i = kids.length - 1; i >= 0; i--) {
        const r = kids[i].getBoundingClientRect();
        if (r.width >= 20 && r.width <= 60 && r.height >= 20) { btn = kids[i]; break; }
      }
    }
  }
  if (!btn) return false;
  btn.setAttribute('data-dy-send', '1');
  return true;
}`

// MARK_BACK_JS 给「返回主列表」按钮打标记（陌生人消息页用）
const MARK_BACK_JS = `() => {
  document.querySelectorAll('[data-dy-back]').forEach(e => e.removeAttribute('data-dy-back'));
  const hints = ['返回', '全部消息', '消息'];
  const cands = [];
  document.querySelectorAll('div,span,button').forEach(e => {
    const t = (e.innerText || '').trim();
    if (!t || t.length > 6) return;
    if (!hints.some(h => t.includes(h))) return;
    const r = e.getBoundingClientRect();
    if (r.width > 10 && r.height > 10 && r.y < window.innerHeight * 0.45) cands.push(e);
  });
  if (!cands.length) return false;
  cands[0].setAttribute('data-dy-back', '1');
  return true;
}`

// JS_CLICK_JS 对已标记元素派发原生 click（某些组件只监听 click，鼠标事件被遮罩吞掉时兜底）
const JS_CLICK_JS = `(sel) => {
  const e = document.querySelector(sel);
  if (!e) return false;
  e.click();
  return true;
}`

// METRICS_JS 视口信息，排查坐标类问题时打印
const METRICS_JS = `() => ({
  w: window.innerWidth, h: window.innerHeight,
  dpr: window.devicePixelRatio,
  sy: Math.round(window.scrollY),
  sh: document.body ? document.body.scrollHeight : 0
})`

// ROW_POINT_JS 给定会话名，算出「真正能点中这一行」的坐标点。
// 关键：不能盲目用行中心——服务器上可能被遮罩/浮层挡住，或行不在可视区。
// 做法：必要时先滚动到可见，再在行内多点做 elementFromPoint 命中测试，
// 只有命中元素属于该行时才采用该点；全部被挡则把「挡住它的元素」报回来自证。
const ROW_POINT_JS = `(name) => {
  const desc = el => el ? (el.tagName + '.' + String(el.className || '').slice(0, 70)) : '';
  const nameOf = e => {
    const l = (e.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
    return (l[0] || '').slice(0, 40);
  };
  const rows = [...document.querySelectorAll('div[class*="conversationConversationItemwrapper"]')];
  let row = rows.find(e => nameOf(e) === name);
  if (!row) row = rows.find(e => { const n = nameOf(e); return n && (n.includes(name) || name.includes(n)); });
  if (!row) return { ok: false, why: 'row-not-found', rows: rows.length };

  let r = row.getBoundingClientRect();
  if (r.top < 4 || r.bottom > window.innerHeight - 4) {
    row.scrollIntoView({ block: 'center' });
    r = row.getBoundingClientRect();
  }
  const pts = [];
  const add = (fx, fy) => pts.push([Math.round(r.x + r.width * fx), Math.round(r.y + r.height * fy)]);
  add(0.45, 0.5); add(0.45, 0.35); add(0.25, 0.5); add(0.6, 0.5); add(0.45, 0.65); add(0.08, 0.5);

  let hit = null, pt = null, covered = '';
  for (const p of pts) {
    const e = document.elementFromPoint(p[0], p[1]);
    if (!e) { if (!covered) covered = 'null'; continue; }
    if (e === row || row.contains(e)) { hit = e; pt = p; break; }
    if (!covered) covered = desc(e);
  }
  return {
    ok: !!pt,
    x: pt ? pt[0] : Math.round(r.x + r.width / 2),
    y: pt ? pt[1] : Math.round(r.y + r.height / 2),
    hit: desc(hit),
    covered: covered,
    rect: [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)],
    rowTag: row.tagName,
    rowClass: String(row.className || '').slice(0, 180),
    rowPE: getComputedStyle(row).pointerEvents,
    rowDisplay: getComputedStyle(row).display,
    rowOpacity: getComputedStyle(row).opacity,
    href: row.tagName === 'A' ? (row.getAttribute('href') || '') : '',
    child0: desc(row.firstElementChild),
    child1: desc(row.firstElementChild && row.firstElementChild.firstElementChild)
  };
}`

// JS_POINTER_CLICK_JS 在指定坐标上派发完整指针序列（pointerdown→mousedown→pointerup→mouseup→click）。
// 应对：组件只监听 pointerdown/mousedown，或原生 click() 打在了不承接事件的父层上。
const JS_POINTER_CLICK_JS = `(x, y) => {
  const e = document.elementFromPoint(x, y);
  if (!e) return { ok: false, why: 'no-element' };
  const base = { bubbles: true, cancelable: true, composed: true, view: window,
                 clientX: x, clientY: y, screenX: x, screenY: y, button: 0, buttons: 1 };
  const pe = Object.assign({}, base, { pointerId: 1, pointerType: 'mouse', isPrimary: true, width: 1, height: 1 });
  const fire = (t, opt, C) => { try { e.dispatchEvent(new C(t, opt)); } catch (_) {} };
  fire('pointerover', pe, PointerEvent);
  fire('mouseover', base, MouseEvent);
  fire('pointermove', pe, PointerEvent);
  fire('mousemove', base, MouseEvent);
  fire('pointerdown', pe, PointerEvent);
  fire('mousedown', base, MouseEvent);
  try { e.focus(); } catch (_) {}
  const up = Object.assign({}, base, { buttons: 0 });
  const upe = Object.assign({}, pe, { buttons: 0 });
  fire('pointerup', upe, PointerEvent);
  fire('mouseup', up, MouseEvent);
  fire('click', up, MouseEvent);
  return { ok: true, hit: e.tagName + '.' + String(e.className || '').slice(0, 70) };
}`

// CHAT_STATE_JS 右侧聊天区当前状态：标题栏 / 激活行 / 气泡数
// 用「激活行」判定点击是否生效，避免标题栏选择器失效时误判为没打开
const CHAT_STATE_JS = `() => {
  const nameOf = e => {
    const l = (e.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
    return (l[0] || '').slice(0, 40);
  };
  const hdr = document.querySelector('div[class*="RightPanelHeaderuser"]');
  const rows = [...document.querySelectorAll('div[class*="conversationConversationItemwrapper"]')];
  const act = rows.find(e => String(e.className || '').includes('curConversation'));
  return {
    header: hdr ? (hdr.innerText || '').replace(/\s+/g, ' ').trim() : '',
    active: act ? nameOf(act) : '',
    rows: rows.length,
    bubbles: document.querySelectorAll('div[class*="MessageItemTextbubbleContent"]').length,
    hasEditor: !!document.querySelector('div[class*="messageEditorinputArea"], div[contenteditable="true"], textarea')
  };
}`

// JS_ENTER_JS 在编辑器上直接派发 Enter 键事件（纯 JS 兜底）。
// 容器/无头环境里 CDP 的键盘事件可能不被页面接收，但 InsertText 能写入——
// 这种「能写不能发」的情况用这条兜底，组件只要监听 keydown 就能触发发送。
const JS_ENTER_JS = `(sel) => {
  const ed = (sel && document.querySelector(sel)) ||
             document.querySelector('[data-dy-input="1"]') ||
             document.activeElement ||
             document.querySelector('div[contenteditable="true"], textarea');
  if (!ed) return false;
  const o = { bubbles: true, cancelable: true, composed: true, key: 'Enter', code: 'Enter',
              keyCode: 13, which: 13, charCode: 13, view: window };
  ed.dispatchEvent(new KeyboardEvent('keydown', o));
  ed.dispatchEvent(new KeyboardEvent('keypress', o));
  ed.dispatchEvent(new KeyboardEvent('keyup', o));
  return true;
}`

// ELEM_CENTER_JS 取元素中心坐标（给「指针序列点击」用）
const ELEM_CENTER_JS = `(sel) => {
  const e = document.querySelector(sel);
  if (!e) return null;
  const r = e.getBoundingClientRect();
  if (r.width < 2 || r.height < 2) return null;
  return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
}`
