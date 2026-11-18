// Вход: отправляем Telegram.WebApp.initData целиком, сервер проверяет подпись.
(function () {
  "use strict";
  var status = document.getElementById("auth-status");
  var hint = document.getElementById("auth-hint");
  var tg = window.Telegram && window.Telegram.WebApp;
  var NOT_ALLOWED = "You are not allowed to use this app";

  if (!tg || !tg.initData) {
    status.textContent = NOT_ALLOWED;
    hint.hidden = false;
    return;
  }
  tg.ready();
  tg.expand();

  // Если после входа снова попали сюда — cookie не сохранилась.
  var tried = false;
  try { tried = !!sessionStorage.getItem("lifelog-auth-tried"); sessionStorage.removeItem("lifelog-auth-tried"); } catch (e) {}
  if (tried) {
    status.textContent = "Не удалось сохранить сессию: этот клиент Telegram блокирует cookies. Попробуй приложение Telegram на телефоне или компьютере.";
    return;
  }

  fetch("/auth/telegram", {
    method: "POST",
    body: new URLSearchParams({ init_data: tg.initData }),
    credentials: "same-origin"
  }).then(function (r) {
    if (r.ok) {
      try { sessionStorage.setItem("lifelog-auth-tried", "1"); } catch (e) {}
      location.reload();
    } else if (r.status === 403) {
      status.textContent = NOT_ALLOWED;
    } else {
      status.textContent = "Вход не удался (HTTP " + r.status + "). Закрой и открой приложение заново.";
    }
  }).catch(function (e) {
    status.textContent = "Нет связи с сервером: " + e.message;
  });
})();
