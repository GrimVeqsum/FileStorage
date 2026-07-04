# File Storage Service

Небольшой backend-сервис для загрузки, хранения, скачивания и удаления файлов.

Файл загружается через HTTP API. Метаданные файла сохраняются в PostgreSQL, а сам файл хранится в MinIO как в S3-compatible storage.

## Что умеет сервис

- загрузить файл через `multipart/form-data`;
- сохранить информацию о файле в PostgreSQL;
- сохранить сам файл в MinIO;
- получить список загруженных файлов;
- скачать файл по `id`;
- удалить файл;
- проверить состояние сервиса через `health` и `ready`;
- вернуть понятные ошибки, если файл не найден, пустой, слишком большой или storage недоступен.

## Стек

- Go
- net/http
- PostgreSQL
- MinIO
- Docker Compose
- SQL migrations
- structured logging через `slog`

## Структура проекта

```text
cmd/app                 точка входа в приложение
internal/config         чтение настроек из env
internal/httptransport  роутер и HTTP endpoints
internal/files          handler, service, repository для файлов
internal/storage        работа с MinIO
internal/response       JSON-ответы и ошибки
migrations              SQL-миграции для PostgreSQL
```

## API

### GET /health

Проверяет, что приложение запущено.

Пример запроса:

```bash
curl http://localhost:8080/health
```

Пример ответа:

```json
{
  "status": "ok"
}
```

### GET /ready

Проверяет, что приложение готово к работе: есть подключение к PostgreSQL и MinIO.

Пример запроса:

```bash
curl http://localhost:8080/ready
```

Пример ответа:

```json
{
  "status": "ready"
}
```

Если PostgreSQL или MinIO недоступны, сервис вернет ошибку:

```json
{
  "error": "service_not_ready",
  "message": "service is not ready"
}
```

### POST /files

Загружает файл в сервис.

Пример запроса:

```bash
curl -X POST http://localhost:8080/files -F "file=@test.txt"
```

Пример ответа:

```json
{
  "id": "5fd42fe2-50af-45a2-b3fb-fffc7248e789",
  "original_name": "test.txt",
  "content_type": "text/plain",
  "size": 23,
  "created_at": "2026-07-04T18:33:46.038036Z"
}
```

После загрузки сам файл сохраняется в MinIO, а информация о нем сохраняется в PostgreSQL.

### GET /files

Возвращает список загруженных файлов.

Пример запроса:

```bash
curl http://localhost:8080/files
```

Пример ответа:

```json
[
  {
    "id": "5fd42fe2-50af-45a2-b3fb-fffc7248e789",
    "original_name": "test.txt",
    "content_type": "text/plain",
    "size": 23,
    "created_at": "2026-07-04T18:33:46.038036Z"
  }
]
```

Если файлов нет, вернется пустой список:

```json
[]
```

### GET /files/{id}

Скачивает файл по его `id`.

Пример запроса:

```bash
curl -L http://localhost:8080/files/5fd42fe2-50af-45a2-b3fb-fffc7248e789 -o downloaded-test.txt
```

Сервис находит metadata файла в PostgreSQL, берет `storage_key`, скачивает файл из MinIO и отдает его клиенту.

Если файл не найден:

```json
{
  "error": "file_not_found",
  "message": "file not found"
}
```

### DELETE /files/{id}

Удаляет файл по его `id`.

Пример запроса:

```bash
curl -X DELETE http://localhost:8080/files/5fd42fe2-50af-45a2-b3fb-fffc7248e789
```

При успешном удалении сервис вернет статус:

```http
204 No Content
```

Тела ответа не будет.

При удалении сервис удаляет сам файл из MinIO и metadata из PostgreSQL.

Если файл не найден:

```json
{
  "error": "file_not_found",
  "message": "file not found"
}
```

## Возможные ошибки

Если файл не передан:

```json
{
  "error": "file_required",
  "message": "file field is required"
}
```

Если файл пустой:

```json
{
  "error": "empty_file",
  "message": "file is empty"
}
```

Если файл больше лимита из `MAX_FILE_SIZE_MB`:

```json
{
  "error": "file_too_large",
  "message": "file is too large"
}
```

Если MinIO недоступен:

```json
{
  "error": "storage_unavailable",
  "message": "storage is unavailable"
}
```
