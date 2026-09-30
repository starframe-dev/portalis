# Миграция к pre-v1 API

Эти изменения намеренно закрепляют безопасные инварианты до v1. Публичные mutable-поля были заменены методами чтения и setter-ами; проект пока не обещает стабильность API.

## Screen

Прямой доступ к `Rows`, `Cols`, `Cells` и `Cursor` больше недоступен. Используйте snapshots:

```go
rows, cols := screen.Rows(), screen.Cols()
cursor := screen.CursorState()
cell, ok := screen.CellAt(row, col)
cells := screen.CellsSnapshot() // глубокая копия, изменение не меняет Screen
```

Изменения состояния выполняйте методами `Screen` (`SetCursor`, `ResizeChecked`, `Put` и другими), а не через возвращённые копии.

## Pty и Emulator

- `Pty.Output`, `Pty.Errors` и `Pty.WriteForGeneration` больше не являются публичным API. `WriteForGeneration` был служебным lifecycle token, а прямое чтение каналов обходило упорядоченную доставку.
- `Emulator.Pty()` удалён: сырой PTY раскрывал process/I/O handles. Для диагностики используйте snapshot `Emulator.PtyState()` (`Running`, `PID`, `Rows`, `Cols`).
- Идентификаторы теперь читаются через `SessionID()` и `ChatName()`.
- Callback-поля заменены setter-ами: `SetOnCWDChange`, `SetOnTitleChange`, `SetOnCommandHistoryChanged`, `SetOnError`, `SetOnExit`.
- `PtyReadyMsg` содержит `Generation` и `ResizeErr`; `PtyExitMsg` расширен полями статуса процесса. Используйте именованные поля при создании литералов, поскольку внешние позиционные литералы ломаются при изменении состава полей.
- При ошибке `TIOCSWINSZ` PTY остаётся живым, а Screen сохраняет последний успешно применённый размер; `OnError` получает ошибку, начальная ошибка дополнительно доступна в `PtyReadyMsg.ResizeErr`.

Пример:

```go
emulator.SetOnError(func(err error) { log.Printf("terminal: %v", err) })
emulator.SetOnExit(func(msg portalis.PtyExitMsg) {
    log.Printf("process exited=%v code=%d signal=%v", msg.ProcessExited, msg.ExitCode, msg.Signal)
})
state := emulator.PtyState()
```

## OSC 7 и заголовок окна

`SetOnCWDChange` теперь получает `portalis.WorkingDirectory`, а не строку:

```go
emulator.SetOnCWDChange(func(cwd portalis.WorkingDirectory) {
    log.Printf("host=%s path=%s local=%v", cwd.Host, cwd.Path, cwd.Local)
})
```

Для remote `file://host/path` сохраняются host и path; `Local` не позволяет считать remote путь локальной директорией. `CWD()` оставлен как convenience getter только пути. Для полной информации используйте `CurrentWorkingDirectory() (WorkingDirectory, bool)`. Заголовок OSC 0/2 доступен через `Title()` и `SetOnTitleChange`.

## История команд

История извлекается эвристически из видимой строки перед prompt при Enter. Это **не** shell history: продолжения команд, нестандартные prompt-ы, скрытый ввод и команды вне видимой строки могут быть пропущены или распознаны неверно. Получение копии выполняется через `CommandHistorySnapshot()`, сохранённый список можно восстановить методом `SetCommandHistory`.

## TERM

По умолчанию дочернему процессу передаётся `TERM=ansi`, а не `xterm-256color`: последний объявлял неподдерживаемые xterm-возможности. `ansi` также не означает полный эмулятор всех ANSI-устройств; неподдерживаемые printer-control команды `mc4/mc5` и G2/G3 charset designators не обещаются. Если host располагает подходящей собственной terminfo записью, TERM можно переопределить:

```go
emulator.SetStartEnv([]string{"TERM=xterm-256color"})
```

Переопределение делает вызывающее приложение ответственным за соответствие terminfo реальным возможностям терминала.
