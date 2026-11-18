// Автосохранение полей дня. Без фреймворков.
(window.lifelogReady || Promise.resolve(true)).then(function (ready) {
  "use strict";
  if (!ready) return;

  // --- Telegram Mini App ---
  var tg = window.Telegram && window.Telegram.WebApp;
  if (!(tg && tg.initData)) tg = null;
  if (tg) {
    tg.onEvent("themeChanged", function () { document.documentElement.dataset.theme = tg.colorScheme; });
    if (location.pathname !== "/" && tg.BackButton) {
      tg.BackButton.show();
      tg.BackButton.onClick(function () {
        if (history.length > 1) history.back(); else location.href = "/";
      });
    }
  }

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
    var pending = {}; // поле → новое значение, ожидающее отправки
    var sending = {};
    var storagePrefix = "lifelog-draft-" + form.dataset.userId + "-" + form.dataset.id + "-";
    var revision = 0;

    function statusFor(el) {
      var f = el.closest(".field");
      return f ? f.querySelector(".st") : null;
    }

    function stage(el, value) {
      var entry = { el: el, value: value, draft: JSON.stringify([value, Date.now(), ++revision]) };
      pending[el.name] = entry;
      try { sessionStorage.setItem(storagePrefix + el.name, entry.draft); } catch (e) {}
    }

    function save(name, keepalive) {
      clearTimeout(timers[name]);
      delete timers[name];
      if (sending[name] || !pending[name]) return;
      var entry = pending[name];
      delete pending[name];
      sending[name] = true;
      var el = entry.el;
      var st = statusFor(el);
      flash(st, "сохраняю…");
      var body = new URLSearchParams({ name: name, value: entry.value });
      fetch(url, { method: "POST", body: body, credentials: "same-origin", keepalive: keepalive })
        .then(function (r) {
          return r.json().then(function (j) {
            if (!r.ok) {
              if (j.locked && !document.hidden) location.reload();
              throw new Error(j.error || ("HTTP " + r.status));
            }
            return j;
          });
        })
        .then(function (j) {
          var key = storagePrefix + name;
          try { if (sessionStorage.getItem(key) === entry.draft) sessionStorage.removeItem(key); } catch (e) {}
          if (pending[name]) return;
          if (el.type === "hidden" || entry.value === "now" || document.activeElement !== el) el.value = j.value;
          var s = form.querySelector("[data-sleep]");
          if (s && j.sleep) s.textContent = j.sleep;
          flash(st, "сохранено", "ok");
        })
        .catch(function (e) { flash(st, "ошибка: " + e.message, "err"); })
        .finally(function () {
          delete sending[name];
          if (pending[name] && !timers[name]) save(name, document.hidden);
        });
    }

    function saveValue(el, value) {
      stage(el, value);
      save(el.name, false);
    }

    function showEnergy(value) {
      form.querySelectorAll("[data-energy]").forEach(function (button) {
        button.setAttribute("aria-pressed", String(button.dataset.energy === value));
      });
    }

    // Черновик живёт до успешного ответа, в том числе после ухода или блокировки.
    Array.prototype.forEach.call(form.elements, function (el) {
      if (!el.name) return;
      try {
        var draft = sessionStorage.getItem(storagePrefix + el.name);
        if (draft === null) return;
        var value = JSON.parse(draft)[0];
        if (value !== "now") el.value = value;
        if (el.name === "energy") showEnergy(value);
        pending[el.name] = { el: el, value: value, draft: draft };
        save(el.name, false);
      } catch (e) {}
    });

    form.addEventListener("change", function (e) {
      if (e.target.name && e.target.type !== "hidden") saveValue(e.target, e.target.value);
    });

    // Сохраняем черновик сразу, а запрос откладываем до паузы в наборе.
    form.querySelectorAll("textarea, input[type=text], input[type=number]").forEach(function (el) {
      el.addEventListener("input", function () {
        stage(el, el.value);
        clearTimeout(timers[el.name]);
        timers[el.name] = setTimeout(function () { save(el.name, false); }, 1200);
      });
    });

    function flush() {
      Object.keys(pending).forEach(function (name) { save(name, true); });
    }
    window.addEventListener("pagehide", flush);
    document.addEventListener("visibilitychange", function () { if (document.hidden) flush(); });

    // Кнопки «сейчас»: время ставит сервер.
    form.querySelectorAll("[data-now]").forEach(function (b) {
      b.addEventListener("click", function () { saveValue(form.elements[b.dataset.now], "now"); });
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
      if (f.dataset.confirm) {
        if (tg && tg.isVersionAtLeast("6.2")) {
          e.preventDefault();
          tg.showConfirm(f.dataset.confirm, function (ok) { if (ok) f.submit(); });
          return;
        }
        if (!confirm(f.dataset.confirm)) { e.preventDefault(); return; }
      }
      if (f.method.toLowerCase() === "post") {
        f.querySelectorAll("button").forEach(function (b) { setTimeout(function () { b.disabled = true; }, 0); });
      }
    });
  });

  // Экспорт внутри Telegram: файл качает сам клиент Telegram (без cookie),
  // поэтому берём у сервера короткоживущий токен для ссылки.
  var exportForm = document.querySelector("form.export");
  if (exportForm && tg && tg.isVersionAtLeast("8.0")) {
    exportForm.addEventListener("submit", function (e) {
      var btn = e.submitter;
      if (!btn || !btn.formAction) return;
      e.preventDefault();
      var path = new URL(btn.formAction).pathname; // /export.md
      var q = new URLSearchParams({ from: exportForm.elements.from.value, to: exportForm.elements.to.value });
      var body = new URLSearchParams(q);
      body.set("format", path.slice("/export.".length));
      fetch("/auth/download", { method: "POST", credentials: "same-origin", body: body })
        .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
        .then(function (j) {
          q.set("dl", j.token);
          tg.downloadFile({ url: location.origin + path + "?" + q, file_name: "lifelog" + path.replace("/export", "") });
        })
        .catch(function (err) { tg.showAlert("Не удалось скачать: " + err.message); });
    });
  }

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
});
