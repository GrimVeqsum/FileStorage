# Отчёт о доработке FileStorage

## 1. Найденные проблемы

- **CRITICAL:** Upload создавал S3 object до PostgreSQL metadata; компенсация зависела от отменённого запроса. Delete сначала удалял S3 object. Падение процесса оставляло потерянные объекты либо доступные записи без файла.
- **HIGH:** отсутствовали восстановление и обработка неоднозначных подтверждений; HTTP body не ограничивался строго; multipart расходовал память/диск; заголовки скачивания и имена обрабатывались небезопасно; первое чтение S3 происходило после HTTP 200; зависимости публиковались наружу с паролями в Compose; приложение могло стартовать до миграций.
- **MEDIUM:** фиксированные короткие таймауты, слабый shutdown, разделяемая переменная ошибки между goroutines, незаметные ошибки конфигурации, UUID/повторный Delete, root/amd64/latest build, отсутствие тестов/CI и эксплуатационной документации.

Полный аудит с приоритетами и первоначальным планом: [AUDIT.md](AUDIT.md).

## 2. Исправления

| Файлы | Результат |
| --- | --- |
| `internal/files/service.go`, `repository.go` | Durable intent, условные SQL transitions, отдельный контекст компенсации, идемпотентный Delete, фоновые retry/claims |
| `migrations/002_file_lifecycle.*.sql` | Состояния и индексы без удаления старых файлов; блокировка опасного rollback |
| `internal/files/handler.go` | Лимиты файла/body, временный spool с немедленным unlink на Unix, предел конкурентных uploads, multipart/413, безопасные имена и заголовки |
| `internal/storage/minio.go` | Single PUT, потоковый Content-MD5 для HTTP spool, раннее чтение/размер, совместимость legacy UUID.extension, корректная проверка bucket |
| `internal/config/config.go`, `cmd/app/main.go`, router | Проверка конфигурации без секретов, schema/bucket readiness, deadlines, запуск/остановка worker и graceful shutdown |
| `Dockerfile`, Compose, env/ignore files, `deploy/minio.Dockerfile` | Непривилегированный app runtime, приватные зависимости, сохранённые имена volumes, миграции перед app, зафиксированные версии, source build MinIO |
| Unit/integration tests, `scripts/test-integration.sh`, CI workflow | Fault injection, реальные PostgreSQL/MinIO, отдельный test project, проверки сборки и race detector |
| `README.md`, `docs/*` | Английский README, архитектура, запуск, API, восстановление, компромиссы и реально выполненные проверки |

## 3. Модель согласованности

`uploading → ready → deleting → deleted`. До PUT существует PostgreSQL intent; API показывает только `ready`. Атомарные условные SQL statements сериализуют смену состояния. При неизвестном результате активации компенсация не может удалить `ready` object.

Worker стартует вместе с процессом и работает каждые 15 секунд, забирая до 32 записей через `SKIP LOCKED`. Upload считается устаревшим после трёх минут; foreground operation ограничена двумя минутами; query/cleanup — пятью секундами. Tombstones сохраняются и повторно очищаются, чтобы удалить поздний PUT после потерянного подтверждения. Это eventual recovery без распределённой транзакции и без обещания exactly-once.

## 4. Сбои и восстановление

| Сценарий | Поведение |
| --- | --- |
| PostgreSQL intent не подтверждён | PUT не выполняется; возможный сохранённый intent восстановит worker |
| MinIO PUT неуспешен / подтверждение потеряно | Файл скрыт; отдельная ограниченная компенсация и повторная очистка |
| Падение между PUT и активацией | Устаревший `uploading` переходит в очистку |
| Активация реально прошла, ответ PostgreSQL потерян | Файл остаётся `ready`; компенсация его не удаляет; клиент проверяет список |
| PostgreSQL Delete intent не записан | S3 object не удаляется; клиент повторяет Delete |
| Delete intent записан, S3 недоступен / процесс упал | Запись скрыта; worker повторяет DELETE |
| S3 DELETE прошёл, финальный SQL update не подтверждён | Идемпотентный повтор завершает операцию |
| Поздний PUT после DELETE | Сохранённый tombstone обеспечивает следующую очистку |
| Повторный/конкурентный DELETE | 204 после успешного завершения, 503 при ошибке зависимости; дальнейший retry безопасен |
| Перезапуск | `ready` файлы сохраняются; промежуточные операции восстанавливаются |

## 5. Реально выполненные проверки

`gofmt`, `go vet`, vet интеграционных тестов, `go test`, `go test -race`, `go build`, обе Compose-конфигурации, ShellCheck, Docker builds и `git diff --check` прошли.

Финальный запуск интеграционных тестов: **21 тест + 29 subtests, 7.485s, с `-race`**, настоящие PostgreSQL и MinIO. Проверены точный 20 MiB файл, байты/заголовки, ошибки и неоднозначные исходы, отмена, конкурентные удаления/cleanup, миграция старых данных, late PUT, настоящий перезапуск и SIGTERM. Отдельный запуск основного Compose подтвердил migration-before-app, healthcheck, nonroot/read-only runtime и CRUD.

Команды, результаты и границы доказательств: [VERIFICATION.md](VERIFICATION.md). Sandbox-доступ к сети/локальным listener/Docker был разрешён; заблокированные попытки не засчитывались как успешные проверки.

## 6. Оставшиеся ограничения

Нет авторизации, quota, пагинации, resumable/range API и idempotency key для POST. Tombstones растут; throughput worker ограничен. Старые orphan objects не удаляются автоматически. Версионированные/locked buckets и внешние изменения объектов требуют отдельного решения. Default tmpfs рассчитан на 20 MiB × 8 uploads, не на максимальные 5 GiB. MinIO upstream архивирован; production deployment требует отдельной оценки сопровождения, TLS, секретов и прав доступа.

GitHub Actions не запускался удалённо. Физический обрыв подтверждения commit, питание/kill в каждой точке, blackholed TCP PostgreSQL, файлы >20 MiB, disk-full/load/soak, Windows/ARM и production TLS не проверены; точные окна отказа моделировались fault injection и сохранёнными состояниями. Детали перечислены в VERIFICATION.md.

## 7. Полный список изменённых/добавленных файлов

Всего: 27 файлов.

- `.dockerignore`
- `.env.example`
- `.github/workflows/ci.yml`
- `.gitignore`
- `Dockerfile`
- `README.md`
- `cmd/app/main.go`
- `deploy/minio.Dockerfile`
- `docker-compose.test.yml`
- `docker-compose.yml`
- `docs/AUDIT.md`
- `docs/FINAL_REPORT.md`
- `docs/VERIFICATION.md`
- `integration/integration_test.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/files/handler.go`
- `internal/files/handler_test.go`
- `internal/files/repository.go`
- `internal/files/service.go`
- `internal/files/service_test.go`
- `internal/httptransport/router.go`
- `internal/storage/minio.go`
- `internal/storage/minio_test.go`
- `migrations/002_file_lifecycle.down.sql`
- `migrations/002_file_lifecycle.up.sql`
- `scripts/test-integration.sh`

Commit, push, PR и изменение Git history не выполнялись. Пользовательские данные и volumes не удалялись.
