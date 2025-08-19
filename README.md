# Сокращатель URL — Go + Gin + Postgres + Redis + Prometheus

Сокращатель URL с аналитикой кликов, кэшированием Redis, token-bucket rate limiting, и метриками Prometheus.

## Фичи

**Сокращение URL** со случайными ID или пользовательскими слагами

**Быстрые перенаправления** с помощью кеша Redis

**Аналитика кликов** (временные метки, IP-адрес, UA → устройство/ОС/браузер)

**Token-bucket rate limiting**

**Метрики Prometheus** для HTTP, БД, кэширования, ограничения скорости и пайплайна кликов

**Батчинг** эвентов кликов в БД

**Эндпоинты** состояния и диагностики

## Архитектура

**HTTP**: Gin

**БД**: PostgreSQL через `pgxpool`

**Кэш & RL**: Redis

**Метрики**: `prometheus/client_golang`

**Парсинг UA**: `mssola/user_agent`

## Флоу:

`POST /api/shorten` → создать `{code → target_url}` (PG) и подогреть Redis.

`GET /:code `→ проверить Redis → фоллбек к PG → поставить клик в очередь → перенаправление 302.

Фоновый обработчик группирует события кликов в таблицу `clicks`.

`GET /api/analytics/:code` суммирует клики.

Метрики доступны в `/metrics`.

---

## API

### Сократить ссылку

```
POST /api/shorten
Content-Type: application/json

{
  "url":  "https://example.com/some/long/path",
  "slug": "optional-custom-slug" // опционально
}
```

**Ответ**

* `200 OK`

  ```json
  {
    "code": "abc1234",
    "short_url": "https://your.host/abc1234"
  }
  ```
* `400 Bad Request` — недопустимое тело/URL
* `409 Conflict` — кастомный слаг уже существует
* `429 Too Many Requests` — рейт лимит превышен (с заголовком `Retry-After`)
* `500 Internal Server Error`

### Перенаправление

```
GET /:code  → 302 Found → Location: <target_url>
```

Ошибка: `404 Not Found`, если код не существует.

### Аналитика

```
GET /api/analytics/:code?limit=100
```

**200 OK**

```json
{
  "code": "abc1234",
  "total": 42,
  "last_click": "2025-08-19T12:34:56Z",
  "recent": [
    {
      "timestamp": "2025-08-19T12:34:56Z",
      "ip": "203.0.113.7",
      "user_agent": "Mozilla/5.0 ...",
      "device": "desktop|mobile|bot",
      "os": "Windows 10",
      "browser": "Chrome"
    }
  ]
}
```

### Диагностика и метрики

* `GET /healthz` → `{ "ok": true }`
* `GET /metrics` → Метрики Prometheus

---

## Запуск

```bash
# 1) Клонируем репозиторий
git clone https://github.com/shardy678/url-shortener.git
cd url-shortener

# 2) Поднимаем сервисы (приложение + Postgres + Redis)
docker compose up --build

# 3) Проверяем работу
   Приложение  → http://localhost:8080
   Healthcheck → http://localhost:8080/healthz
   Метрики    → http://localhost:8080/metrics
```