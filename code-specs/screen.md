# Specification: screen.go

## Назначение

`screen.go` реализует двумерную сетку терминального экрана (2D cell grid) с поддержкой:

- Отрисовки текста с атрибутами (цвет, стиль)
- Управления позицией курсора
- Обёртывания строк
- Скроллинга вверх (scrollback buffer)
- Выделения текста (selection)
- Синхронизированного вывода (synchronized output)
- Входов/выходов альтернативного экрана (ANSI SGR)

## Обзор типов

### `StyleBits`

```go
type StyleBits uint8
```

Флаговые типы стилей текста:

| Константа | Значение | Описание |
|-----------|----------|----------|
| `StyleBold` | 1 | Жирный шрифт |
| `StyleDim` | 2 | Тусклый шрифт |
| `StyleItalic` | 4 | Курсив |
| `StyleUnderline` | 8 | Подчёркивание |
| `StyleBlink` | 16 | Мигающий |
| `StyleReverse` | 32 | Инверсия (видео) |
| `StyleHidden` | 64 | Скрытый |
| `StyleStrikethrough` | 128 | Зачёркивание |

### `Cell`

```go
type Cell struct {
    Rune  rune      // Символ ячейки (0 = пробел)
    FG    lipgloss.Color // Цвет фона (foreground)
    BG    lipgloss.Color // Цвет текста (background)
    Style StyleBits   // Стилевые флаги
}
```

`Empty()` возвращает `true`, если ячейка пуста (Rune == 0 или пробел).

### `Cursor`

```go
type Cursor struct {
    Row int         // Строка (0-indexed)
    Col int         // Столбец (0-indexed)
    FG  lipgloss.Color
    BG  lipgloss.Color
    Style StyleBits
}
```

Курсор хранит позицию и атрибуты для отрисовки.

### `Screen`

```go
type Screen struct {
    Rows   int          // Высота экрана (количество строк)
    Cols   int          // Ширина экрана (количество столбцов)
    Cells  [][]Cell     // Двумерная сетка ячеек

    Cursor Cursor       // Текущая позиция курсора

    savedCursor Cursor        // Сохранённый курсор (ESC 7/8)
    savedCells   [][]Cell      // Содержимое при входе в альтернативный экран

    scrollTop, scrollBottom int   // Границы области прокрутки (DECSTBM, 0-indexed)
    scrollback      [][]Cell   // Выведенные за пределы области строки
    scrollbackCells int        // Число ячеек в scrollback
    scrollbackLimit int        // Лимит строк scrollback (по умолчанию 10000)
    viewOffset      int        // Смещение просмотра (сколько строк вверх от live-экрана)

    wrapPending bool  // true после записи в последнюю колонку; следующая буква оборачивает

    syncActive bool    // true во время синхронизированного вывода
    lastRender string   // Последний закоммиченный кадр
    renderDirty bool    // true если lastRender устарел

    applicationCursor bool // ?1h
    bracketedPaste    bool // ?2004h
    CursorVisible     bool // ?25h
    CursorBlinkVisible bool  // Курсор мигает (для эмулятора)

    selStartRow, selStartCol int  // Начало выделения
    selEndRow, selEndCol     int   // Конец выделения
    selectionActive          bool // Активно ли выделение
}
```

## Публичный API

### Создание и инициализация

#### `NewScreen(rows, cols int) *Screen`

Создаёт экран с заданными размерами. Неположительные параметры нормализуются до одной ячейки. Для размеров, превышающих лимиты терминала или бюджет площади сетки, конструктор использует безопасный размер 24×80. Лимиты: 65535 строк, 65535 колонок и не более 262144 ячеек в сетке. `ResizeChecked` отклоняет недопустимый размер до аллокации и оставляет экран без изменений; совместимый `Resize` также не мутирует экран при отказе. Scrollback по умолчанию ограничен 10000 строками и всегда дополнительно ограничен 1048576 ячейками; этот cap действует и при отключении лимита строк.

```go
s := NewScreen(24, 80)
if err := s.ResizeChecked(40, 120); err != nil {
    return err
}
```

#### `SetScrollbackLimit(limit int)`

Устанавливает максимальное количество строк в scrollback буфере.

- `0` или отрицательное значение — отключает лимит строк; safety cap в 1048576 ячеек продолжает действовать.
- При увеличении лимита старые строки не удаляются.
- При уменьшении лимита отбрасываются старые строки.

#### `Resize(rows, cols int)` и `ResizeChecked(rows, cols int) error`

`Resize` сохраняет прежнюю совместимую сигнатуру и игнорирует недопустимый размер, оставляя экран без изменений. `ResizeChecked` возвращает ошибку при недопустимых размерах и также ничего не меняет; используйте его, если вызывающему коду нужно обработать отказ.

### Управление курсором

#### `SetCursor(row, col int)`

Устанавливает позицию курсора.

- Индексы нормализуются: отрицательные → 0, ≥ Rows → Rows-1
- `wrapPending` сбрасывается

#### `CursorPos() (row, col int)`

Возвращает текущую позицию курсора.

#### `SaveCursor()`

Сохраняет текущую позицию курсора без атрибутов (FG/BG/Style сбрасываются).

#### `RestoreCursor()`

Восстанавливает сохранённую позицию курсора.

#### `Clear()`

Очищает весь экран. Сбрасывает `wrapPending`.

#### `ClearLine()`

Очищает строку курсора от курсора до конца строки.

#### `ClearLineLeft()`

Очищает строку курсора от начала до курсора (включительно).

#### `ClearLineAll()`

Очищает всю строку курсора.

### Запись текста

#### `Put(r rune)`

Записывает символ в позицию курсора и advances курсор.

**Поведение обёртывания:**

- Если запись в последнюю колонку → `wrapPending = true`
- Следующая запись автоматически переносит курсор на следующую строку
- При достижении верхней границы экрана строка поднимается в scrollback

#### `SetScrollRegion(top, bottom int)`

Устанавливает область прокрутки (DECSTBM).

- `top`, `bottom` — 1-indexed (как в ANSI)
- Нормализуется: `top < 1` → 1, `bottom > Rows` → Rows
- Если `bottom <= top` → `bottom = Rows`, `top = 1`
- `scrollTop`, `scrollBottom` конвертируются в 0-indexed

### Управление прокруткой

#### `ScrollUp()` / `scrollLineUp()`

Поднимает активную область на одну строку вверх. Если `scrollTop == 0`, верхняя строка сохраняется в scrollback.

#### `ScrollRegionUp(n int)` / `ScrollRegionDown(n int)`

Прокручивают только активную область (DECSTBM) на n строк.

#### `Index()` / `NextLine()` / `ReverseIndex()`

- `Index()` — двигает курсор вниз; на нижней границе области прокручивает вверх.
- `NextLine()` — колонка 0, затем `Index()`.
- `ReverseIndex()` — двигает курсор вверх; на верхней границе области прокручивает вниз.

#### `ScrollViewUp(n int)` / `ScrollViewDown(n int)` / `ResetView()`

Перемещают вид вверх/вниз по scrollback, возвращают к live-экрану.

#### `ViewOffset() int`

Возвращает текущее смещение просмотра.

### Редактирование содержимого

#### `InsertChars(n int)` / `DeleteChars(n int)` / `EraseChars(n int)`

Изменяют текущую строку: ICH сдвигает текст вправо и вставляет пробелы, DCH сдвигает влево, ECH стирает ячейки без сдвига.

#### `InsertLines(n int)` / `DeleteLines(n int)`

Вставляют/удаляют строки внутри активной области прокрутки.

### Альтернативный экран (ANSI SGR)

#### `EnterAltScreen()`

Входит в альтернативный экран:

- Сохраняет текущий курсор и содержимое экрана
- Очищает экран
- Устанавливает курсор в (0, 0)

#### `ExitAltScreen()`

Возвращается из альтернативного экрана:

- Восстанавливает сохранённый экран и курсор
- Сохраняет main-buffer margins как full-screen при изменении высоты; custom DECSTBM margins clamp-ятся в новой геометрии
- Parser сохраняет/восстанавливает G0/G1 charset state для `?1049`
- Сбрасывает `wrapPending`

### Выделение текста

#### `StartSelection(row, col int)`

Начинает выделение с указанной ячейки.

#### `ExtendSelection(row, col int)`

Расширяет выделение до новой позиции.

**Правила:**

- Индексы нормализуются к границам экрана
- При перетаскивании влево по одной строке ячейка под мышью исключается из выделения

#### `ClearSelection()`

Сбрасывает активное выделение.

#### `cellInSelection(row, col int) bool`

Проверяет, находится ли ячейка внутри выделения.

#### `SelectionText() []string`

Возвращает текст текущего выделения.

- Каждая строка — отдельный элемент слайса; завершающие пробелы обрезаются.
- При перетаскивании влево ячейка под мышью исключается.
- Если выделение затрагивает любую половину wide cell, `SelectionText` копирует grapheme целиком один раз.
- Сканирование ограничено 4 Mi ячейками и результат — 100 MiB; при превышении `SelectionText` возвращает `nil`.

### Рендеринг

#### `Render() string`

Возвращает отрисованный экран как строку.

**Логика:**

- Если `syncActive` без selection → возвращает `lastRender`
- Если `syncActive` с selection → накладывает selection на копию последнего committed frame; не показывает uncommitted cells и не меняет `lastRender`/`renderDirty`
- Если экран не менялся с последнего рендера (`!renderDirty`) → возвращает кэшированный `lastRender`
- Иначе обновляет кэш и возвращает текущий кадр
- Hidden glyph под cursor/selection заменяется пробелами той же display width; cursor/selection на wide continuation покрывает glyph целиком
- При `viewOffset > 0` строки берутся из scrollback; иначе используется live-экран

#### `SetSync(active bool)`

Включает/выключает синхронизированный вывод. Вход фиксирует последний реально отрисованный кадр; выход помечает экран dirty, чтобы следующий `Render()` один раз опубликовал новое состояние.

### Утилитарные функции

#### `markDirty()`

Помечает экран изменённым; при необходимости сохраняет committed frame до следующей мутации.

## Реализация стилей

### `renderStyle(fg, bg lipgloss.Color, style StyleBits) string`

Конвертирует стиль в ANSI SGR код.

**Формат:**

```
\x1b[0;m  // сброс
[1;2;3;4;5;7;8;9;38;2;r;g;b;48;2;r;g;b]m
```

- `0` — сброс всех атрибутов
- `1` — Bold
- `2` — Dim
- `3` — Italic
- `4` — Underline
- `5` — Blink
- `7` — Reverse
- `8` — Hidden
- `9` — Strikethrough
- `38;2;r;g;b` — RGB foreground
- `48;2;r;g;b` — RGB background

### `sgrColor(c lipgloss.Color, bg bool) string`

Конвертирует `lipgloss.Color` в ANSI код:

- Hex цвета (`#RRGGBB`) → RGB SGR код
- Если цвет не hex → возвращает пустую строку

## Внутренние детали

### Scrollback Management

```
scrollback [][]Cell — выведенные строки
scrollbackCells int — число ячеек в буфере
scrollbackLimit int — лимит строк (по умолчанию 10000); общий cap — 1048576 ячеек
```

При добавлении новой строки:

1. Копируется содержимое верхней строки в scrollback
2. Удаляются старые строки при достижении лимита строк или 1048576 ячеек; объём сетки не зависит от ширины терминала

### Selection Logic

```
selStartRow, selStartCol — начало выделения
selEndRow, selEndCol     — конец выделения
selectionActive          — флаг активности
```

При перетаскивании влево по одной строке:

```
exclusiveStart = (startRow == endRow && startCol > endCol)
// ячейка под мышью (меньший col) исключается из выделения
```

### Render Optimization

Рендеринг оптимизирован для минимизации ANSI эскэпов:

- Стили применяются только при изменении
- Курсор/выделение получают `\x1b[7m` (reverse) + `\x1b[0m`
- В конце строки — сброс стилей

## Ограничения

1. **Scrollback** — по умолчанию не более 10000 строк и всегда не более 1048576 ячеек
2. **Cursor** всегда в пределах `0..Rows-1` × `0..Cols-1`
3. **Selection** работает только когда `selectionActive = true`
4. **Synchronized output** требует явного включения через `SetSync(true)`
5. **Hex цвета** поддерживаются только формат `#RRGGBB`

## Примеры использования

### Создание экрана

```go
s := NewScreen(24, 80)
s.SetCursor(0, 0)
s.Put('H')
```

### Управление прокруткой

```go
s.Put('X') // ... много Put() ...
s.ScrollUp() // строка поднимается в scrollback
```

### Выделение текста

```go
s.StartSelection(10, 5)
s.ExtendSelection(10, 15)
text := s.SelectionText() // ["X X X X X X X X X X X X X X X X"]
```

### Альтернативный экран

```go
s.EnterAltScreen()
s.Clear()
// отрисовка альтернативного экрана
s.ExitAltScreen() // восстановление
```

### Синхронизированный вывод

```go
s.SetSync(true)
// любые изменения не влияют на отображение
s.SetSync(false) // восстановление последнего кадра
```


## Hardening invariants

- \`NewScreen\` нормализует неположительный размер до 1×1; \`ResizeChecked\` отклоняет недопустимые размеры, а совместимый \`Resize\` оставляет экран без изменений.
- При \`?1049\` alternate screen основной buffer и terminal state сохраняются отдельно; G0/G1 charset восстанавливаются Parser.
- Full-screen margins сохраняют полный диапазон новой высоты; custom DECSTBM margins сохраняются при grow и clamp-ятся при shrink. Сохранённый cursor также clamp-ится.
- Resize согласованно меняет размеры active и saved buffer.
- Прокрутка alternate screen не добавляет строки в основной scrollback.
- При vertical shrink cursor-aware верхние строки добавляются в primary scrollback один раз, нижние строки сохраняются; активный и saved buffer вычисляют top-trim независимо.
- Перед wrap `Put` проверяет, продолжает ли rune предыдущий grapheme; wide glyph, расширяющийся у правого края, перемещается целиком.
- Keycap sequence с U+20E3 имеет display width 2, даже если общая Unicode width library сообщает 1.
- Размер одного grapheme cluster ограничен \`maxGraphemeBytes\` (4096 байт), чтобы combining/ZWJ flood не создавал неограниченное потребление памяти.
- Scrollback ограничен 1048576 ячейками; при уменьшении line limit \`viewOffset\` clamp-ится к новому размеру.
- Mouse state хранит DEC modes 1000/1002/1003 и SGR flag 1006; Emulator использует их для маршрутизации событий.


## Состояния DEC

- Основной и alternate buffers имеют независимые снимки экрана. \`?1049\` использует отдельный слот сохранения и не перезаписывает cursor из DECSC/DECRC.
- При входе в alternate screen сохраняются main cursor, scroll region, origin mode, autowrap и wrap-pending; при выходе они восстанавливаются и clamp-ятся к актуальным размерам.
- DECSC/DECRC сохраняет rendition cursor, а также origin/autowrap/wrap state.
- \`originMode\` ограничивает вертикальное позиционирование и движение областью DECSTBM.
- \`autoWrap\` по умолчанию включён; DECRST \`?7\` прекращает запись у правого края.

## Очистка с цветом фона

Erase, вставленные пустые ячейки/строки, строки после прокрутки и очистка всего
экрана используют текущий фон cursor (BCE), а не нулевой цвет ячейки. Это
сохраняет корректный вид цветных TUI при \`TERM=xterm-256color\`.

## Wide cells при изменении размера

После resize проверяются active, saved и scrollback rows. Если граница обрезает
wide base вместе с continuation, удаляются и осиротевшая continuation-ячейка,
и base, который больше не образует целый glyph.
