// Проверяем свежий запуск Telegram и на страницах с уже действующей сессией.
(function () {
  "use strict";
  var loginStatus = document.getElementById("auth-status");
  var status = loginStatus || document.getElementById("tg-status");
  var hint = document.getElementById("auth-hint");
  var tg = window.Telegram && window.Telegram.WebApp;

  function reveal() {
    document.documentElement.classList.remove("tg-checking");
    return true;
  }

  function login() {
    if (!tg || !tg.initData) {
      if (!loginStatus) return reveal();
      status.textContent = "You are not allowed to use this app";
      hint.hidden = false;
      return false;
    }
    tg.ready();
    tg.expand();
    var hash = new URLSearchParams(tg.initData).get("hash");
    var key = hash ? "lifelog-init-" + hash.toLowerCase() : "";
    try {
      if (key && sessionStorage.getItem(key)) {
        if (!loginStatus) return reveal();
        status.textContent = "Сессия недоступна. Закрой и открой журнал заново из Telegram.";
        return false;
      }
    } catch (e) {}

    return fetch("/auth/telegram", {
      method: "POST",
      body: new URLSearchParams({ init_data: tg.initData }),
      credentials: "same-origin"
    }).then(function (r) {
      return r.json().then(function (result) {
        if (!r.ok) {
          status.textContent = result.error || "Вход не удался. Открой журнал заново из Telegram.";
          return false;
        }
        try { if (key) sessionStorage.setItem(key, "1"); } catch (e) {}
        if (result.changed) {
          location.reload();
          return false;
        }
        return reveal();
      });
    }).catch(function () {
      status.textContent = "Не удалось проверить вход. Открой журнал заново из Telegram.";
      return false;
    });
  }

  window.lifelogReady = Promise.resolve().then(login);
  window.addEventListener("pageshow", function (event) {
    if (event.persisted) location.reload();
  });
})();
