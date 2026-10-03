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

## Запуск (Docker, одной командой)

```bash
docker compose up --build
```

Поднимает PostgreSQL и приложение. API доступно на http://localhost:8080.

```bash
docker compose down     # остановить
docker compose down -v  # остановить и удалить данные БД
```

## Запуск без Docker (приложение локально)

```bash
docker compose up -d db
go run ./cmd/server
```

## Конфигурация

| Переменная   | По умолчанию                                            |
|--------------|---------------------------------------------------------|
| HTTP_ADDR    | :8080                                                   |
| DATABASE_URL | postgres://social:social@localhost:5432/social?sslmode=disable |

## Postman

Импортируй `postman_collection.json` — запросы по порядку (register → login → get),
`userId` и `token` подставляются автоматически.

## Структура

```
cmd/server/                  — точка входа
internal/config/             — конфиг из env
internal/store/              — SQL, пул соединений, миграции
internal/store/migrations/   — SQL-миграции (применяются при старте)
internal/httpapi/            — обработчики и хелперы
```
