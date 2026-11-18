-- Пользователи (вход через Telegram). Каждый день принадлежит пользователю.
-- Строка id = 1 — владелец дней, записанных до появления пользователей;
-- её занимает первый пользователь из ALLOWED_USERS при первом входе.
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    tg_id      INTEGER UNIQUE,
    username   TEXT,
    created_at DATETIME NOT NULL
);

INSERT INTO users (id, created_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

-- SQLite не умеет ADD COLUMN ... REFERENCES с непустым DEFAULT при включённых
-- внешних ключах, поэтому таблица пересоздаётся.
CREATE TABLE days_new (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    wake_at       DATETIME NOT NULL,
    bed_at        DATETIME,
    train_start   DATETIME,
    train_minutes INTEGER,
    train_type    TEXT,
    energy        INTEGER CHECK (energy BETWEEN 1 AND 5),
    report        TEXT,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);

INSERT INTO days_new (id, user_id, wake_at, bed_at, train_start, train_minutes, train_type, energy, report, created_at, updated_at)
SELECT id, 1, wake_at, bed_at, train_start, train_minutes, train_type, energy, report, created_at, updated_at FROM days;

DROP TABLE days;
ALTER TABLE days_new RENAME TO days;
CREATE INDEX days_user_wake ON days (user_id, wake_at);
