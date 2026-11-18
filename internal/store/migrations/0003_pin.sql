-- PIN для входа (как экран блокировки): хэш и счётчик неудачных попыток.
-- Счётчик хранится у пользователя, а не в сессии — новая сессия его не сбрасывает.
ALTER TABLE users ADD COLUMN pin_hash TEXT;
ALTER TABLE users ADD COLUMN pin_failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN pin_locked_until DATETIME;
