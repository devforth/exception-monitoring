// Small enhancements on top of the server-rendered pages. The UI works without them.

let copyToastTimeout;

// Copy buttons (the "copy-button" partial): copy the text of their target.
document.addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-copy], button[data-copy-text]");
  if (!button) return;

  // The text is on the button (data-copy-text), or in the element it names:
  // a textarea's value or an element's text.
  let source = button.dataset.copy ? document.getElementById(button.dataset.copy) : null;
  const status = document.getElementById("copy-status");

  if (button.dataset.copyText !== undefined && !source) {
    source = document.createElement("textarea");
    source.value = button.dataset.copyText;
    source.hidden = true;
    source.dataset.temporary = "true";
    button.after(source);
  }
  if (!source) return;

  const isField = source instanceof HTMLTextAreaElement || source instanceof HTMLInputElement;
  const text = isField ? source.value : source.textContent;
  const done = button.dataset.copied || "Copied to clipboard.";

  clearTimeout(copyToastTimeout);
  if (status) status.textContent = "";
  const label = button.title;
  button.disabled = true;
  try {
    if (!navigator.clipboard?.writeText) throw new Error("Clipboard unavailable");
    await navigator.clipboard.writeText(text);
    button.title = "Copied!";
    if (status) status.textContent = done;
  } catch {
    if (source instanceof HTMLInputElement && ["password", "hidden"].includes(source.type)) {
      // Password fields cannot be copied with execCommand in some browsers.
      // Keep the fallback off screen and remove it before returning focus.
      const previousFocus = document.activeElement;
      const fallback = document.createElement("textarea");
      fallback.value = text;
      fallback.readOnly = true;
      fallback.className = "secret-copy-fallback";
      fallback.setAttribute("aria-hidden", "true");
      fallback.tabIndex = -1;
      document.body.append(fallback);
      let copied = false;
      try {
        fallback.focus({ preventScroll: true });
        fallback.select();
        copied = document.execCommand("copy");
      } catch {
        // Leave the token masked even when clipboard access is unavailable.
      } finally {
        fallback.remove();
        previousFocus?.focus({ preventScroll: true });
      }
      if (status) status.textContent = copied ? done : "Could not copy. Allow clipboard access and try again.";
      if (copied) button.title = "Copied!";
      return;
    }
    // HTTP and browsers without clipboard permission can still copy a selection.
    const wasHidden = source.hidden;
    if (isField) {
      source.hidden = false;
      source.classList.add("agent-prompt-fallback");
      source.readOnly = true;
      source.focus();
      source.select();
    } else {
      const range = document.createRange();
      range.selectNodeContents(source);
      window.getSelection().removeAllRanges();
      window.getSelection().addRange(range);
    }
    let copied = false;
    try {
      copied = document.execCommand("copy");
    } catch {
      // Keep the selected text available for manual copying.
    }
    if (isField) source.hidden = copied && wasHidden;
    if (status) status.textContent = copied ? done : "Copy the selected text with Ctrl+C or ⌘C.";
    if (copied) button.title = "Copied!";
  } finally {
    button.disabled = false;
    setTimeout(() => { button.title = label; }, 2000);
    copyToastTimeout = setTimeout(() => { if (status) status.textContent = ""; }, 5000);
    if (source.dataset.temporary && source.hidden) source.remove();
  }
});

// Show exact times in the browser's time zone (the server renders UTC).
function localizeTimes(root) {
  root.querySelectorAll("time[datetime]").forEach((element) => {
    const date = new Date(element.getAttribute("datetime"));

    if (!Number.isNaN(date.getTime())) {
      element.title = date.toLocaleString();
    }
  });
}

// Long log lines are cut to a fixed height; show "Expand" only where text is hidden.
function showExpandButtons(root) {
  root.querySelectorAll(".log-text").forEach((container) => {
    const body = container.querySelector("pre.log-body");
    const button = container.querySelector("button.expand");

    if (body && button && !body.classList.contains("expanded")) {
      button.hidden = body.scrollHeight <= body.clientHeight + 2;
    }
  });
}

// Makes a log line easier to read: loggers like pino send errors as JSON with
// the stack trace in one string full of "\n". Multi-line string fields are
// printed as real lines, other fields as indented JSON.
function readableLog(text) {
  let value;

  try {
    value = JSON.parse(text);
  } catch {
    return text;
  }

  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return text;
  }

  return Object.entries(value)
    .map(([key, field]) => {
      if (typeof field === "string") {
        return field.includes("\n") ? `${key}:\n${field}` : `${key}: ${field}`;
      }

      return `${key}: ${JSON.stringify(field, null, 2)}`;
    })
    .join("\n\n");
}

// One <dialog> reused for every log; Esc and the backdrop close it.
function showLogViewer(text) {
  let dialog = document.querySelector("dialog.log-viewer");

  if (!dialog) {
    dialog = document.createElement("dialog");
    dialog.className = "log-viewer";
    dialog.innerHTML = '<div class="viewer-bar"><strong>Log</strong><button type="button" class="close">Close</button></div><pre></pre>';
    dialog.addEventListener("click", (event) => {
      // A click outside the content lands on the dialog element itself (the backdrop).
      if (event.target === dialog || event.target.matches("button.close")) {
        dialog.close();
      }
    });
    document.body.append(dialog);
  }

  dialog.querySelector("pre").textContent = readableLog(text);
  dialog.showModal();
}

// Issues list: show how many issues are checked and enable the action buttons
// only when there is a selection. Without JS the buttons simply stay enabled.
function updateBulkBar(form) {
  const checked = form.querySelectorAll('input[name="issue"]:checked').length;
  const all = form.querySelectorAll('input[name="issue"]').length;
  const selectAll = form.querySelector("input.select-all");

  form.querySelector(".bulk-count").textContent = checked ? `${checked} selected` : "Select issues to change them together";
  form.querySelectorAll('.bulk-bar button[name="action"]').forEach((button) => {
    button.disabled = checked === 0;
  });

  if (selectAll) {
    selectAll.checked = checked > 0 && checked === all;
    selectAll.indeterminate = checked > 0 && checked < all;
  }
}

// Date range picker (flatpickr): one field where two clicks pick "from" and
// "to", with the days between highlighted. It writes the native from/to
// inputs, now hidden, so the form and the server work as without it.
function setupRangePicker(form) {
  const from = form.querySelector('input[type="date"][name="from"]');
  const to = form.querySelector('input[type="date"][name="to"]');

  if (!window.flatpickr || !from || !to || form.querySelector(".date-range")) {
    return;
  }

  const wrapper = document.createElement("span");
  wrapper.className = "date-range";
  wrapper.innerHTML = '<input type="text" class="date-range-input" placeholder="Date range" aria-label="Date range" readonly>' +
    '<button type="button" class="date-range-clear" title="Clear dates" aria-label="Clear dates">×</button>';
  from.before(wrapper);
  from.type = "hidden";
  to.type = "hidden";

  const changed = () => form.dispatchEvent(new Event("input", { bubbles: true })); // htmx reloads the list

  const picker = window.flatpickr(wrapper.querySelector("input"), {
    mode: "range",
    maxDate: "today", // nothing can have happened in the future
    dateFormat: "Y-m-d",
    altInput: false,
    locale: { firstDayOfWeek: 1, rangeSeparator: " – " },
    defaultDate: [from.value, to.value].filter(Boolean),
    onChange(dates, text, instance) {
      // The first click only starts the range; wait for the second.
      if (dates.length === 1) {
        return;
      }

      from.value = dates[0] ? instance.formatDate(dates[0], "Y-m-d") : "";
      to.value = dates[1] ? instance.formatDate(dates[1], "Y-m-d") : "";
      changed();
    },
  });

  wrapper.querySelector(".date-range-clear").addEventListener("click", () => {
    picker.clear();
    from.value = "";
    to.value = "";
    changed();
  });
}

// Without flatpickr (no JS or failed load) the native pickers stay: the picker
// of "to" starts at "from" and the other way round. The server also swaps a
// reversed range.
function linkDateRange(form) {
  const from = form.querySelector('input[type="date"][name="from"]');
  const to = form.querySelector('input[type="date"][name="to"]');

  if (from && to) {
    to.min = from.value;
    from.max = to.value && to.value < to.max ? to.value : to.max; // "to" if set, else today
  }
}

// Connection form: show only the fields of the selected type, lock the rest
// of an email connection until the address is confirmed, and hide the project
// list while "All projects" is checked (it covers every project).
function updateConnectionForm(form) {
  // A new connection has type radios; an existing one keeps its type on the form.
  const selected = form.querySelector('input[name="type"]:checked');
  const type = selected ? selected.value : form.dataset.type;

  if (selected) {
    form.querySelectorAll(".type-fields").forEach((fields) => {
      fields.hidden = fields.dataset.type !== selected.value;
      // The fields of the other types are neither checked nor sent: a hidden
      // invalid field (a browser filling a login into it) would block the form.
      fields.querySelectorAll("input, select, textarea").forEach((field) => {
        field.disabled = fields.hidden;
      });
    });
  }

  // The name placeholder shows what an empty name becomes.
  const name = form.querySelector('input[name="name"]');

  if (name && type) {
    const address = form.querySelector('input[name="address"]');
    const local = address ? address.value.split("@")[0].trim() : "";

    name.placeholder = type === "email" ? local || "team" : name.dataset[`placeholder${type[0].toUpperCase()}${type.slice(1)}`] || "";
  }

  // A chat's test button works once the fields it needs are filled.
  form.querySelectorAll("button[data-test-needs]").forEach((button) => {
    const fields = button.closest(".type-fields") || form;
    const filled = (name) => {
      const input = fields.querySelector(`[name="${name}"]`);
      return Boolean(input && (input.value.trim() !== "" || input.dataset.saved !== undefined));
    };

    button.disabled = !button.dataset.testNeeds.split("|").some((group) => group.split(" ").every(filled));
  });

  // "Find chat ID" opens the bot's getUpdates once the token is valid (its
  // pattern leaves nothing to escape in the URL). A saved token is not in the
  // page, so it needs to be typed again.
  const botToken = form.querySelector('input[name="bot_token"]');

  form.querySelectorAll("a[data-telegram-updates]").forEach((link) => {
    const token = botToken && !botToken.disabled ? botToken.value.trim() : "";
    const valid = token !== "" && botToken.checkValidity();

    if (valid) {
      link.href = `https://api.telegram.org/bot${token}/getUpdates`;
    } else {
      link.removeAttribute("href");
    }

    link.classList.toggle("disabled", !valid);
    link.setAttribute("aria-disabled", String(!valid));
  });

  // An email connection is filled in only after its address is confirmed.
  const rest = form.querySelector(".connection-rest");

  if (rest && type) {
    rest.disabled = type === "email" && !form.querySelector(".email-step[data-verified]");
  }

  const allProjects = form.querySelector("input.all-projects");
  const projectChoices = form.querySelector(".project-choices");

  if (allProjects && projectChoices) {
    projectChoices.hidden = allProjects.checked;
  }
}

// Closed form sections (details.form-section) show in their heading what is
// set inside; it follows the fields as they change. [data-summary-of] lists
// the badges of the checked boxes of that name ("All projects" while its
// data-summary-all box is checked).
function updateSectionSummaries(form) {
  form.querySelectorAll("[data-summary-of]").forEach((target) => {
    const all = target.dataset.summaryAll && form.querySelector(`input[name="${target.dataset.summaryAll}"]`);

    if (all && all.checked) {
      target.textContent = "All projects";
      return;
    }

    const badges = [...form.querySelectorAll(`input[name="${target.dataset.summaryOf}"]:checked`)].map(summaryBadge);

    if (badges.length === 0) {
      const none = document.createElement("span");
      none.className = "muted";
      none.textContent = "none";
      badges.push(none);
    }

    target.replaceChildren(...badges);
  });
}

// The badge a checkbox shows next to it, or a project badge of its name.
function summaryBadge(input) {
  const label = input.closest("label");
  const badge = label && label.querySelector(".badge");

  if (badge) {
    return badge.cloneNode(true);
  }

  const name = label ? label.textContent.trim() : input.value;
  const project = document.createElement("span");
  project.className = "project";
  project.dataset.project = name;
  project.textContent = name;

  return project;
}

["change", "input"].forEach((type) => {
  document.addEventListener(type, (event) => {
    const form = event.target.form;

    if (form && form.querySelector(".form-section")) {
      updateSectionSummaries(form);
    }
  });
});

// Multi-select filters: the field says how many values are checked ("All
// levels", "Levels: 2"). The form is not swapped by htmx, so it is kept in
// sync here.
function updatePicker(picker) {
  const summary = picker.querySelector(".picker-summary");

  if (!summary) {
    return;
  }

  const count = picker.querySelectorAll("input[name]:checked").length;

  summary.textContent = count ? `${summary.dataset.name}: ${count}` : summary.dataset.all;
}

document.addEventListener("change", (event) => {
  const picker = event.target.closest(".picker");

  if (!picker) {
    return;
  }

  // "All" and the values exclude each other; with nothing chosen, "All" is on.
  const all = picker.querySelector(".picker-clear");
  const values = [...picker.querySelectorAll("input[name]")];

  if (all && event.target === all) {
    values.forEach((input) => {
      input.checked = false;
    });
    all.checked = true;
  } else if (all) {
    all.checked = !values.some((input) => input.checked);
  }

  updatePicker(picker);
});

// Collapse or expand the menu; the cookie lets the server render the next
// page the same way, so it does not jump on load.
document.addEventListener("click", (event) => {
  const toggle = event.target.closest(".sidebar-toggle");

  if (!toggle) {
    return;
  }

  const collapsed = toggle.closest(".app").classList.toggle("sidebar-collapsed");

  toggle.setAttribute("aria-expanded", String(!collapsed));
  document.cookie = `em_sidebar=${collapsed ? "collapsed" : "expanded"}; path=/; max-age=31536000; SameSite=Lax`;
});

// Close an open dropdown on a click outside it or on Escape.
document.addEventListener("click", (event) => {
  document.querySelectorAll(".picker[open]").forEach((picker) => {
    if (!picker.contains(event.target)) {
      picker.open = false;
    }
  });
});

// Esc closes what is open (a dropdown; dialogs and the date picker close
// themselves), otherwise it goes back like the page's "← …" link (data-back).
// Not while typing: there Esc belongs to the field.
//
// The page changes on keyup, and keydown cancels the browser's own Esc:
// Firefox stops loading the page on Esc, which would cancel a navigation
// started on keydown, so the page would just stay.
function backLink(event) {
  const busy = document.querySelector("dialog[open], .flatpickr-calendar.open, .picker[open], [popover].open");
  const typing = event.target instanceof Element && event.target.closest("input, textarea, select, [contenteditable]");

  return busy || typing ? null : document.querySelector("a[data-back]");
}

// The link to follow on the coming keyup, decided on keydown: an Esc that
// closes a dropdown must not also leave the page once the dropdown is gone.
let backOnKeyup = null;

document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape" || event.defaultPrevented) {
    return;
  }

  backOnKeyup = backLink(event);

  if (backOnKeyup) {
    event.preventDefault();

    return;
  }

  document.querySelectorAll(".picker[open]").forEach((picker) => {
    picker.open = false;
  });
});

document.addEventListener("keyup", (event) => {
  if (event.key === "Escape" && backOnKeyup) {
    window.location.href = backOnKeyup.href;
  }

  backOnKeyup = null;
});

function enhance() {
  localizeTimes(document);
  showExpandButtons(document);
  document.querySelectorAll("form.bulk").forEach(updateBulkBar);
  document.querySelectorAll("form.filters").forEach((form) => {
    setupRangePicker(form);
    linkDateRange(form);
  });
  document.querySelectorAll("form.connection-form").forEach(updateConnectionForm);
}

document.addEventListener("change", (event) => {
  const form = event.target.closest("form.connection-form");

  if (!form) {
    return;
  }

  // A new type brings its default for issue links: on for email, off for
  // chats (they may be public), unless the box was set by hand.
  const issueLink = form.querySelector('input[name="include_issue_link"]');

  if (event.target === issueLink) {
    issueLink.dataset.touched = "true";
  } else if (event.target.matches('input[name="type"]') && issueLink && !issueLink.dataset.touched) {
    issueLink.checked = event.target.value === "email";
  }

  updateConnectionForm(form);
});

// Submit the form of a field whose events do not reach it: one outside its
// form (form= attribute, like the page size under a list), or a select that
// shows something else when changed (data-submit-on-change, whose MCP apps).
document.addEventListener("change", (event) => {
  const field = event.target;
  const outside = field.form && field.hasAttribute("form") && !field.form.contains(field);

  if (outside || field.matches("select[data-submit-on-change]")) {
    field.form.requestSubmit();
  }
});

document.addEventListener("input", (event) => {
  if (event.target.matches("form.connection-form input")) {
    updateConnectionForm(event.target.form);
  }

  if (event.target.matches('form.filters input[type="date"]')) {
    linkDateRange(event.target.form);
  }
});

// Checkboxes of the issues list.
document.addEventListener("change", (event) => {
  const form = event.target.closest("form.bulk");

  if (!form) {
    return;
  }

  if (event.target.matches("input.select-all")) {
    form.querySelectorAll('input[name="issue"]').forEach((checkbox) => {
      checkbox.checked = event.target.checked;
    });
  }

  updateBulkBar(form);
});

// Buttons with data-confirm (all issue actions) ask before sending, on the
// issue page and in the list. "{count}" becomes the number of checked issues.
// Capture phase: the request must not start before the answer.
document.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-confirm]");

  if (!button) {
    return;
  }

  const form = button.closest("form");
  const checked = form ? form.querySelectorAll('input[name="issue"]:checked').length : 0;
  const message = button.dataset.confirm.replace("{count}", String(checked));

  if (!window.confirm(message)) {
    event.preventDefault();
    event.stopImmediatePropagation();
  }
}, true);

document.addEventListener("click", (event) => {
  // Select the whole DSN on click, ready to copy.
  if (event.target.matches("input.dsn")) {
    event.target.select();
  }

  const container = event.target.closest(".log-text");

  if (!container) {
    return;
  }

  const body = container.querySelector("pre.log-body");

  if (event.target.matches("button.expand")) {
    const expanded = body.classList.toggle("expanded");

    event.target.textContent = expanded ? "Collapse" : "Expand";
  }

  if (event.target.matches("button.full-screen")) {
    showLogViewer(body.textContent);
  }
});

document.addEventListener("DOMContentLoaded", enhance);
// After an outerHTML swap the event points at the removed element, so rescan the page.
document.addEventListener("htmx:afterSettle", enhance);

// Client tabs keep connected apps in view. Links also work without JavaScript.
function selectMCPClient(tab, focus = false) {
  const settings = tab.closest(".mcp-settings");
  const client = tab.dataset.mcpTab;

  settings.querySelectorAll("[data-mcp-tab]").forEach((item) => {
    const selected = item === tab;
    item.setAttribute("aria-selected", String(selected));
    item.tabIndex = selected ? 0 : -1;
    document.getElementById(item.getAttribute("aria-controls")).hidden = !selected;
  });
  settings.querySelectorAll("[data-mcp-client]").forEach((input) => { input.value = client; });
  history.replaceState(null, "", tab.href);

  if (focus) tab.focus();
}

document.addEventListener("click", (event) => {
  const tab = event.target.closest("[data-mcp-tab]");

  if (!tab || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || event.button !== 0) return;

  event.preventDefault();
  selectMCPClient(tab);
});

document.addEventListener("keydown", (event) => {
  const tab = event.target.closest("[data-mcp-tab]");

  if (!tab || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;

  const tabs = Array.from(tab.closest('[role="tablist"]').querySelectorAll("[data-mcp-tab]"));
  let index = tabs.indexOf(tab);

  if (event.key === "Home") index = 0;
  else if (event.key === "End") index = tabs.length - 1;
  else index = (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;

  event.preventDefault();
  selectMCPClient(tabs[index], true);
});

// Token generation returns just the settings card; keep its selected URL in sync.
document.addEventListener("htmx:afterSwap", (event) => {
  if (event.detail.target.id !== "mcp-settings") return;

  const tab = document.querySelector('[data-mcp-tab][aria-selected="true"]');

  if (tab) history.replaceState(null, "", tab.href);
});

// Popovers close themselves on Esc; mark the open ones so Esc does not also go back.
document.addEventListener("toggle", (event) => {
  if (event.target instanceof Element && event.target.hasAttribute("popover")) {
    event.target.classList.toggle("open", event.newState === "open");
  }
}, true);
