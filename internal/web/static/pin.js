// Экран PIN: установка, разблокировка, смена. Проверка — только на сервере.
(window.lifelogReady || Promise.resolve(true)).then(function (ready) {
  "use strict";
  if (!ready) return;
  var root = document.querySelector(".pin");
  var len = parseInt(root.dataset.length, 10) || 6;
  var mode = root.dataset.mode; // setup | unlock | change
  var title = document.getElementById("pin-title");
  var msg = document.getElementById("pin-msg");
  var dots = root.querySelectorAll(".dots span");
  var keys = root.querySelectorAll(".keypad button");
  var tg = window.Telegram && window.Telegram.WebApp;
  if (!(tg && tg.initData)) tg = null;

  // Шаги ввода для каждого режима: имя значения и заголовок.
  var STEPS = {
    setup: [["pin", "Придумай PIN"], ["pin2", "Повтори PIN"]],
    unlock: [["pin", "Введи PIN"]],
    change: [["old", "Текущий PIN"], ["pin", "Новый PIN"], ["pin2", "Повтори новый PIN"]]
  }[mode];
  var URL_ = mode === "change" ? "/pin/change" : "/pin";

  var digits = "";
  var step = 0;
  var values = {};
  var busy = false;

  function draw() { dots.forEach(function (d, i) { d.classList.toggle("on", i < digits.length); }); }
  function say(text) { msg.textContent = text || " "; }
  function goStep(i) { step = i; title.textContent = STEPS[i][1]; }
  function stepOf(name) { for (var i = 0; i < STEPS.length; i++) if (STEPS[i][0] === name) return i; return 0; }
  function fail(text, backTo) {
    say(text);
    goStep(backTo || 0);
    var box = root.querySelector(".dots");
    box.classList.remove("shake"); void box.offsetWidth; box.classList.add("shake");
    if (tg && tg.HapticFeedback) tg.HapticFeedback.notificationOccurred("error");
    digits = ""; draw();
  }
  function disable(sec) {
    keys.forEach(function (k) { k.disabled = true; });
    var t = setInterval(function () {
      sec--;
      if (sec <= 0) {
        clearInterval(t);
        keys.forEach(function (k) { k.disabled = false; });
        say("");
        return;
      }
      say("Ввод заблокирован. Повтори через " + (sec >= 60 ? Math.ceil(sec / 60) + " мин" : sec + " с"));
    }, 1000);
  }

  function submit() {
    busy = true;
    fetch(URL_, { method: "POST", body: new URLSearchParams(values), credentials: "same-origin" })
      .then(function (r) { return r.json().then(function (j) { return { r: r, j: j }; }); })
      .then(function (res) {
        busy = false;
        if (res.r.ok) {
          values = {};
          if (mode === "change") {
            title.textContent = "PIN изменён"; say("");
            if (tg && tg.HapticFeedback) tg.HapticFeedback.notificationOccurred("success");
            setTimeout(function () { location.href = "/data"; }, 900);
          } else {
            location.reload();
          }
          return;
        }
        var retryNew = mode === "change" && res.j.step === "new";
        values = retryNew ? { old: values.old } : {};
        if (res.r.status === 429) { fail(res.j.error); disable(res.j.retry_after || 60); return; }
        if (res.j.locked) { location.reload(); return; }
        fail(res.j.error || ("Ошибка " + res.r.status), retryNew ? stepOf("pin") : 0);
      })
      .catch(function (e) { busy = false; values = {}; fail("Нет связи: " + e.message); });
  }

  function complete() {
    var name = STEPS[step][0];
    values[name] = digits;
    digits = ""; draw();
    if (name === "pin2" && values.pin2 !== values.pin) {
      delete values.pin; delete values.pin2;
      fail("Не совпало — ещё раз", stepOf("pin"));
      return;
    }
    if (step + 1 < STEPS.length) { goStep(step + 1); say(""); return; }
    submit();
  }

  function press(k) {
    if (busy) return;
    if (k === "del") { digits = digits.slice(0, -1); draw(); return; }
    if (digits.length >= len) return;
    digits += k; draw();
    if (tg && tg.HapticFeedback) tg.HapticFeedback.impactOccurred("light");
    if (digits.length === len) setTimeout(complete, 120);
  }

  keys.forEach(function (b) { b.addEventListener("click", function () { press(b.dataset.k); }); });
  document.addEventListener("keydown", function (e) {
    if (e.key >= "0" && e.key <= "9") press(e.key);
    else if (e.key === "Backspace") press("del");
  });
  if (mode === "change" && tg && tg.BackButton) {
    tg.BackButton.show();
    tg.BackButton.onClick(function () { location.href = "/data"; });
  }
});
