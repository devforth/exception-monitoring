// Small enhancements on top of the server-rendered pages. The UI works without them.

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

function enhance() {
  localizeTimes(document);
  showExpandButtons(document);
  document.querySelectorAll("form.bulk").forEach(updateBulkBar);
  document.querySelectorAll("form.filters").forEach((form) => {
    setupRangePicker(form);
    linkDateRange(form);
  });
}

document.addEventListener("input", (event) => {
  if (event.target.matches('form.filters input[type="date"]')) {
    linkDateRange(event.target.form);
  }
});

// Checkboxes of the issues list ("load more" rows included, they are in the same form).
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
