# Specification for pty.go

## Назначение

Файл `pty.go` реализует обёртку над псевдотерминалом (PTY) для взаимодействия с дочерними процессами в терминальной среде. PTY-обёртка позволяет запустить команду в эмуляции терминала, читать её вывод, отправлять данные и управлять размером окна.

Этот компонент используется в терминальном таргете (Terminal) для создания интерактивных сессий с сохранением TTY-поведения.

## Публичный API

### `Spawn(command string, args []string, env ...string) (*Pty, error)`

Запускает новую PTY-сессию с указанной командой, аргументами и опциональными переменными окружения.

**Параметры:**
- `command` — имя запускаемой команды (например, `"bash"`)
- `args` — аргументы команды
- `env` — переменные окружения для процесса (опционально; explicit `TERM` заменяет default)

**Возвращает:**
- `*Pty` — объект PTY для взаимодействия с сессией
- `error` — ошибка при запуске

**Пример использования:**
```go
pty, err := Spawn("bash", []string{"-l"}, "PATH=/usr/bin")
if err != nil {
    // обработка ошибки
}
defer pty.Close()
```

Spawn задаёт `TERM=ansi`; явное значение из caller-provided `env` имеет приоритет. Это консервативная 8-color terminfo profile, не обещание полного ANSI/VT эмулятора. `mc4/mc5` printer controls и расширения вне документированного subset не поддерживаются.

### `SpawnInDir(command string, args []string, dir string, env ...string) (*Pty, error)`

Запускает новую PTY-сессию в указанной рабочей директории.

### `State() PtyState`

Возвращает snapshot состояния без process/I/O handles:

```go
type PtyState struct {
    Running bool
    PID     int
    Rows    int
    Cols    int
}
```

### `Write(data []byte) error`

Отправляет байты в PTY через одну упорядоченную очередь и одного writer worker. Admission order — порядок передачи данных в child; один payload полностью записывается до следующего. Очередь ограничена 64 запросами и 100 MiB суммарных bulk payload; один payload больше 100 MiB отвергается. Внутренний generation-aware метод не является публичным API.

Интерактивные клавиатурные и мышиные события используют отдельный reserve до 64 KiB и fail-fast admission: UI `Update` не ждёт PTY I/O, byte budget, queue lock или свободный slot. Перегрузка доставляется как нетерминальное `PtyErrorMsg`. Все accepted writes остаются в общем FIFO; paste payload с delimiters является одним атомарным запросом.

### `Resize(rows, cols int) error`

Изменяет размер окна PTY через `pty.Setsize` / `TIOCSWINSZ`.

**Поведение:**
- Отклоняет размеры вне общих terminal-size limits до ioctl
- Игнорирует вызов, если размер не изменился
- `TIOCSWINSZ` сам доставляет `SIGWINCH` foreground process group; дополнительный direct signal не отправляется
- `lastRows`/`lastCols` обновляются только после успешного `Setsize`

### `Close() error`

Закрывает PTY, отменяет enqueue, убивает дочерний процесс и дожидается завершения reader/writer goroutines. Операция идемпотентна.

### `Listen(sessionID string) tea.Cmd`

Возвращает команду Bubble Tea, которая ждёт одно событие: следующий output chunk, non-fatal warning или завершение PTY. Продолжайте выполнять команды, возвращаемые `Emulator.Update`, до завершения PTY; `Output`, `Errors` и raw handles не экспортируются.

**События:**
- `PtyOutputMsg` — один упорядоченный read chunk размером до 4 KiB
- `PtyWarningMsg` — non-fatal diagnostic, например сбой записи raw trace; после него listener продолжается
- `PtyExitMsg` — ошибка или нормальное завершение процесса

**Примечание:** все уже прочитанные chunks выдаются до `PtyExitMsg`; EOF не может обогнать накопленный вывод.

## Типы данных

### `Pty`

Структура для управления PTY-сессией.

Все process, I/O и lifecycle поля приватны. `State() PtyState` выдаёт snapshot (`Running`, `PID`, `Rows`, `Cols`); поток событий читается через `Listen`.

### `PtyWarningMsg`

Non-fatal PTY diagnostic (например trace I/O failure). `Emulator` вызывает `SetOnError` и продолжает listener.

### `PtyOutputMsg`

Сообщение о новых данных из PTY.

**Поля:**
- `SessionID string` — идентификатор сессии
- `Generation uint64` — поколение listener, назначается Emulator
- `Data []byte` — выходные данные

### `PtyExitMsg`

Сообщение об ошибке или выходе процесса.

**Поля:**
- `SessionID string` — идентификатор сессии
- `Generation uint64` — поколение listener
- `ProcessExited bool` — получен process exit status
- `ExitCode int` — код выхода; для завершения сигналом обычно `-1`
- `Signal os.Signal` — сигнал, завершивший процесс, если он был
- `Err error` — только ошибка PTY/инфраструктуры; ненулевой exit code сам по себе ошибкой не является

## Внутренняя логика

### `readLoop()`

Внутренняя функция, запускаемая в отдельной goroutine. Читаёт данные из PTY и отправляет их через канал `Output`.

**Логика:**
1. Читает данные буфером 4096 байт без попытки интерпретировать ANSI/VT.
2. Сохраняет raw trace, если включён диагностический режим.
3. Отправляет неизменённые bytes в канал `Output`.
4. Завершает `readDone` после последнего прочитанного chunk; `Listen` сначала
   дренирует buffered output и только затем публикует EOF/error.

Ответы терминала (DSR/DA) формируют Parser/Emulator, поэтому они корректны,
даже если escape-последовательность разделена между несколькими PTY reads.

### Диагностическая запись raw PTY

При заданном `PORTALIS_RAW_TRACE=<base>` записываются `<base>.<pid>` (исходные байты)
и `<base>.<pid>.chunks` (длина каждого read chunk). Файлы создаются с `0600`; существующий
base path не перезаписывается. Каждая дорожка ограничена `PORTALIS_RAW_TRACE_MAX_BYTES`
(по умолчанию 16 MiB) и `PORTALIS_RAW_TRACE_MAX_FILES` (по умолчанию 3); значения
ограничены сверху 1 GiB и 16 файлами. При достижении квоты выполняется ограниченная
ротация. Ошибки открытия, записи и ротации выдаются как `PtyWarningMsg` и передаются
через `SetOnError`; запись trace не останавливает PTY output. Trace может содержать
секреты и включается только для диагностики.

### Граница read chunk

Read loop читает PTY буфером 4096 байт и сохраняет эти границы. Это ограничивает длительность одного `Parser.Feed` и сохраняет отзывчивость UI под непрерывным выводом.


## Обработка ошибок

- Ошибки при запуске PTY возвращаются сразу через `Spawn`
- Ошибки при записи возвращаются через `Write`
- Ошибки при изменении размера возвращаются через `Resize`
- Ошибки при закрытии возвращаются через `Close`
- EOF и Linux `EIO` от PTY master при завершении slave считаются нормальным завершением; process exit status возвращается в `PtyExitMsg`
- Ошибки read loop возвращаются через `Listen`; trace I/O failures доставляются отдельными `PtyWarningMsg` и не завершают процесс

## Совместимость `PtyExitMsg`

`PtyExitMsg` содержит `SessionID`, `Generation`, `Err`, `ProcessExited`, `ExitCode` и `Signal`. Именованные literals предпочтительны; позиционные литералы внешнего кода ломаются при расширении структуры.

## Взаимодействие с Bubble Tea

Пакет использует `github.com/charmbracelet/bubbletea` для интеграции с моделью Event Loop:

- `SendBytes(p *Pty, data []byte) tea.Cmd` — отправляет байты в PTY
- `Listen(sessionID string) tea.Cmd` — слушает события PTY

## Безопасность

- Ошибки запуска, записи, resize, close и read-loop передаются вызывающему коду или через `Listen`; trace failures доступны через warning path и не прерывают PTY.
- Проверка на закрытый PTY перед операциями
- Процесс убивается при закрытии PTY
- Используется `pty.Start` для безопасного запуска в PTY
- Переменные окружения явно передаются; вызывающий код отвечает за их значения

## Ограничения

- PTY имеет буферизированный выход (канал на 64 chunks)
- Ошибки обрабатываются асинхронно через каналы
- На один Emulator допускается только один ожидающий listener; порядок chunks сохраняется

## Пример полного использования

```go
func RunCommand(command string, args []string) (string, error) {
    pty, err := Spawn(command, args)
    if err != nil {
        return "", err
    }
    defer pty.Close()

    output := make([]byte, 0)

    // Слушаем вывод
    ptyOutput := pty.Listen("session1")
    tea.Batch(ptyOutput, func() tea.Msg {
        // ...
        return nil
    })

    return string(output), nil
}
```

## Примечания

- PTY использует библиотеку `github.com/creack/pty` для создания псевдотерминала
- Child processes получают `TERM=ansi` по умолчанию; caller может явно переопределить его, принимая ответственность за terminfo compatibility.
- Вывод обрабатывается через `bufio.Reader` для корректного разбора строк
- Escape-последовательности терминала обрабатываются для поддержки ресайза окна


## Инвариант упорядоченной записи

Все источники ввода (клавиатура, мышь, DSR/DA replies, focus, paste и `SendBytes`)
передают данные в одну ограниченную FIFO-очередь. Один writer worker выполняет
системные записи последовательно; отдельная goroutine на каждый write не создаётся.
Bulk очередь ограничивает 64 запроса и 100 MiB; интерактивный ввод имеет отдельный
64 KiB reserve и не ждёт writer I/O или заполненного queue slot из UI update.
`Close` отменяет enqueue, закрывает PTY fd, завершает worker и ждёт reader loop; операция
идемпотентна через `sync.Once`, а generation mismatch отбрасывает устаревшие записи.
