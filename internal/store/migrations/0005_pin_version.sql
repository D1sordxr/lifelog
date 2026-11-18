-- Смена, сброс и ручная блокировка отзывают все прежние разблокировки.
ALTER TABLE users ADD COLUMN pin_version INTEGER NOT NULL DEFAULT 0;
