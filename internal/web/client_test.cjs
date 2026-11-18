const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

function runScript(name, globals) {
  return vm.runInNewContext(fs.readFileSync(path.join(__dirname, "static", name), "utf8"), {
    URLSearchParams,
    ...globals
  }, { filename: name });
}

function memoryStorage() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key)
  };
}

const tick = () => new Promise(setImmediate);

test("свежий запуск проверяется в живой сессии, навигация не отправляет его повторно", async () => {
  const sessionStorage = memoryStorage();
  const sent = [];
  let reloads = 0;
  let reveals = 0;
  async function open(hash, changed) {
    const window = {
      addEventListener() {},
      Telegram: { WebApp: { initData: "hash=" + hash, ready() {}, expand() {} } }
    };
    const status = {};
    runScript("auth.js", {
      window,
      sessionStorage,
      document: {
        getElementById: (id) => id === "tg-status" ? status : null,
        documentElement: { classList: { remove() { reveals++; } } }
      },
      location: { reload() { reloads++; } },
      fetch: async (url, options) => {
        sent.push([url, options.body.get("init_data")]);
        return { ok: true, json: async () => ({ changed }) };
      }
    });
    return window.lifelogReady;
  }
  assert.equal(await open("abc", false), true);
  assert.equal(await open("ABC", false), true);
  assert.equal(await open("def", true), false);
  assert.equal(await open("def", false), true);
  assert.deepEqual(sent, [["/auth/telegram", "hash=abc"], ["/auth/telegram", "hash=def"]]);
  assert.equal(reloads, 1);
  assert.equal(reveals, 3);
});

test("при повторе шага нового PIN сохраняется текущий PIN", async () => {
  let press;
  const submitted = [];
  const timers = [];
  const dots = { classList: { add() {}, remove() {} } };
  const root = {
    dataset: { mode: "change", length: "6" },
    querySelectorAll: () => [],
    querySelector: () => dots
  };
  runScript("pin.js", {
    window: {},
    document: {
      querySelector: () => root,
      getElementById: () => ({}),
      addEventListener: (event, handler) => { press = handler; }
    },
    setTimeout: (fn) => timers.push(fn),
    fetch: async (url, { body }) => {
      submitted.push(Object.fromEntries(body));
      return { ok: false, status: 400, json: async () => ({ error: "Проверь новый PIN", step: "new" }) };
    }
  });
  await tick();
  function enter(digits) {
    for (const key of digits) press({ key });
    timers.shift()();
  }
  enter("123456");
  enter("123456");
  enter("123456");
  await tick();
  enter("654321");
  enter("654321");
  assert.deepEqual(submitted, [
    { old: "123456", pin: "123456", pin2: "123456" },
    { old: "123456", pin: "654321", pin2: "654321" }
  ]);
});

test("черновик переживает уход без подтверждения и восстанавливается только для своего пользователя", async () => {
  const sessionStorage = memoryStorage();
  const sent = [];
  function open(userId, locked) {
    let input;
    let pagehide;
    const report = {
      name: "report", value: "", closest: () => null,
      addEventListener: (event, fn) => { input = fn; }
    };
    const form = {
      dataset: { id: "7", userId },
      elements: [report],
      addEventListener() {},
      querySelector: () => null,
      querySelectorAll: (selector) => selector.startsWith("textarea") ? [report] : []
    };
    runScript("app.js", {
      window: { addEventListener: (event, fn) => { pagehide = fn; } },
      document: {
        hidden: locked,
        querySelectorAll: (selector) => selector === "form.day" ? [form] : [],
        querySelector: () => null,
        addEventListener() {}
      },
      sessionStorage, setTimeout: () => 1, clearTimeout() {},
      fetch: async (url, { keepalive, body }) => {
        sent.push([url, keepalive, body.get("value")]);
        return locked
          ? { ok: false, status: 401, json: async () => ({ locked: true }) }
          : { ok: true, json: async () => ({ value: body.get("value") }) };
      }
    });
    return { report, type(value) { report.value = value; input(); }, hide() { pagehide(); } };
  }
  const key = "lifelog-draft-1-7-report";
  const first = open("1", true);
  await tick();
  first.type("Черновик");
  assert.notEqual(sessionStorage.getItem(key), null);
  first.hide();
  await tick();
  assert.deepEqual(sent, [["/days/7/field", true, "Черновик"]]);
  assert.notEqual(sessionStorage.getItem(key), null);

  const other = open("2", false);
  await tick();
  assert.equal(other.report.value, "");
  assert.equal(sent.length, 1);

  const restored = open("1", false);
  await tick();
  assert.equal(restored.report.value, "Черновик");
  assert.deepEqual(sent[1], ["/days/7/field", false, "Черновик"]);
  assert.equal(sessionStorage.getItem(key), null);
});
