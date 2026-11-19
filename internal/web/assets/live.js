// Live updates for the thread page: new replies appear without a reload,
// and an indicator shows who is writing one.
//
// The page works without this file. Everything it needs comes from the
// page itself:
//   <main data-thread-id data-member-id data-live-path>
//   #reply-list         top-level replies; new ones are appended here
//   #reply-{id}         each reply; [data-flat-children] when its children do not indent
//   #reply-count        the "N replies" heading, with data-count
//   #typing-indicator   empty and hidden until someone is replying
//   #live-announcer     aria-live region for screen readers
//
// The server sends two JSON messages over the WebSocket at data-live-path:
//   {"type":"reply","reply_id":N,"parent_id":N,"member_id":N}
//   {"type":"typing","member_id":N,"name":"..."}
// and accepts one: {"type":"typing"}. A reply's markup is fetched from
// /replies/{id}/fragment, rendered by the server for this member.
(() => {
  'use strict';

  const main = document.getElementById('main');
  const list = document.getElementById('reply-list');
  if (!main || !list || !main.dataset.livePath || !('WebSocket' in window)) return;

  const selfID = Number(main.dataset.memberId);
  const countEl = document.getElementById('reply-count');
  const indicator = document.getElementById('typing-indicator');
  const announcer = document.getElementById('live-announcer');
  const socketURL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + main.dataset.livePath;

  const RETRY_BASE_MS = 1000;       // first reconnect delay; doubles each failure
  const RETRY_MAX_MS = 30000;
  const MAX_FAILURES = 8;           // give up after this many in a row (signed out, thread removed)
  const HIDDEN_GRACE_MS = 60000;    // close the socket once the tab has been hidden this long
  const TYPING_SEND_MS = 3000;      // tell the server we are typing at most this often
  const TYPING_SHOW_MS = 5000;      // how long one typing signal keeps a name in the indicator
  const HIGHLIGHT_MS = 4000;

  // --- Connection -----------------------------------------------------

  let socket = null;
  let failures = 0;
  let retryTimer = 0;
  let hiddenTimer = 0;
  let paused = false; // true while the tab has been hidden for a long time

  function connect() {
    if (socket || paused) return;
    const ws = new WebSocket(socketURL);
    socket = ws;
    ws.addEventListener('open', () => { failures = 0; });
    ws.addEventListener('message', (e) => {
      let ev;
      try { ev = JSON.parse(e.data); } catch { return; }
      if (!ev || typeof ev !== 'object') return;
      if (ev.type === 'reply') onReply(ev);
      else if (ev.type === 'typing') onTyping(ev);
    });
    ws.addEventListener('close', () => {
      if (socket === ws) socket = null;
      typing.clear();
      renderTyping();
      reconnectLater();
    });
  }

  // Capped exponential backoff with jitter, so a restart of the forum
  // does not bring every open tab back in the same instant.
  function reconnectLater() {
    if (paused || retryTimer || failures >= MAX_FAILURES) return;
    const ceiling = Math.min(RETRY_MAX_MS, RETRY_BASE_MS * 2 ** failures);
    failures += 1;
    retryTimer = setTimeout(() => {
      retryTimer = 0;
      connect();
    }, ceiling / 2 + Math.random() * (ceiling / 2));
  }

  function pause() {
    paused = true;
    clearTimeout(retryTimer);
    retryTimer = 0;
    if (socket) socket.close();
  }

  document.addEventListener('visibilitychange', () => {
    clearTimeout(hiddenTimer);
    if (document.hidden) {
      hiddenTimer = setTimeout(pause, HIDDEN_GRACE_MS);
    } else if (paused) {
      paused = false;
      failures = 0;
      connect();
    }
  });

  // --- New replies ----------------------------------------------------

  const fetching = new Set(); // reply IDs being fetched, so a repeat event is ignored

  async function onReply(ev) {
    const id = ev.reply_id;
    const parentID = ev.parent_id || 0;
    typing.delete(ev.member_id); // their reply arrived; they are no longer writing it
    renderTyping();
    if (!Number.isSafeInteger(id) || id < 1 || !Number.isSafeInteger(parentID) || parentID < 0) return;
    // The poster's own page already holds the reply after their redirect.
    if (document.getElementById('reply-' + id) || fetching.has(id)) return;

    fetching.add(id);
    try {
      const res = await fetch('/replies/' + id + '/fragment', {
        credentials: 'same-origin',
        headers: { Accept: 'text/html' },
      });
      if (!res.ok) return; // deleted already, or we are signed out
      // The fragment is rendered and sanitised by the server exactly as
      // the thread page is, so it is inserted as markup.
      const holder = document.createElement('template');
      holder.innerHTML = await res.text();
      const node = holder.content.firstElementChild;
      const target = childrenOf(parentID);
      if (!node || !target || document.getElementById('reply-' + id)) return;

      // Appended at the end, never re-sorted: nothing the reader is
      // looking at moves.
      target.appendChild(node);
      highlight(node);
      bumpCount();
      announce(node);
    } catch {
      // Network trouble: the reply shows up on the next page load.
    } finally {
      fetching.delete(id);
    }
  }

  // childrenOf returns the element new replies to parentID go into: the
  // top-level list, or the parent's children container, created the way
  // the template does when the parent had no replies yet. It returns
  // null when the parent is not on this page.
  function childrenOf(parentID) {
    if (!parentID) return list;
    const parent = document.getElementById('reply-' + parentID);
    if (!parent) return null;
    let kids = parent.querySelector(':scope > .reply-children');
    if (!kids) {
      kids = document.createElement('div');
      kids.className = 'reply-children';
      if ('flatChildren' in parent.dataset) kids.classList.add('reply-children--flat');
      parent.appendChild(kids);
    }
    return kids;
  }

  function highlight(node) {
    const reply = node.matches('.reply') ? node : node.querySelector('.reply');
    if (!reply) return;
    reply.classList.add('reply--new');
    setTimeout(() => reply.classList.remove('reply--new'), HIGHLIGHT_MS);
  }

  function bumpCount() {
    if (!countEl) return;
    const n = (Number(countEl.dataset.count) || 0) + 1;
    countEl.dataset.count = String(n);
    countEl.textContent = n === 1 ? '1 reply' : n + ' replies';
  }

  function announce(node) {
    if (!announcer) return;
    const author = node.querySelector('.byline__name');
    announcer.textContent = author ? 'New reply from ' + author.textContent.trim() : 'New reply';
  }

  // --- Typing indicator -----------------------------------------------

  const typing = new Map(); // member ID -> { name, until }
  let typingTimer = 0;

  function onTyping(ev) {
    if (!Number.isSafeInteger(ev.member_id) || ev.member_id === selfID || typeof ev.name !== 'string') return;
    typing.set(ev.member_id, { name: ev.name, until: Date.now() + TYPING_SHOW_MS });
    renderTyping();
  }

  function renderTyping() {
    clearTimeout(typingTimer);
    if (!indicator) return;
    const now = Date.now();
    let soonest = Infinity;
    for (const [id, entry] of typing) {
      if (entry.until <= now) typing.delete(id);
      else soonest = Math.min(soonest, entry.until);
    }
    const names = Array.from(typing.values(), (entry) => entry.name);
    let text = '';
    if (names.length === 1) text = names[0] + ' is replying…';
    else if (names.length === 2) text = names[0] + ' and ' + names[1] + ' are replying…';
    else if (names.length > 2) text = names.length + ' people are replying…';
    // textContent only: a name is a member's own text, never markup.
    indicator.textContent = text;
    indicator.hidden = text === '';
    if (typing.size) typingTimer = setTimeout(renderTyping, soonest - now + 20);
  }

  // One listener on the document covers the thread's composer and every
  // inline reply form, including those in replies added later.
  let lastTypingSent = 0;
  document.addEventListener('input', (e) => {
    const el = e.target;
    if (!(el instanceof HTMLTextAreaElement) || !el.closest('.reply-form')) return;
    const now = Date.now();
    if (now - lastTypingSent < TYPING_SEND_MS) return;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    lastTypingSent = now;
    socket.send('{"type":"typing"}');
  });

  if (!document.hidden) connect();
  else paused = true; // opened in a background tab: connect when it is first shown
})();
