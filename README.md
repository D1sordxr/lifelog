# lifelog

Минималистичный личный веб-журнал дня: подъём, тренировка, энергия, отчёт, отбой.
Один пользователь, один бинарник, одна база SQLite. Заполняется с телефона и ПК,
доступен только внутри своей сети Tailscale.

- **Одна запись = одни сутки бодрствования** (от подъёма до отбоя), а не календарный день.
  Дата дня — локальная дата подъёма; отбой может быть уже следующей датой.
- **Сон** не хранится, а вычисляется: подъём этого дня − отбой предыдущего.
  Если отбой не отмечен или между ними больше 20 часов (пропущены дни) — сон неизвестен.
- Go + `net/http` + `html/template` + SQLite (`modernc.org/sqlite`, без CGO).
  Шаблоны и статика вшиты в бинарник через `embed`. Фронт — HTML, CSS и немного JS, без сборки.

## Страницы и эндпоинты

| | |
|---|---|
| `GET /` | «Сегодня»: «Встал сейчас» → форма дня с автосохранением → «Лёг сейчас» |
| `GET /history` | все дни, новые сверху; любой можно открыть и поправить |
| `GET /days/{id}` | редактирование дня (включая отбой), удаление |
| `GET /chart?d=30\|60\|90` | график сна: дни × время суток, полоса от отбоя до подъёма |
| `GET /data` | экспорт и импорт |
| `GET /export.csv` `/export.json` `/export.md` | экспорт, параметры `?from=ГГГГ-ММ-ДД&to=ГГГГ-ММ-ДД` (включительно), `&inline=1` — показать вместо скачивания |
| `POST /import` | импорт CSV (файл `file` или текст `csv`) |
| `POST /wake` | новый день с подъёмом «сейчас» |
| `POST /days/{id}/field` | автосохранение одного поля: `name`, `value` (`now` для времени) → JSON |
| `POST /days/{id}/bed` | отбой «сейчас» |
| `GET /healthz` | проверка живости |

Во всех экспортах есть вычисленная длительность сна. Markdown удобно вставлять в чат
с ИИ — на странице «Данные» есть кнопка «Скопировать Markdown».

## Запуск локально

```bash
go run .
```

Откроется на <http://127.0.0.1:8080>, база — `./lifelog.db`.

```bash
go test ./...
```

### Настройки (переменные окружения)

| Переменная | По умолчанию | |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | адрес. Оставляйте localhost — наружу через `tailscale serve` |
| `DB_PATH` | `lifelog.db`; у службы Windows — `%ProgramData%\lifelog\lifelog.db` | файл базы |
| `TZ` | часовой пояс системы | пояс для отображения и ввода, например `Europe/Moscow` |
| `BACKUP_DIR` | `<папка базы>/backups` | куда класть ежедневные копии |
| `BACKUP_KEEP` | `30` | сколько копий хранить |
| `BASIC_AUTH_USER`, `BASIC_AUTH_PASS` | — | если заданы обе — включается basic auth |

## Установка на Windows (служба)

Нужен Go 1.26+ и Tailscale. Все команды — в PowerShell **от имени администратора**.

```powershell
git clone https://github.com/D1sordxr/lifelog.git
cd lifelog
go build -o lifelog.exe .
.\lifelog.exe install
```

`install` делает следующее:

- копирует exe в `C:\Program Files\lifelog\`;
- создаёт службу `lifelog`: автозапуск при загрузке, перезапуск через 5 с после сбоя,
  запуск под виртуальной учётной записью `NT SERVICE\lifelog` (без пароля и без прав администратора);
- создаёт `C:\ProgramData\lifelog\` (база, `backups\`, `lifelog.log`) с доступом только
  для администраторов, SYSTEM и самой службы;
- запускает службу.

Проверка: `curl.exe http://127.0.0.1:8080/healthz` → `ok`.

Чтобы задать настройки, выставьте переменные перед `install` — они сохранятся в параметрах службы:

```powershell
$env:TZ = "Europe/Moscow"
.\lifelog.exe install
```

Если при `install` ни одна переменная не задана, прежние настройки службы не меняются.

**Обновление:** `git pull`, `go build -o lifelog.exe .`, `.\lifelog.exe install`
(служба остановится, exe заменится, служба запустится; данные не трогаются).

**Удаление:** `.\lifelog.exe uninstall` — удаляет службу, база остаётся.

Управление: `Get-Service lifelog`, `Restart-Service lifelog`, лог — `C:\ProgramData\lifelog\lifelog.log`.

> Если ПК уходит в сон, журнал недоступен. Для сервера отключите сон
> в «Параметры → Система → Питание».

## Доступ через Tailscale (HTTPS)

1. В админке Tailscale включите **MagicDNS** и **HTTPS Certificates** (DNS → HTTPS Certificates).
2. На машине с lifelog (на Windows — от администратора):

   ```bash
   tailscale serve --bg 8080
   ```

   Это проксирует `https://<машина>.<tailnet>.ts.net` → `http://127.0.0.1:8080` с сертификатом `*.ts.net`.
   С `--bg` настройка хранится в Tailscale и **переживает перезагрузку**.
   Проверить — `tailscale serve status`, выключить — `tailscale serve --https=443 off`.
3. Никакого `tailscale funnel`: приложение видно только устройствам вашего tailnet.

### iPhone

Откройте `https://<машина>.<tailnet>.ts.net` в Safari (Tailscale на телефоне включён) →
«Поделиться» → «На экран „Домой“». Приложение откроется без панелей браузера, как обычное.

## Linux (systemd)

```bash
GOOS=linux GOARCH=amd64 go build -o lifelog .   # arm64 для ARM-серверов
sudo useradd --system --home /var/lib/lifelog --shell /usr/sbin/nologin lifelog
sudo install -d -o lifelog -g lifelog -m 750 /var/lib/lifelog
sudo install -D -m 755 lifelog /opt/lifelog/lifelog
sudo cp deploy/linux/lifelog.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now lifelog
sudo tailscale serve --bg 8080
```

## Бэкапы

Встроены в приложение: при старте и затем раз в час проверяется, есть ли копия за сегодня.
Если нет — делается `VACUUM INTO` в `BACKUP_DIR/lifelog-ГГГГ-ММ-ДД.db`, и остаются только
`BACKUP_KEEP` последних копий. Копия — обычная база SQLite: чтобы восстановиться,
остановите службу и положите копию на место `lifelog.db` (удалив `-wal`/`-shm`).
Разовая копия: `lifelog backup`.

База работает в режиме WAL.

## Импорт старых записей

CSV в формате экспорта, колонки по заголовку в любом порядке:
`date, wake_at, bed_at, train_start, train_minutes, train_type, energy, report`
(`sleep_minutes` игнорируется). Обязательна `wake_at`. Время — полное
(`2026-01-31T07:30:00+03:00`, `2026-01-31 07:30`) или `ЧЧ:ММ` при заполненной `date`
(`2026-01-31` или `31.01.2026`); отбой и начало тренировки раньше подъёма относятся к
следующим суткам. Если день с такой датой подъёма уже есть, непустые ячейки его обновляют.
Разделитель — запятая или точка с запятой. При любой ошибке не импортируется ничего.

```csv
date,wake_at,bed_at,train_minutes,train_type,energy,report
2026-01-30,07:30,23:45,40,бег,4,Обычный день
2026-01-31,08:10,00:30,,,3,"Лёг поздно, отчёт с запятой"
```

## Устройство

```
main.go                     конфиг, запуск, команды backup/install/uninstall
platform_windows.go         служба Windows: SCM, установка, права на папку данных
platform_other.go           не-Windows: обычный процесс
internal/store/             SQLite, миграции, Day, сон, текущий день, импорт, бэкапы
  migrations/NNNN_*.sql     версии схемы
  fields.go                 реестр редактируемых полей (разбор и проверка ввода)
internal/export/            CSV / JSON / Markdown и разбор CSV
internal/web/               обработчики, график (SVG на сервере), шаблоны, статика
tools/genicon/              генератор иконок PWA (go run ./tools/genicon)
deploy/linux/               systemd-юнит
```

«Текущий день» — самый поздний по подъёму день, если у него ещё нет отбоя. Старые дни
без отбоя (забыли нажать «Лёг») текущими не становятся; если текущий день открыт больше
20 часов, на главной появится подсказка и кнопка «Встал сейчас — новый день».

### Миграции

Файлы `internal/store/migrations/NNNN_описание.sql` вшиваются в бинарник. При старте
применяются по порядку номеров те, которых нет в таблице `schema_migrations`, каждая в своей
транзакции. Уже применённые файлы не меняйте — добавляйте новые.

### Как добавить новое поле

Пример: «шаги за день».

1. Миграция `internal/store/migrations/0002_steps.sql`:
   ```sql
   ALTER TABLE days ADD COLUMN steps INTEGER;
   ```
2. `internal/store/day.go`: поле `Steps *int` в `Day`; колонку — в `dayCols`, `scanDay`,
   `insertDay`, `updateDay`.
3. `internal/store/fields.go`: запись в `Fields` с `Set` (разбор и проверка) и `Get`.
   Через реестр поле сразу заработает в автосохранении, сохранении формы и импорте.
4. `internal/web/templates/_dayform.html`: `<input name="steps" value="{{$v.steps}}">` в блоке `.field`.
5. По желанию: колонка в `internal/export` (`Columns`, `WriteCSV`, `jsonDay`, Markdown) и в истории.
6. `go test ./...`.
