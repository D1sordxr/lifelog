-- Использованные строки Telegram initData (по hash): каждая принимается один раз,
-- чтобы ссылку мини-приложения (#tgWebAppData=…) нельзя было переиспользовать.
CREATE TABLE used_init_data (
    hash       TEXT PRIMARY KEY,
    expires_at DATETIME NOT NULL
);
