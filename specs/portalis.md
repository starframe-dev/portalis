# Спецификация проекта Portalis

## Обзор

**Portalis** — встроенный терминальный эмулятор для правой панели приложения Automata. Предоставляет полноценную терминальную сессию с поддержкой ANSI/VT-последовательностей, PTY, буфером обмена и интеграцией с Bubble Tea.

## Архитектура

### Компоненты

```
portalis/
├── ansi.go          # ANSI-парсер: CSI, OSC, UTF-8, цвета
├── clipboard.go     # Работа с буфером обмена (macOS/Linux/Wayland)
├── emulator.go      # Terminal emulator (координирует Screen + Parser + PTY)
├── pty.go           # PTY обёртка (spawn, write, read, resize)
└── screen.go        # Экран: 2D сетка, рендеринг, scrollback, выделение
```

### Диаграмма компонентов

```
┌─────────────────────────────────────────────────────────────────┐
│                        Emulator (Главный контроллер)              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │   Parser     │→ │    Screen    │←│      PTY              │  │
│  │ (ANSI parser)│  │ (rendering)  │  │ (PTY process I/O)    │  │
│  └──────────────┘  └──────────────┘  └──────────────────────┘  │
│         ↑                  ↑                  ↑                  │
│         │                  │                  │                  │
│   ANSI sequences      User input          PTY output            │
│   (CSI, OSC)          (keyboard/mouse)    (stdout/stderr)       │
└─────────────────────────────────────────────────────────────────┘
```

### Emulator — встраиваемый компонент без привязки к host model

```go
type Emulator struct {
    SessionID string          // Идентификатор сессии
    ChatName  string          // Имя чата/сессии
    cmd       string          // Команда для запуска (bash/sh)
    args      []string        // Аргументы команды
    screen    *Screen         // Экран терминала
    parser    *Parser         // Парсер вывода
    pty       *Pty            // PTY процесс
    focused   bool            // Сфокусирован или нет
    width     int             // Ширина панели (от warp)
    height    int             // Высота панели (от warp)
    stopped   bool            // Завершена ли сессия
    cwd       string          // Последняя рабочая директория
    commandHistory []string      // История команд (max 1000)
    initialCWD string          // Изначальная рабочая директория
    scrollbackLimit int         // Лимит строк (по умолчанию 10000)
    scrollbackCells int         // Дополнительный cap — 1048576 ячеек
    pressX, pressY int         // Позиция нажатия мыши
    dragSelecting bool        // В процессе ли drag-выбора
    mu        sync.RWMutex   // Мьютекс для синхронизации
}
```

## Жизненный цикл сессии

### Запуск

```
1. Пользователь выбирает чат/терминал в дереве
2. SessionManager.openItem создаёт Emulator
3. App.Update(ItemSelectedMsg) вызывает em.Start()
4. em.Start() спавнит PTY процесс
5. Возвращается PtyReadyMsg
```

### Обновление

```
1. Emulator.Update(msg) обрабатывает:
   - tea.KeyMsg → запись в PTY, парсинг ANSI
   - tea.MouseMsg → SGR мышь, скроллинг, drag-выбор
   - tea.WindowSizeMsg → игнорируется (resize через ResizeMsg)
   - ResizeMsg → обновление размеров экрана и PTY
   - PtyOutputMsg → парсинг вывода через Parser
   - PtyExitMsg → завершение сессии
   - CursorBlinkMsg → переключение мигания курсора
```

### Завершение

```
1. При EOF от PTY приходит PtyExitMsg
2. SessionManager удаляет сессию
3. Emulator.Stop() закрывает PTY
4. Panel показывает ASCII-арт вместо терминала
```

## Данные потока

### Входные данные

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Keyboard Event  │────▶│     Emulator    │────▶│      Parser     │
│  (KeyMsg/Mouse)  │     │  (Event Loop)   │     │   (ANSI States) │
└─────────────────┘     └────────┬─────────┘     └────────┬─────────┘
                                 │                         │
                                 ▼                         ▼
                         ┌─────────────────┐     ┌─────────────────┐
                         │   Screen       │     │   Screen.Put()  │
                         │  (Cell Grid)   │◀────│  (Update Buffer) │
                         └─────────────────┘     └─────────────────┘
                                 ▲                         ▲
                                 │                         │
                                 └─────────────────────────┘
                                         Render String
```

### Выводные данные

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│      PTY        │────▶│    Screen       │────▶│    Render()     │
│  (Read Loop)    │     │  (Cell Grid)    │     │  (String)       │
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

### ANSI Парсинг

```
Input Data → Parser.Feed() → CSI/OSC/SGR → Screen.Put() → Render
```

## Публичный API

### Emulator

| Метод | Описание |
|-------|----------|
| `NewEmulator(sessionID, chatName, cmd, args)` | Создание эмулятора |
| `Start()` / `StartWithEnv()` | Запуск PTY |
| `StartSync(extraEnv)` | Синхронный запуск |
| `View(width, height)` | Рендеринг экрана |
| `Update(msg)` | Обработка событий |
| `Focus()` / `Blur()` | Управление фокусом |
| `Close()` | Закрытие PTY |
| `Stop()` | Остановка сессии |
| `SetScrollbackLimit(limit)` | Настройка лимита скролла |
| `SetInitialCWD(dir)` | Установить начальную директорию |
| `SetCommandHistory(history)` | Восстановить историю команд |

### Callbacks и snapshots

Callbacks устанавливаются через `SetOnCWDChange`, `SetOnTitleChange`, `SetOnCommandHistoryChanged`, `SetOnError` и `SetOnExit`. Они вызываются после снятия `Emulator.mu` и могут повторно вызывать методы эмулятора. OSC 7 передаёт `WorkingDirectory{Host, Path, Local}`; OSC 0/2 устанавливает terminal title.

Идентификаторы читаются через `SessionID()`/`ChatName()`, состояние PTY — через `PtyState()`. `CWD()` возвращает только путь; для remote/local authority используйте `CurrentWorkingDirectory()`. `CommandHistorySnapshot()` возвращает копию списка. История команды эвристически извлекается с видимой строки перед prompt и не является shell history; список ограничен 1000 записями.

Аргументы конструктора, `StartEnv()` и восстановленная история копируются, чтобы вызывающий код не мог изменять внутренние срезы.

### Screen

| Метод | Описание |
|-------|----------|
| `NewScreen(rows, cols)` | Создание экрана |
| `Resize(rows, cols)` | Совместимый с прежней сигнатурой resize; недопустимые размеры игнорируются без мутации |
| `ResizeChecked(rows, cols)` | Изменение размера с возвращением ошибки валидации |
| `Put(r rune)` | Запись символа |
| `SetCursor(row, col)` | Установка курсора |
| `ScrollUp()` / `ScrollViewUp(n)` | Скроллинг вверх |
| `Clear()`, `ClearLine()`, `ClearLineLeft()` / `ClearLineAll()` | Очистка |
| `EnterAltScreen()` / `ExitAltScreen()` | Альтернативный экран |
| `StartSelection(row, col)` / `ExtendSelection(row, col)` / `ClearSelection()` | Выделение |
| `SelectionText()` | Получение выделенного текста; при превышении 100 MiB или 4 Mi ячеек возвращает `nil` |
| `Render()` | Рендеринг экрана |

### Parser

| Метод | Описание |
|-------|----------|
| `NewParser(screen)` | Создание парсера |
| `Feed(data []byte)` | Парсинг ANSI данных |
| `SetCWDCallback(fn)` | Callback для изменений рабочей директории |

### PTY

| Метод | Описание |
|-------|----------|
| `Spawn(command, args, env...)` | Запуск PTY |
| `SpawnInDir(command, args, dir, env...)` | Запуск в директории |
| `Write(data)` | Отправка данных в PTY |
| `Resize(rows, cols)` | Изменение размера |
| `Close()` | Закрытие PTY |
| `Listen(sessionID)` | Слушатель событий |
| `WriteForGeneration(generation, data)` | Отклонение записи от устаревшего PTY поколения |

### Clipboard

| Функция | Описание |
|---------|----------|
| `copyToClipboard(lines)` | Копирование в буфер обмена |
| `pasteFromClipboard()` | Вставка из буфера обмена |
| `(*Emulator).PasteFromClipboard()` | Асинхронная интеграция вставки с PTY и текущим paste mode |

## Дизайн-решения

### 1. Совместимость с tmux и xterm

- Поддержка наборов символов DEC Special Graphics (`ESC(B`, `ESC(0`) и управляющих ESC-последовательностей (`ESC 7`, `ESC 8`, `ESC D`, `ESC E`, `ESC M`).
- Редактирование строк: `ICH` (`CSI @`), `DCH` (`CSI P`), `ECH` (`CSI X`), `IL` (`CSI L`), `DL` (`CSI M`).
- Прокрутка внутри области: `SU` (`CSI S`) / `SD` (`CSI T`) и `ReverseIndex`.
- Режимы: application cursor (`?1`), видимость курсора (`?25`), bracketed paste (`?2004`), synchronized output (`?2026`), alternate screen (`?1049`).
- VPA (`CSI d`) и HPA (`CSI G`).

### 2. Модель событий (Bubble Tea)

Emulator использует Bubble Tea для обработки событий:
- Event loop через `Update(msg tea.Msg) tea.Cmd`
- Асинхронные операции через `tea.Cmd`
- Сообщения через `tea.Msg`

### 2. Состояние и синхронизация

- `sync.RWMutex` защищает доступ к мутабельному состоянию
- Generation check и `Parser.Feed` выполняются под одним `Emulator.mu`, чтобы stale PTY chunk не мог попасть в новый terminal state.
- Parser callbacks только queue'ят CWD/terminal-response state; внешние callbacks и PTY writes выполняются после unlock.

### 3. Буфер прокрутки

- Scrollback ограничен `scrollbackLimit` (по умолчанию 10000 строк) и общим cap в 1048576 ячеек.
- При достижении любого cap вытесняются самые старые строки; cap ячеек действует и при `SetScrollbackLimit(0)`.
- `viewOffset` хранит положение просмотра и clamp-ится при изменении/вытеснении буфера.

### 4. Bracketed Paste

- Текст вставляется через `ESC[200~...ESC[201~`, только если дочернее приложение включило режим `?2004`.
- Вставка через Bubble Tea `KeyMsg.Paste` передаётся как plain text без режима или обёрнутой в bracketed, в зависимости от состояния экрана.
- Изображения передаются через путь к файлу.
- Поддержка macOS (Swift), Wayland, X11.

### 5. OSC 7 для рабочей директории

- PTY конфигурируется с `PROMPT_COMMAND` для эмитирования OSC 7; путь percent-encode'ится перед помещением в control sequence
- Callback вызывается при изменении cwd
- Поддержка форматов `file://hostname/path` и `/absolute/path`

### 6. Synchronized Output

- `ESC[?2026h` (вход) / `ESC[?2026l` (выход)
- Во время sync промежуточные изменения не отображаются
- `Render()` возвращает последний закоммиченный кадр
- Применяется внутри `Screen.SetSync`, а не вручную

### 7. Управление курсором

- `ESC[?1h` / `ESC[?1l` — application cursor keys
- `ESC[?25h` / `ESC[?25l` — видимость курсора
- Сохранение/восстановление позиции без атрибутов (`ESC 7` / `ESC 8`)
- Синхронизированное мигание курсора через `CursorBlinkMsg`

### 8. Передача клавиш в PTY

- Все C0 control keys (Ctrl+A…Ctrl+_, Ctrl+Space, Backspace) передаются как соответствующие байты 0x00–0x1F/0x7F.
- Модификатор `Alt` добавляет префикс `ESC` для rune/backspace/control; для стрелок, Home/End, PageUp/PageDown, F1–F20 используется xterm-параметр modifier (1;2/3/5/7/8).
- При application cursor mode базовые стрелки и Home/End кодируются через SS3 (`ESC O ...`).
- `Ctrl+V` не перехватывается: передаётся в PTY как 0x16, а clipboard-вставка идёт через `KeyMsg.Paste`.

### 9. Размер PTY и производительность

- `Pty.Resize` проверяет размеры до `TIOCSWINSZ`, пропускает повторный идентичный размер и обновляет сохранённую геометрию только после успешного ioctl; дополнительный сигнал `SIGWINCH` не отправляется.
- `Pty.Listen` выдаёт упорядоченные read chunks до 4 KiB и гарантированно дренирует уже прочитанный вывод перед `PtyExitMsg`.
- `Screen` использует dirty cache: неизменившийся кадр возвращает предыдущий render, инвалидация происходит при любом изменении ячеек/курсора/выделения/режимов.

## Ограничения

1. **Scrollback** — по умолчанию не более 10000 строк и всегда не более 1048576 ячеек
2. **Command history** — макс 1000 команд
3. **Cursor** — всегда в пределах границ экрана
4. **Selection** — работает только когда `selectionActive = true`
5. **Hex цвета** — поддерживается только формат `#RRGGBB`

## Безопасность

1. Ошибки запуска, записи и завершения PTY передаются вызывающему коду; bounded raw trace errors идут как non-fatal `PtyWarningMsg`/`SetOnError`.
2. PTY проверяется на закрытие перед операциями; дочерний процесс завершается при закрытии.
3. Clipboard temp-файлы создаются с правами `0600` в private per-Emulator directory `0700`, ограниченной количеством/байтами/TTL; удаляются при `Stop`/`Close` либо при ошибке/устаревшей сессии.
4. Clipboard subprocess ограничен 5 секундами и выводом 100 MiB; PNG до decode ограничен 25 000 000 пикселями.
5. Сканирование clipboard selection ограничено 4 Mi ячейками и выводом 100 MiB.

## Зависимости

- `github.com/charmbracelet/bubbletea` — TUI фреймворк
- `github.com/charmbracelet/lipgloss` — стилизация текста
- `github.com/creack/pty` — PTY поддержка

## Примечания

- PTY поддерживается на Unix-подобных системах Linux и macOS; Windows не поддерживается.
- Clipboard image-файлы отслеживаются Emulator и удаляются при `Stop`/`Close`; Wayland читает только объявленные MIME types.
- Swift используется для чтения изображений из clipboard macOS.
- PTY получает `TERM=ansi` по умолчанию, если вызывающий код не переопределил значение. Полный ANSI/VT и printer controls `mc4/mc5` не обещаются.
- PTY output читается через `bufio.Reader` небольшими упорядоченными chunks.

## Связанные спецификации

- `code-specs/ansi.md` — детальная спецификация Parser
- `code-specs/screen.md` — детальная спецификация Screen
- `code-specs/pty.md` — детальная спецификация PTY
- `code-specs/emulator.md` — детальная спецификация Emulator
- `code-specs/clipboard.md` — детальная спецификация Clipboard


## Инварианты hardening

- \`Pty.Close()\` идемпотентен и закрывает PTY fd, даже если другая goroutine заблокирована в \`Write\`.
- EOF/ошибка публикуются только после всех уже прочитанных PTY chunks.
- Alternate screen не пишет в основной scrollback; оба screen buffer согласованно меняют размер.
- Размер grapheme cluster ограничен, чтобы hostile terminal output не создавал неограниченные строки.
- Mouse events передаются child process только после DECSET 1000/1002/1003; режим 1006 выбирает SGR encoding. Shift оставляет событие локальной обработке.
- Публичные размеры Screen нормализуются минимум к 1×1; scrollback дополнительно ограничен 1048576 ячейками.


## Совместимость VT/xterm

- DSR/CPR и DA обрабатываются Parser, а не поиском по raw PTY chunk; поэтому escape sequences могут пересекать границы чтения.
- Поддерживаются DECOM \`?6\`, DECAWM \`?7\`, focus tracking \`?1004\`, mouse 1000/1002/1003/1006, REP, BCE и игнорируемые DCS/SOS/PM/APC strings.
- Bracketed-paste delimiters относятся к input protocol и не переводят output Parser в отдельное paste-состояние.
- Alternate screen отдельно сохраняет cursor и scroll region; DECSC/DECRC сохраняют rendition и charset state.
- PTY spawn выполняется вне Emulator mutex; lifecycle generation отменяет queued и выполняющийся start.
- Clipboard subprocess ограничен по времени и объёму; paste проверяет generation, а temp-файлы очищаются.
