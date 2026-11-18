-- Один день = одни сутки бодрствования (от подъёма до отбоя).
-- Все времена хранятся как TEXT в UTC: 'YYYY-MM-DDTHH:MM:SSZ' (сортируются как строки).
CREATE TABLE days (
    id            INTEGER PRIMARY KEY,
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

CREATE INDEX days_wake_at ON days (wake_at);
