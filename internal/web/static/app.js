// Автосохранение полей дня. Без фреймворков.
(function () {
  "use strict";

  function flash(st, text, cls) {
    if (!st) return;
    st.textContent = text;
    st.className = "st show " + (cls || "");
    clearTimeout(st._t);
    if (cls === "ok") st._t = setTimeout(function () { st.classList.remove("show"); }, 1800);
  }

  document.querySelectorAll("form.day").forEach(function (form) {
    var url = "/days/" + form.dataset.id + "/field";
    var timers = {};
    var pending = {}; // name -> value, ещё не отправлено

    function statusFor(el) {
      var f = el.closest(".field");
      return f ? f.querySelector(".st") : null;
    }

    function save(el, value) {
      var name = el.name;
      if (value === undefined) value = el.value;
      delete pending[name];
      clearTimeout(timers[name]);
      var st = statusFor(el);
      flash(st, "сохраняю…");
      var body = new URLSearchParams({ name: name, value: value });
      return fetch(url, { method: "POST", body: body, credentials: "same-origin" })
        .then(function (r) {
          return r.json().then(function (j) {
            if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
            return j;
          });
        })
        .then(function (j) {
          if (el.type !== "hidden" && document.activeElement !== el && el.value !== j.value) el.value = j.value;
          if (el.type === "hidden" || value === "now") el.value = j.value;
          var s = form.querySelector("[data-sleep]");
          if (s && j.sleep) s.textContent = j.sleep;
          flash(st, "сохранено", "ok");
        })
        .catch(function (e) { flash(st, "ошибка: " + e.message, "err"); });
    }

    form.addEventListener("change", function (e) {
      if (e.target.name && e.target.type !== "hidden") save(e.target);
    });

    // Текст сохраняем по ходу набора (iOS может выгрузить страницу без blur).
    form.querySelectorAll("textarea, input[type=text], input[type=number]").forEach(function (el) {
      el.addEventListener("input", function () {
        pending[el.name] = el.value;
        clearTimeout(timers[el.name]);
        timers[el.name] = setTimeout(function () { save(el); }, 1200);
      });
    });

    function flush() {
      Object.keys(pending).forEach(function (name) {
        var body = new URLSearchParams({ name: name, value: pending[name] });
        if (!(navigator.sendBeacon && navigator.sendBeacon(url, body))) {
          fetch(url, { method: "POST", body: body, keepalive: true });
        }
        delete pending[name];
      });
    }
    window.addEventListener("pagehide", flush);
    document.addEventListener("visibilitychange", function () { if (document.hidden) flush(); });

    // Кнопки «сейчас»: время ставит сервер.
    form.querySelectorAll("[data-now]").forEach(function (b) {
      b.addEventListener("click", function () { save(form.elements[b.dataset.now], "now"); });
    });

    // Энергия: повторное нажатие снимает выбор.
    var energy = form.elements.energy;
    form.querySelectorAll("[data-energy]").forEach(function (b) {
      b.addEventListener("click", function () {
        var v = energy.value === b.dataset.energy ? "" : b.dataset.energy;
        energy.value = v;
        form.querySelectorAll("[data-energy]").forEach(function (o) {
          o.setAttribute("aria-pressed", String(o.dataset.energy === v));
        });
        save(energy, v);
      });
    });

    // Enter / «Готово» на клавиатуре не отправляет форму, а просто сохраняет поле.
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
    });
  });

  // Если форма уже отправляется (кнопки «Встал»/«Лёг»), не даём нажать дважды.
  document.querySelectorAll("form:not(.day)").forEach(function (f) {
    f.addEventListener("submit", function (e) {
      if (f.dataset.confirm && !confirm(f.dataset.confirm)) { e.preventDefault(); return; }
      if (f.method.toLowerCase() === "post") {
        f.querySelectorAll("button").forEach(function (b) { setTimeout(function () { b.disabled = true; }, 0); });
      }
    });
  });

  // Копирование Markdown в буфер обмена.
  var copyBtn = document.querySelector("[data-copy-md]");
  if (copyBtn) {
    copyBtn.addEventListener("click", function () {
      var f = copyBtn.form;
      var st = document.querySelector("[data-copy-status]");
      var q = new URLSearchParams({ from: f.elements.from.value, to: f.elements.to.value, inline: "1" });
      var text = fetch("/export.md?" + q).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.text();
      });
      var done = function () { flash(st, "скопировано", "ok"); };
      var fail = function (e) { flash(st, "не удалось: " + e.message, "err"); };
      // Safari требует, чтобы запись в буфер начиналась прямо в обработчике нажатия.
      if (window.ClipboardItem && navigator.clipboard && navigator.clipboard.write) {
        navigator.clipboard.write([new ClipboardItem({ "text/plain": text.then(function (t) { return new Blob([t], { type: "text/plain" }); }) })]).then(done, fail);
      } else if (navigator.clipboard) {
        text.then(function (t) { return navigator.clipboard.writeText(t); }).then(done, fail);
      } else {
        fail(new Error("буфер обмена недоступен"));
      }
    });
  }
})();
