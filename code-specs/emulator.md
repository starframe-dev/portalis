# Emulator

## Назначение

`Emulator` — встроенный терминальный эмулятор, который запускает оболочку/command в PTY (псевдо-терминал) и отображает вывод. Работает независимо от UI-фреймворка; хосты подают ему сообщения о клавиатуре/мышь/размере и вызывают `View(width, height)` для рендеринга.

Использует библиотеку `bubbletea` для управления TUI-приложением.

## Публичный API

### Конструктор

```go
func NewEmulator(sessionID, chatName, command string, args []string) *Emulator
```

Создаёт новый терминальный эмулятор для указанной сессии. Если `command` пуст, пытается найти оболочку (bash, sh).

| Параметр | Тип | Описание |
|----------|-----|----------|
| sessionID | string | Идентификатор сессии |
| chatName | string | Имя чата |
| command | string | Команда для запуска (оболочка) |
| args | []string | Аргументы команды |

### Старт

```go
func (e *Emulator) Start() tea.Cmd
```

Начинает запуск процесса PTY. Возвращает `tea.Cmd`, который возвращит `PtyReadyMsg` по готовности.

```go
func (e *Emulator) StartSync(extraEnv []string) error
```

Запускает PTY синхронно с дополнительными переменными окружения. Не возвращает `tea.Cmd` — PTY готов сразу. Возвращает ошибку при неудаче spawn или initial resize; при ошибке resize уже запущенный PTY остаётся прикреплённым.

```go
func (e *Emulator) StartWithEnv(extraEnv []string) tea.Cmd
```

Запуск PTY с переменными окружения. Возвращает `tea.Cmd`, завершающий работу и возвращающий `PtyReadyMsg` или `PtyExitMsg`. Ошибка начального resize не завершает сессию: она передаётся в `PtyReadyMsg.ResizeErr`.

```go
func (e *Emulator) SetScrollbackLimit(limit int)
```

Устанавливает максимальное число строк scrollback (по умолчанию 10000). Значения ≤ 0 отключают лимит строк, но ограничение 1048576 ячеек продолжает действовать. Вызывать до `Start()` необязательно: после запуска лимит обновляет экран сразу.

```go
func (e *Emulator) SetInitialCWD(dir string)
```

Устанавливает каталог, в котором начнётся PTY-процесс.

```go
func (e *Emulator) SetCommandHistory(history []string)
```

Восстанавливает историю команд; сохраняются не более 1000 последних записей. Входной слайс копируется.

```go
func (e *Emulator) Close() error
```

Закрывает PTY, отменяет ожидающие записи и удаляет отслеживаемые clipboard temp-файлы; возвращает первую ошибку очистки.

```go
func (e *Emulator) Stop() error
```

Завершает сессию, удаляет отслеживаемые clipboard temp-файлы и переключает панель на вид с ASCII-артом; возвращает первую ошибку очистки.

```go
func (e *Emulator) Focus()
```

Отмечает эмулятор как сфокусированный.

```go
func (e *Emulator) Blur()
```

Отмечает эмулятор как нефокусированный.

```go
func (e *Emulator) View(width, height int) string
```

Рендерит терминальный экран при заданном размере панели. Размер экрана синхронизируется через `ResizeMsg`.

```go
func (e *Emulator) Update(msg tea.Msg) tea.Cmd
```

Обрабатывает сообщения.

### Callbacks и snapshots

Callbacks регистрируются setter-ами и вызываются вне `Emulator.mu`, поэтому обработчик может повторно вызывать методы эмулятора:

```go
e.SetOnCWDChange(func(WorkingDirectory) {})
e.SetOnTitleChange(func(string) {})
e.SetOnCommandHistoryChanged(func([]string) {})
e.SetOnError(func(error) {})
e.SetOnExit(func(PtyExitMsg) {})
```

`SetOnCWDChange` передаёт `WorkingDirectory{Host, Path, Local}`. Для текущего места доступны `CurrentWorkingDirectory() (WorkingDirectory, bool)` и convenience getter `CWD() string`; заголовок OSC 0/2 читается через `Title()`. Пустой OSC 0/2 очищает title и передаётся в `SetOnTitleChange`, когда значение действительно меняется.

`SetOnExit` вызывается ровно один раз, когда `Update` принимает событие фактического завершения дочернего процесса (`PtyExitMsg.ProcessExited`). Для доставки callback host должен поддерживать цепочку `Listen()` и передавать полученные сообщения в `Update`. Намеренные `Stop()` и `Close()` не вызывают callback; их устаревшие exit-сообщения игнорируются.

Сырой PTY accessor отсутствует. `PtyState()` возвращает snapshot (`Running`, `PID`, `Rows`, `Cols`), не раскрывая process и I/O handles. Историю можно получить копией через `CommandHistorySnapshot()`; новые записи эвристически извлекаются из видимой строки перед prompt и не являются shell history.

## Типы сообщений

### ResizeMsg

```go
type ResizeMsg struct {
    Width  int
    Height int
}
```

Подаётся хостом, когда выделенная прямоугольная область эмулятора изменилась. Содержит размер контента в ячейках (без границ или отступов).

### CursorBlinkMsg

```go
type CursorBlinkMsg struct{}
```

Подаётся хостом для переключения состояния мигания курсора. Один таймер хоста должен транслировать это сообщение всем видимым терминальным эмуляторам для синхронизации мигания курсоров.

### PtyReadyMsg

```go
type PtyReadyMsg struct {
    SessionID      string
    Generation     uint64
    AlreadyRunning bool
    ResizeErr      error
}
```

Подаётся, когда PTY готов к прослушиванию. `ResizeErr` сообщает non-fatal ошибку применения начального размера; `Update` передаёт её в `OnError` и запускает listener.

### PtyExitMsg

```go
type PtyExitMsg struct {
    SessionID     string
    Generation    uint64
    ProcessExited bool
    ExitCode      int
    Signal        os.Signal
    Err           error
}
```

Подаётся при завершении PTY, в том числе при нормальном выходе. `Err` содержит только инфраструктурную ошибку; статус процесса доступен в отдельных полях.

### PtyWarningMsg

```go
type PtyWarningMsg struct {
    SessionID  string
    Generation uint64
    Err        error
}
```

Сообщает о non-fatal PTY предупреждении (например, ошибке bounded raw trace). После callback `OnError` listener продолжается.

## Структура данных Emulator

```go
type Emulator struct {
    // Все поля состояния и callback storage закрыты.
    // Чтение выполняется через snapshot/getter-методы, изменение — через setter-ы.
}
```

## Вспомогательные функции

### DefaultShell / defaultShell

```go
func DefaultShell() (string, []string)
func defaultShell() (string, []string)
```

Возвращают рабочую оболочку (bash, sh). `defaultShell` — устаревший внутренний алиас для обратной совместимости.

### stripPrompt

```go
func stripPrompt(line string) string
```

Удаляет префикс shell prompt с терминальной строки. Ищет последнее появление общих маркеров завершения prompt: `$ `, `# `, `> `, `% `.

### renderAsciiArt

```go
func renderAsciiArt(width, height int) string
```

Возвращает ASCII-арт иконку, показываемую, когда сессия завершена. Центрирует арт в заданном размере.

### emptyView

```go
func emptyView(width, height int) string
```

Возвращает пустой экран (пробелы) для эмулятора, у которого ещё нет PTY.

### keyToBytes / keyToBytesWithModes

```go
func keyToBytes(msg tea.KeyMsg) []byte
func keyToBytesWithModes(msg tea.KeyMsg, modes keyEncodingModes) []byte
```

Конвертирует `tea.KeyMsg` в последовательность байтов, отправляемую PTY. Учитывает режимы экрана (`applicationCursor`, `bracketedPaste`).

**Поддерживаемые группы:**
- C0 control keys: `Ctrl+A`…`Ctrl+_`, `Ctrl+Space`, `Backspace` — передаются как соответствующие байты 0x00–0x1F/0x7F
- Rune keys и paste — plain bytes, с `ESC` префиксом при `Alt`
- Bracketed paste — если `KeyMsg.Paste` и режим `?2004` включён, оборачивается в `ESC[200~...ESC[201~`
- Стрелки, Home/End, PageUp/PageDown — xterm CSI/SS3 с modifier-параметром (1;2/3/5/7/8); SS3 используется при application cursor mode
- F1–F20 — xterm/urxvt sequences
- `Shift+Tab` — `ESC[Z`
- `Ctrl+V` — передаётся как 0x16; clipboard-вставка обрабатывается через `KeyMsg.Paste`

### mouseToBytes

```go
func mouseToBytes(msg tea.MouseMsg) []byte
```

Кодирует событие мыши bubbletea в SGR-последовательность мыши, чтобы TUI-приложения внутри PTY получали отдельные события press/release/wheel.

## Правила

- Терминальный рендеринг должен сохранять keyboard-first взаимодействие.
- Поддержка мыши разрешена, но она не должна быть единственной моделью взаимодействия без явного решения.
- Запуск дочерних процессов должен иметь явные границы и обработку ошибок.

## Внутреннее поведение

### Старт

1. Создаётся `Screen` безопасного размера 80×24; `SetScrollbackLimit` меняет только scrollback, а размер экрана задаётся сообщением ресайза.
2. Создаётся `Parser` с callback'ом на смену рабочей директории.
3. Запускается PTY через `spawnPty(extraEnv)`.
4. Если ширина/высота заданы — ресайз экрана и PTY.

### Обработка клавиш

- `Ctrl+V` передаётся в PTY как 0x16; clipboard-вставка идёт через `KeyMsg.Paste`.
- Любое нажатие возвращает экран в live режим.
- При `Enter` извлекается команда из строки, сохраняется в историю (max 1000).
- Байты пишутся синхронно в PTY для сохранения порядка нажатий.
- Модификаторы `Alt`/`Ctrl`/`Shift` и F-клавиши кодируются в xterm-совместимые последовательности.
- При application cursor mode (`DECSET ?1`) базовые стрелки и Home/End кодируются через SS3 (`ESC O ...`).

### Обработка мыши

- Колесо локально прокручивает scrollback на три строки, когда child не захватил mouse mode или удерживается Shift.
- Левая кнопка: нажатие запоминает позицию, движение начинает drag-выбор, отпускание копирует выделенный текст.
- События передаются PTY после DECSET mouse mode (`?1000`, `?1002` или `?1003`). `?1006` включает SGR-кодирование, иначе используется X10. Shift принудительно оставляет событие локальной обработке.

### Обработка ресайза

- `WindowSizeMsg` содержит размер всего окна и намеренно игнорируется; host wrapper преобразует его в `ResizeMsg` с размером области эмулятора.
- `ResizeMsg` задаёт запрошенный размер отдельно от последнего успешно применённого.
- Без PTY размер экрана применяется сразу; с активным PTY сначала выполняется `TIOCSWINSZ`, и только при успехе меняются Screen/applied dimensions. Ошибка возвращается как non-fatal `PtyWarningMsg`, PTY и предыдущий размер остаются в силе.
- Если во время ioctl поступило новое поколение resize, применяется самый свежий запрос; ошибка устаревшего поколения не отменяет новый запрос. `TIOCSWINSZ` сам посылает `SIGWINCH` foreground process group.

### Скроллинг

- Колесо прокручивает scrollback на три строки, если child не захватил mouse mode или удерживается Shift.
- В режиме mouse reporting колесо передаётся дочернему процессу.
- Scrollback ограничен 10000 строками и 1048576 ячейками.


## Lifecycle invariants

- \`Start()\`/\`StartWithEnv()\` получают lifecycle generation в момент создания \`tea.Cmd\`; \`Stop()\` и \`Close()\` инвалидируют ещё не выполненные start-команды.
- \`Close()\`/\`Stop()\` сначала отсоединяют PTY под mutex, затем закрывают процесс уже без mutex.
- Новый terminal reset очищает старый OSC 7 \`cwd\`; пока новый shell не сообщил путь, \`CWD()\` возвращает \`InitialCWD\`.
- \`PtyExitMsg\` и старые listener messages не могут завершить или модифицировать новую generation.


## Семантика lifecycle и ответов терминала

- PTY создаётся вне \`Emulator.mu\`; lifecycle token фиксируется до spawn, поэтому \`Stop\`/\`Close\` могут отменить незавершённый запуск.
- \`PtyReadyMsg\`, \`PtyOutputMsg\`, \`PtyWarningMsg\` и \`PtyExitMsg\` содержат точное generation; ноль не считается wildcard.
- DSR/DA ответы накапливаются при разборе и записываются в PTY после снятия \`Emulator.mu\`.
- \`OnError\` получает PTY/clipboard ошибки и non-fatal diagnostics; все внешние callbacks вызываются вне mutex и могут повторно вызывать API.
- \`Focus()\`/\`Blur()\` возвращают \`tea.Cmd\`; при включённом child режиме \`?1004\` отправляются \`CSI I\`/\`CSI O\`. \`Update\` принимает Bubble Tea \`FocusMsg\`/\`BlurMsg\`.
- \`Close()\`/\`Stop()\` возвращают первую ошибку очистки и удаляют отслеживаемые clipboard temp-файлы.
- Аргументы конструктора, environment и восстановленная история копируются; command capture эвристически считывается с видимой строки и не является shell history. Список ограничен 1000 entries.
- \`PtyExitMsg\` сообщает process exit code/signal; используйте именованные literals, поскольку позиционные literals несовместимы при расширении структуры.
