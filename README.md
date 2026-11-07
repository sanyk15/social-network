# social-network

Заготовка социальной сети: регистрация, авторизация и просмотр анкет.
Монолит на Go + PostgreSQL, без ORM.

## Стек

- Go 1.23+ (`net/http`, `pgx`)
- PostgreSQL 16
- bcrypt для паролей

## Эндпоинты

| Метод | Путь           | Авторизация  |
|-------|----------------|--------------|
| POST  | /user/register | нет          |
| POST  | /login         | нет          |
| GET   | /user/get/{id} | Bearer token |
| GET   | /user/search  | Bearer-токен |

### POST /user/register

```json
{
  "username": "alice",
  "password": "secret123",
  "first_name": "Алиса",
  "last_name": "Иванова",
  "birth_date": "1995-04-12",
  "gender": "female",
  "interests": ["programming", "music"],
  "city": "Москва"
}
```

`gender`: `male` | `female` | `other`. `birth_date`: `YYYY-MM-DD`.
Ответы: `201 {"id": 1}`, `400` — ошибка валидации, `409` — логин занят.

### POST /login

```json
{ "username": "alice", "password": "secret123" }
```

Ответы: `200 {"token": "..."}`, `401` — неверные данные.

### GET /user/get/{id}

Заголовок: `Authorization: Bearer <token>`

```json
{
  "id": 1,
  "username": "alice",
  "first_name": "Алиса",
  "last_name": "Иванова",
  "birth_date": "1995-04-12",
  "gender": "female",
  "interests": ["programming", "music"],
  "city": "Москва"
}
```

Ответы: `200`, `401` — нет/протух токен, `404` — не найден.

### GET /user/search

Заголовок: `Authorization: Bearer <token>`

Параметры (оба обязательны): `first_name`, `last_name` — префиксы имени и фамилии.

```bash
curl -G http://localhost:8080/user/search \
  -H "Authorization: Bearer $TOKEN" \
  --data-urlencode "first_name=Конст" \
  --data-urlencode "last_name=Оси"
```

Ответ — массив анкет, отсортированный по `id` (без пагинации). `400` — отсутствует параметр, `401` — нет/протух токен.

## Запуск (Docker, одной командой)

```bash
docker compose up --build
```

Поднимает PostgreSQL и приложение. API доступно на http://localhost:8080.

```bash
docker compose down     # остановить
docker compose down -v  # остановить и удалить данные БД
```

## Репликация (ДЗ-3)

`docker compose up` поднимает primary и две физические streaming-реплики:

| Сервис       | Порт на хосте | Роль                     |
|--------------|---------------|--------------------------|
| `pg-master`  | 5432          | primary, приём записи    |
| `pg-slave-1` | 5433          | hot standby, чтение      |
| `pg-slave-2` | 5434          | hot standby, чтение      |
| `app`        | 8080          | API                      |

Реплики клонируются через `pg_basebackup` при первом старте
(`deploy/postgres/slave-entrypoint.sh`) и стримят WAL с
`application_name=pg-slave-N`. Состояние:

```bash
docker compose exec pg-master psql -U social -d social -c \
  "select application_name, state, sync_state, sent_lsn, replay_lsn from pg_stat_replication"
docker compose exec pg-slave-1 psql -U social -d social -tAc "select pg_is_in_recovery()"
```

### Маршрутизация чтений и записи

- `register`, `login` (вставка сессии) и `/test/write` всегда идут на primary.
- `GET /user/get/{id}`, `GET /user/search`, аутентификация по токену и чтение
  логина при `login` идут на реплику: `Store.WithReadOnly(ctx)` выбирает
  здоровую реплику round-robin и кладёт её в контекст запроса. Это Go-аналог
  Spring `@Transactional(readOnly = true)` — все чтения одного запроса
  попадают в одну и ту же реплику.
- Если реплика отстала и точечное чтение вернуло «не найдено», запрос
  повторяется на primary (`READ_FALLBACK_TO_MASTER`). Это защищает от
  401/404 сразу после записи. Поиск при пустом результате на primary не
  повторяется: пустой ответ — законный.
- Если все реплики недоступны, чтения уходят на primary.

### Кворумная синхронная репликация

```bash
load/switch-replication.sh sync    # synchronous_standby_names = 'ANY 2 (pg-slave-1, pg-slave-2)'
load/switch-replication.sh async   # обратно на асинхронную
```

Настройка ставится через `ALTER SYSTEM` на primary. `postgresql.auto.conf`
локальный и не реплицируется, поэтому после промоушена кворум нужно выставить
заново — уже для одного оставшегося слейва (`ANY 1 (<follower>)`).

### Тестовый API (только при `ENABLE_TEST_API=true`)

| Метод | Путь                 | Назначение                                   |
|-------|----------------------|----------------------------------------------|
| POST  | `/test/write`        | вставка строки в `write_load`, ответ `201`   |
| GET   | `/test/write/count`  | `count(*)` на текущем primary                |
| GET   | `/test/replication`  | LSN, lag, `pg_stat_replication`, счётчики маршрутизации |
| GET   | `/healthz`           | процесс жив                                  |
| GET   | `/readyz`            | primary доступен                             |

### Нагрузочные сценарии и failover

Скрипты лежат в `load/`, результаты — в `load/results/` (вне git).
Приложение и k6 запускаются нативно, в Docker остаются только БД.

```bash
load/run_read_matrix.sh                 # baseline (1 БД) vs master+2 реплики
load/start-stack.sh                     # поднять стек и приложение для замеров записи
load/run_write_bench.sh async           # чистый замер записи
load/run_write_bench.sh sync
load/run_failover_matrix.sh             # kill -9 primary под записью, async и sync
load/summarize.py                       # сводный CSV/Markdown
```

`failover-test.sh` убивает primary через `SIGKILL`, промоутит реплику с
наибольшим `pg_last_wal_replay_lsn()`, переключает вторую реплику на неё,
понижает кворум до `ANY 1 (<follower>)` и сравнивает число подтверждённых
записей (HTTP 201) с числом строк на новом primary.

## Запуск без Docker (приложение локально)

```bash
docker compose up -d pg-master
DATABASE_URL='postgres://social:social@127.0.0.1:5432/social?sslmode=disable' go run ./cmd/server
```

## Генерация данных

```bash
go run ./cmd/seed -csv ../people.v2.csv -n 1000000 -truncate
```

Сидер читает `Фамилия Имя,YYYY-MM-DD,Город`, перемешивает строки (фиксированный seed),
вставляет батчами через `COPY` и обновляет статистику (`ANALYZE`). Пароль всех
сгенерированных анкет — `password123`, логины — `user0000001` … `user1000000`.

## Конфигурация

| Переменная   | По умолчанию                                            |
|--------------|---------------------------------------------------------|
| HTTP_ADDR    | :8080                                                   |
| DATABASE_URL | postgres://social:social@localhost:5432/social?sslmode=disable |
| DATABASE_REPLICA_URLS | пусто (все чтения идут на primary); в compose — оба слейва через запятую |
| READ_FALLBACK_TO_MASTER | true                                          |
| ENABLE_TEST_API | false (в compose — true)                             |
| DB_MAX_CONNS | 16 |
| LOG_REQUESTS | true |

## Postman

Импортируй `postman_collection.json` — запросы по порядку (register → login → get),
`userId` и `token` подставляются автоматически.

## Структура

```
cmd/server/                  — точка входа
cmd/seed/                    — генератор данных
internal/config/             — конфиг из env
internal/store/              — SQL, пулы master/реплик, миграции
internal/store/migrations/   — SQL-миграции (применяются при старте)
internal/httpapi/            — обработчики, health/test API
deploy/postgres/             — init-скрипт primary и entrypoint реплики
load/                        — k6-сценарии, бенчмарки, failover, сводка
```
