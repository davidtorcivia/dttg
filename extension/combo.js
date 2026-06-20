// Lightweight autocomplete combobox. Works for a single value (category) or a
// comma-separated list where only the token under the caret is completed (tags).
// getItems() is called lazily so the suggestion source can update after the
// popup has already rendered (e.g. when fresh taxonomy arrives from the server).

"use strict";

function makeCombo(input, getItems, opts) {
  opts = opts || {};
  const multi = !!opts.multi;
  const max = opts.max || 8;

  const wrap = document.createElement("div");
  wrap.className = "combo";
  input.parentNode.insertBefore(wrap, input);
  wrap.appendChild(input);

  const list = document.createElement("ul");
  list.className = "combo-list";
  list.setAttribute("role", "listbox");
  list.hidden = true;
  wrap.appendChild(list);

  input.setAttribute("autocomplete", "off");
  input.setAttribute("aria-autocomplete", "list");
  input.setAttribute("aria-expanded", "false");

  let items = [];
  let active = -1;

  function currentToken() {
    if (!multi) return input.value.trim();
    return input.value.slice(input.value.lastIndexOf(",") + 1).trim();
  }

  function takenTokens() {
    if (!multi) return new Set();
    return new Set(
      input.value.split(",").map((s) => s.trim().toLowerCase()).filter(Boolean)
    );
  }

  function apply(choice) {
    if (!multi) {
      input.value = choice;
    } else {
      const v = input.value;
      const before = v.slice(0, v.lastIndexOf(",") + 1);
      input.value = (before ? before + " " : "") + choice + ", ";
    }
    close();
    input.focus();
  }

  function render() {
    list.innerHTML = "";
    items.forEach((name, i) => {
      const li = document.createElement("li");
      li.className = "combo-item" + (i === active ? " active" : "");
      li.setAttribute("role", "option");
      li.setAttribute("aria-selected", i === active ? "true" : "false");
      li.textContent = name;
      // mousedown (not click) so it fires before the input's blur closes the list
      li.addEventListener("mousedown", (e) => {
        e.preventDefault();
        apply(name);
      });
      list.appendChild(li);
    });
  }

  function refresh() {
    const token = currentToken().toLowerCase();
    const taken = takenTokens();
    const all = getItems() || [];
    const starts = [];
    const contains = [];
    for (const name of all) {
      const ln = name.toLowerCase();
      if (taken.has(ln)) continue;
      if (token === "" || ln.startsWith(token)) starts.push(name);
      else if (ln.includes(token)) contains.push(name);
    }
    items = starts.concat(contains).slice(0, max);
    active = items.length ? 0 : -1;
    if (items.length) {
      list.hidden = false;
      input.setAttribute("aria-expanded", "true");
      render();
    } else {
      close();
    }
  }

  function close() {
    list.hidden = true;
    active = -1;
    input.setAttribute("aria-expanded", "false");
  }

  function isOpen() {
    return !list.hidden;
  }

  input.addEventListener("input", refresh);
  input.addEventListener("focus", refresh);
  input.addEventListener("blur", () => setTimeout(close, 120));
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!isOpen()) return refresh();
      active = (active + 1) % items.length;
      render();
    } else if (e.key === "ArrowUp") {
      if (!isOpen()) return;
      e.preventDefault();
      active = (active - 1 + items.length) % items.length;
      render();
    } else if ((e.key === "Enter" || e.key === "Tab") && isOpen() && active >= 0) {
      // Accept the highlighted suggestion; for Enter, swallow it so the form
      // isn't submitted on the same keystroke.
      if (e.key === "Enter") e.preventDefault();
      apply(items[active]);
    } else if (e.key === "Escape" && isOpen()) {
      e.preventDefault();
      e.stopPropagation();
      close();
    }
  });
}
