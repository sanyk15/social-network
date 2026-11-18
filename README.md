# social-network

Социальная сеть: анкеты, дружба, посты и лента друзей.
Монолит на Go + PostgreSQL + Redis, без ORM.

## Стек

- Go 1.25+ (`net/http`, `pgx`); образ собирается на `golang:1.27-alpine`
- PostgreSQL 16 (+ две реплики, ДЗ-3)
- Redis 7 — кэш ленты, кэш постов и очередь fan-out (ДЗ-4)
- bcrypt для паролей

## Эндпоинты

| Метод | Путь                      | Авторизация  |
|-------|---------------------------|--------------|
| POST  | /user/register            | нет          |
| POST  | /login                    | нет          |
| GET   | /user/get/{id}            | Bearer token |
| GET   | /user/search              | Bearer-токен |
| POST  | /friend/add               | Bearer token |
| POST  | /friend/delete            | Bearer token |
| PUT   | /friend/set/{user_id}     | Bearer token |
| PUT   | /friend/delete/{user_id}  | Bearer token |
| POST  | /post/create              | Bearer token |
| POST  | /post/update              | Bearer token |
| PUT   | /post/update              | Bearer token |
| POST  | /post/delete              | Bearer token |
| PUT   | /post/delete/{id}         | Bearer token |
| GET   | /post/get?id=N            | Bearer token |
| GET   | /post/get/{id}            | Bearer token |
| GET   | /post/feed                | Bearer token |

Методы дружбы и постов принимают обе формы из ДЗ и из спецификации курса:
`/friend/add` с телом `{"user_id": N}` и `PUT /friend/set/{user_id}`,
`POST /post/delete` с телом `{"id": N}` и `PUT /post/delete/{id}`.
Идентификаторы в JSON можно передавать и числом, и строкой.

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

## Лента постов друзей (ДЗ-4)

### API

- `POST /friend/add` (или `PUT /friend/set/{user_id}`) — тело `{"user_id": 2}`,
  дружба симметричная. `200 {"ok":true,"created":true|false}`.
- `POST /friend/delete` (или `PUT /friend/delete/{user_id}`) — тело
  `{"user_id": 2}`.
- `POST /post/create` — тело `{"text": "..."}` (до 4096 символов).
  `200 {"id": 1, "post": {...}}`.
- `POST|PUT /post/update` — тело `{"id": 1, "text": "новый текст"}`.
- `POST /post/delete` с телом `{"id": 1}` или `PUT /post/delete/1`.
- `GET /post/get/{id}` или `GET /post/get?id=1` → `{"id","text","author_user_id","created_at"}`.
- `GET /post/feed?limit=10&offset=0` → массив постов, новые первыми.
  `limit` по умолчанию 10, максимум 1000.

`text` и посты проверяются на длину; изменять и удалять пост может только автор
(иначе `404`, чтобы не раскрывать существование чужих постов).

### Схема данных

- `friends(user_id, friend_id)` — одна строка на пару, `user_id < friend_id`
  (CHECK). Обратное направление покрыто индексом `friends_friend_id_idx`.
- `posts(id BIGSERIAL, author_id, text, created_at, updated_at, deleted_at)` —
  мягкое удаление. Индекс `posts_author_live_idx (author_id, id DESC)
  WHERE deleted_at IS NULL` нужен для перестройки ленты из БД.

Идентификатор поста — `BIGSERIAL`, то есть растёт вместе со временем вставки.
Поэтому «последние 1000 постов» — это «1000 максимальных id», и в Redis
сортированный набор использует id и как score, и как member. Это даёт строгий
порядок без коллизий по времени.

### Как работает кэш

Лента формируется на уровне кэша: `GET /post/feed` читает только Redis-набор
`sn:feed:{user_id}`, а из PostgreSQL лента достаётся лишь при промахе — и сразу
кладётся в кэш (cache-aside).

```
sn:feed:{uid}          ZSET   посты ленты, member = score = post id (≤ 1000)
sn:feed:ready:{uid}    STR    "1" — набор построен из БД (пустая лента ≠ промах)
sn:feed:rebuild:{uid}  STR    "1" — идёт перестройка
sn:feed:dirty:{uid}    STR    "1" — запись пришла во время перестройки
sn:friends:{uid}       SET    кэш списка друзей для fan-out
sn:post:{id}           STR    JSON поста (кэш на час)
sn:feed:jobs           STREAM очередь fan-out (режим queue)
```

Запись:

- **Создание поста** — `ZADD` в ленту каждого друга и в свою ленту
  (`FEED_INCLUDE_OWN=true`), затем `ZREMRANGEBYRANK` оставляет 1000 старших id.
  Fan-out выполняется пайплайном Lua-скриптов (по одному на получателя).
- **Редактирование** — обновляет строку и удаляет `sn:post:{id}`; лента не
  перестраивается, потому что порядок задаётся id, а не временем правки.
  Следующее чтение ленты заново подтянет текст из БД.
- **Удаление** — мягкое удаление, удаление `sn:post:{id}` и `ZREM` из лент
  автора и его друзей. Плюс ленивая фильтрация: если id всё же остался в
  наборе, пост не вернётся из БД и не попадёт в выдачу.
- **Добавление/удаление друга** — удаляются `sn:feed` и `sn:feed:ready` обоих
  пользователей и кэш друзей; следующее чтение перестраивает ленту из БД,
  поэтому новый друг появляется/исчезает сразу.

TTL ленты и флага `ready` — 10 минут (`FEED_CACHE_TTL`), продлевается при
каждом чтении. Если ключи истекли, лента перестраивается из БД, так что кэш
никогда не является источником истины.

### Гонка «перестройка vs fan-out»

Перестройка читает БД и затем пишет ZSET. Параллельный fan-out может прийти
между этими шагами; наивный `ZADD` был бы затёрт `DEL` перестройки. Поэтому:

1. перестройка ставит `sn:feed:rebuild:{uid}` **до** чтения БД;
2. fan-out видит флаг и не пишет в набор, а ставит `sn:feed:dirty:{uid}`;
3. перестройка после записи удаляет флаг и проверяет dirty — если он есть,
   перечитывает БД (до 3 раундов, затем снимает `ready`, чтобы следующий
   читатель перестроил ленту).

Fan-out, который не увидел флаг, гарантированно завершился до чтения БД, значит
его пост уже попал в выборку. Дополнительно перестройки сериализует
`SET NX` на флаге: второй читатель ждёт `ready`, а не дублирует работу.

### Перестройка кэшей из СУБД

Три способа:

```bash
# 1. лениво: удалить ключи и прочитать ленту
redis-cli DEL sn:feed:42 sn:feed:ready:42
curl -H "Authorization: Bearer $TOKEN" 'localhost:8080/post/feed?limit=10'

# 2. HTTP (ENABLE_TEST_API=true)
curl -X POST localhost:8080/admin/feed/rebuild \
  -H "Authorization: Bearer $TOKEN" -d '{"user_id": 42}'
curl -X POST localhost:8080/admin/feed/rebuild \
  -H "Authorization: Bearer $TOKEN" -d '{"all": true}'

# 3. CLI/докер
docker compose --profile admin run --rm feedrebuild -user-id 42
docker compose --profile admin run --rm feedrebuild -all -batch 500
```

Диагностика кэша и очереди: `GET /admin/feed/status?user_id=42` (готовность,
размер, TTL) и `GET /admin/feed/stats` (попадания/промахи, перестройки,
fan-out, длина и pending очереди).

### Очередь (опционально по заданию)

По умолчанию `FEED_FANOUT_MODE=sync`: лента друга обновляется в том же запросе,
поэтому «создал пост → сразу прочитал ленту» детерминировано. С
`FEED_FANOUT_MODE=queue` создание поста кладёт job в Redis Stream
`sn:feed:jobs`, а `FEED_WORKERS` воркеров consumer group `feed-workers`
разбирают его:

- чтение `XREADGROUP`, до 64 сообщений за раз, блокировка 2 с;
- обработка до 3 попыток, затем `XACK` (зависший job не блокирует поток —
  лента всё равно восстановится из БД по TTL);
- `XAUTOCLAIM`/`XCLAIM` возвращает сообщения, не подтверждённые упавшим
  воркером (idle > 60 с);
- `XADD MAXLEN ~ 100000` ограничивает длину потока.

Для демонстрации очереди:

```bash
FEED_FANOUT_MODE=queue docker compose up -d --build
load/feed-smoke.sh            # те же проверки, но через очередь
```

### Наполнение тестовыми данными

Сначала пользователи (ДЗ-1/2):

```bash
docker compose --profile seed run --rm seed     # или go run ./cmd/seed
```

Затем дружба и посты:

```bash
# тексты из файла курса (скачать один раз)
curl -L -o ../posts.txt https://raw.githubusercontent.com/OtusTeam/highload/master/homework/posts.txt

docker compose --profile seed run --rm \
  -v "$PWD/../posts.txt:/data/posts.txt:ro" \
  seedfeed -posts-file /data/posts.txt -users 1000 -friends 20 -posts 100000
```

Без смонтированного файла `seedfeed` сгенерирует lorem ipsum сам:

```bash
docker compose --profile seed run --rm seedfeed
```

Сидер связывает дружбой первых `-users` пользователей (в среднем `-friends`
друзей на каждого), вставляет `-posts` постов со случайными авторами и
возрастающим `created_at` (поэтому id идут по таймлайну) и обновляет
статистику. `-truncate` очищает `posts` и `friends`. В конце сидер сбрасывает
кэш лент (`sn:feed:*`, `sn:post:*`, `sn:friends:*`): `COPY` идёт мимо
fan-out, поэтому закэшированные ленты устарели. Без `REDIS_URL`/`-redis`
кэш не трогается — тогда нужен `feedrebuild` или ожидание TTL.

Отчёт по ДЗ-4 и результаты проверки: `../reports/feed-cache/report.md` (вне git).

### Проверка

```bash
docker compose up -d --build
load/feed-smoke.sh
```

Скрипт проверяет: пост не виден до дружбы; после `friend/add` появляется в
ленте; правка текста видна (инвалидация `sn:post`); удаление убирает пост;
явная и ленивая перестройка из БД; `friend/delete` убирает посты; в ленте
остаются ровно 1000 последних постов (создаёт 1005 и проверяет вытеснение).
Кэш-проверки требуют `redis-cli`; без него они пропускаются.

## Запуск (Docker, одной командой)

```bash
docker compose up --build
```

Поднимает PostgreSQL с репликами, Redis и приложение. API доступно на
http://localhost:8080.

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
| `redis`      | 6379          | кэш ленты и очередь      |
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
- Лента строится из СУБД на primary (перестройка не должна отставать от
  fan-out), а тела постов для выдачи читаются с реплик с тем же fallback.
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
docker compose up -d pg-master redis
DATABASE_URL='postgres://social:social@127.0.0.1:5432/social?sslmode=disable' \
REDIS_URL='redis://127.0.0.1:6379/0' \
go run ./cmd/server
```

## Генерация данных

```bash
go run ./cmd/seed -csv ../people.v2.csv -n 1000000 -truncate
```

Сидер читает `Фамилия Имя,YYYY-MM-DD,Город`, перемешивает строки (фиксированный seed),
вставляет батчами через `COPY` и обновляет статистику (`ANALYZE`). Пароль всех
сгенерированных анкет — `password123`, логины — `user0000001` … `user1000000`.

Дружба и посты для ДЗ-4:

```bash
go run ./cmd/seedfeed -users 1000 -friends 20 -posts 100000 -truncate
```

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
| REDIS_URL | redis://localhost:6379/0 |
| FEED_SIZE | 1000 |
| FEED_CACHE_TTL | 10m |
| FRIENDS_CACHE_TTL | 10m |
| POST_CACHE_TTL | 1h |
| FEED_INCLUDE_OWN | true (посты автора попадают в его же ленту) |
| FEED_FANOUT_MODE | sync (queue — fan-out через Redis Streams) |
| FEED_WORKERS | 2 |

## Postman

Импортируй `postman_collection.json` — запросы по порядку (register → login →
get/search → friend → post → feed), `userId`, `secondUserId`, `token` и `postId`
подставляются автоматически.

## Структура

```
cmd/server/                  — точка входа
cmd/seed/                    — генератор пользователей
cmd/seedfeed/                — генератор дружбы и постов (ДЗ-4)
cmd/feedrebuild/             — перестройка лент из СУБД (ДЗ-4)
internal/config/             — конфиг из env
internal/store/              — SQL, пулы master/реплик, миграции
internal/store/migrations/   — SQL-миграции (применяются при старте)
internal/cache/              — Redis-клиент
internal/feed/               — кэш ленты, fan-out, очередь и перестройка
internal/httpapi/            — обработчики, health/test API, admin-кэш
deploy/postgres/             — init-скрипт primary и entrypoint реплики
load/                        — k6-сценарии, бенчмарки, failover, feed-smoke
```
