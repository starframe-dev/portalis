# Спецификация последнего pre-release hardening pass

## Цель

Подготовить `github.com/starframe-dev/portalis` к первому стабильному semver-релизу: закрыть выявленные риски PTY, resize, clipboard, памяти и public API; сопоставить terminal capabilities с реализацией; сделать CI и документацию проверяемыми. Изменения выполняются до v1, поэтому не сохраняем ошибочные публичные escape hatches ценой безопасности инвариантов.

Baseline: `27c3706be7838c17f7cf2f59167ce5c55b78a1ec` (`main`, локальный и GitHub HEAD совпадали в начале pass). Последний доступный GitHub CI для baseline завершился успешно. Ветка `main` на baseline была не защищена; tags и `CHANGELOG.md` отсутствовали. Во время первоначального read-only аудита исходники не менялись; текущие uncommitted изменения описаны в разделе «Ход реализации».

## Ход реализации (uncommitted)

После исходной проверки выполнена локальная реализация шагов 3–7; версия проекта пока не утверждена и релиз не выполняется.

- Public API: `Screen` state и `Pty` handles/queues приватизированы; `Emulator.Pty()` удалён, его заменяет `PtyState`; callbacks работают через setters. OSC 7 callback передаёт `WorkingDirectory{Host, Path, Local}`, добавлены OSC 0/2 title callbacks и копирующий `CommandHistorySnapshot`. Добавлены внешние API regression tests и русская `MIGRATION.md`.
- Resize: запрошенные dimensions теперь отделены от последнего применённого размера. Активный PTY сначала проходит `TIOCSWINSZ`, затем обновляет Screen; ioctl failure остаётся non-fatal и не меняет уже применённый размер. Устаревшая ошибка повторяет latest resize generation; начальный resize failure проходит через `PtyReadyMsg.ResizeErr`.
- Clipboard: Wayland выбирает только объявленные MIME types; PNG получает decoded-memory budget 128 MiB; временные изображения находятся в private per-emulator directory с лимитами файлов/байтов/TTL и очисткой.
- Terminal: default TERM изменён на `ansi`; добавлены tab stops, insert mode, RIS/DECSTR, режимы 1047/1048 и title OSC; DA2 теперь generic без версии xterm. OSC 7 сохраняет remote host. Не поддерживается полный ANSI/VT, включая terminfo printer controls `mc4/mc5`.
- PTY writer: интерактивные keyboard/mouse writes ставятся в bounded reserve без ожидания I/O и queue capacity; FIFO и атомарность paste payload сохраняются. Overload report non-terminal; фактическая ошибка системной записи завершает PTY.
- Trace: raw/chunk traces ограничены размером и числом ротаций, создаются с `0600`, trace failures идут через non-fatal warning path.
- Fuzz сравнивает parser/screen semantic state при произвольной сегментации. `Pty.Close` ждёт завершения reader и writer goroutines. CI actions закреплены SHA; добавлен tag/manual cross-compilation matrix Linux/macOS amd64/arm64. Обновлены README, changelog и per-file specs.

Это не итоговая verification запись: race/static/security/cross-platform матрица и final HEAD будут зафиксированы ниже после выполнения.

## Выводы независимой проверки

1. `handleKey` и mouse reporting синхронно вызывают `Pty.WriteForGeneration`. Когда ограниченная очередь или PTY backpressure задерживает запись, блокируется Bubble Tea `Update`; paste-команда может удерживать очередь до передачи десятков мегабайт.
2. Resize заранее меняет логический экран до подтверждения `TIOCSWINSZ`; ошибка ioctl превращается в `PtyExitMsg`, а новое поколение resize не перепробуется после ошибки старого. Screen и фактический winsize могут разойтись.
3. `Screen.Rows/Cols/Cells/Cursor`, каналы `Pty.Output/Errors`, `Emulator.Pty()` и публичные callback/session поля обходят блокировки и внутренние инварианты. `WriteForGeneration` также раскрывает служебный lifecycle token. Позиционные литералы `PtyExitMsg` уже уязвимы к добавлению полей.
4. PNG ограничен размером файла и числом пикселей, но не максимальным decoded byte count; лимит 25 млн пикселей допускает сотни MiB decoded image. Wayland backend сначала угадывает тип по сигнатуре, не проверяя MIME.
5. Clipboard image temp files глобальны в `/tmp`, chmod выполняется после создания файла, а число/суммарный объём/TTL файлов ограничены только закрытием Emulator.
6. OSC 7 сводит удалённый URI к строке пути; host теряется. Встроенная command history извлекается из скриншота prompt/screen и не является надёжным shell history API.
7. `TERM=xterm-256color` объявляет терминал шире фактической реализации: как минимум не хватает tab-stop controls (`ESC H`, `CSI g`), insert mode (`CSI 4 h/l`) и полного reset поведения. Текущие DA ответы зашиты строками, один из них выглядит как ответ конкретной версии xterm.
8. Raw trace защищён mode `0600`, но не ограничен по размеру и числу файлов; ошибки/ротация не сообщаются callback-ом.
9. Fuzz сравнивает только цельную подачу и один midpoint split; CI actions указаны movable tags `@v7`. GitHub description ошибочно заявляет OSC 52, changelog отсутствует, README называет API framework-agnostic, хотя он импортирует Bubble Tea типы. `main` не защищён. Успешные GitHub CI runs для `27c3706` подтверждены read-only запросом.

## Требования реализации

### PTY writer и lifecycle

- Сохранить общий FIFO writer: клавиатурные/мышиные/terminal-reply записи не перемешиваются с paste payload и не переставляются местами.
- Событие Bubble Tea не ждёт завершения PTY I/O и не блокируется при нехватке byte budget. Для интерактивной очереди предусмотреть bounded reserve и fail-fast при реальном overload; ошибку доставлять как нетерминальную PTY error message, не завершать сессию.
- Clipboard paste остаётся атомарным, включая bracketed-paste delimiters: не вклинивать keystrokes внутрь paste. Допустимая задержка до доставки child ограничивается существующим размерным лимитом; UI остаётся отзывчивым.
- `Close` должен будить producer-ов, закрывать writer и reader, завершать дочерний процесс и ждать обеих goroutine. Ошибки short write проверяются отдельным writer-тестом.
- Добавить тесты: 100 конкурентных писателей без разрезания payload; partial writes; насыщенная очередь плюс `Close`; stale generation; restart/Stop/Close гонки; активный большой paste вместе с быстрым `Update` клавиши; повторить lifecycle-тесты под race detector.

### Resize

- Разделить запрошенный и последний успешно применённый размер.
- При успешном `TIOCSWINSZ` синхронно фиксировать тот же размер в `Screen`; при ошибке сохранять предыдущий успешно применённый размер, отправлять нетерминальную ошибку и оставлять PTY живым.
- Если новое поколение resize появляется, пока выполняется старое, повторять актуальный размер; поздняя ошибка устаревшего поколения не отменяет более новое.
- Тестами проверить 1000 сообщений resize, инъекцию ошибок ioctl, resize во время старта/останова, отсутствие redundant SIGWINCH и согласованность Screen/winsize.

### Public API до v1

- Инкапсулировать изменяемые поля `Screen` и `Pty`, заменить их безопасными getters/snapshot-методами; скрыть служебный write generation.
- Удалить сырой `Emulator.Pty()`; предоставить `PtyState` snapshot с `Running`, `PID`, `Rows`, `Cols`.
- Сделать `SessionID`, `ChatName` и callback storage закрытыми; добавить getters и setter-методы под mutex. Добавить `OnExit` callback с exit code/signal/process-exited status.
- Предоставить `WorkingDirectory{Host, Path, Local}`: не терять host OSC 7 URI; `CWD()` оставить совместимым convenience getter. Старый screen-scraped `CommandHistory` переименовать/описать как heuristic, не обещать полноценный shell history.
- Документировать все breaking changes и примеры миграции до v1. Именованные литералы сообщений остаются предпочтительными; поля exit message после фиксации v1 не расширять без версии/API type.

### Clipboard, temp files и память

- Wayland backend запрашивает список MIME types; text читается как text MIME, PNG — только при точном `image/png`; бинарные данные не угадываются по сигнатуре.
- Ввести независимый decoded image byte cap (цель — не более 128 MiB worst-case RGBA64) и проверять его по `DecodeConfig` до `Decode`; сохранить byte/pixel/time лимиты.
- На Emulator выделять private temp directory `0700`; каждый файл создавать атомарно `0600`. Удалять директорию при close/stop/завершении PTY; держать настраиваемые лимиты количества, суммарных байтов и TTL с тестируемыми defaults.
- Лимитировать grapheme payload так, чтобы максимальные активные/alternate/scrollback cells имели конечный byte bound; сохранить лимит 1 048 576 scrollback cells. Зафиксировать worst-case estimate в документации.
- Не выполнять live clipboard integration tests без `PORTALIS_RUN_CLIPBOARD_INTEGRATION=1`.

### Terminal semantics и заявляемые capabilities

- Выбрать консервативный стандартный `TERM`, покрываемый встроенным поведением (`ansi`), сохранить явный caller override и добавить проверку его среды/terminfo.
- Реализовать и проверить `ESC H`, `CSI 0g/3g`, `CSI 4h/4l`, RIS (`ESC c`), DECSTR (`CSI ! p`), `?1047/?1048`, title OSC, а также корректные DA1/DA2 ответы без незаслуженных xterm-version claims. Не заявлять неподдержанные features.
- Проверить `infocmp` capabilities; документировать точный совместимый subset и оставшиеся unsupported sequences.
- Fuzz подавать те же bytes с произвольными границами чанков; сравнивать cells, cursor, modes, buffers и parser state; проверять инварианты размеров/wide cells.

### Trace, CI и релизная гигиена

- Ограничить raw trace настраиваемым размером, ротацией и максимальным числом файлов; приватные права сохранить. Сообщать rotation/write/open ошибки через warning message/callback. Тестировать квоту, ротацию, права и cleanup.
- Закрепить GitHub Actions по полным commit SHA; добавить ручную/tag-triggered release verification для Linux/macOS и поддерживаемых amd64/arm64 сборок; оставить pinned staticcheck/govulncheck и race/fuzz проверки.
- Создать русскоязычные изменения specs и пользовательскую `CHANGELOG.md`; README исправить по API, TERM, history, clipboard, trace и supported OS. Точные сведения о release version добавить только после выбора версии.
- После локальной проверки настроить protection `main`: required status checks, PR-only, up-to-date branch, запрет force push и удаления. Число обязательных approvals нужно уточнить отдельно; signed commits не требовать без отдельного решения.
- Исправить GitHub description: удалить неподдерживаемый OSC 52 и заменить ложное утверждение об OSC/feature/framework neutrality фактическим описанием.

## Проверка и критерии успеха

- После каждого тематического code change запускать адресные regression tests; затем `gofmt -l .`, `go test ./...`, `go test -race ./...`, `go vet ./...`, pinned `staticcheck`, `govulncheck`, `actionlint`, `git diff --check`.
- Stress: выбранные PTY lifecycle/resize/writer тесты с `-count=100` под race detector; fuzz smoke локально не менее 30 секунд и CI не менее 5 секунд.
- Проверить cross-compile test binaries для `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`; реальные GitHub checks отдельно дождаться после push, если изменения будут опубликованы.
- Clipboard-интеграция и cuetty stress с удалением существующих artifacts остаются opt-in и в этом pass не запускаются.
- Не создавать commit, tag или GitHub Release самостоятельно. Финальный verdict должен отделять локально проверенную готовность от ещё не прошедших GitHub checks и не выполненных production-настроек.

## Фактическая проверка и verdict — 2026-09-29

- Baseline и final `HEAD`/`origin/main`: `27c3706be7838c17f7cf2f59167ce5c55b78a1ec`. Изменения остались только в рабочем дереве; commit, tag, push и release не выполнялись.
- Успешно: `env -u PORTALIS_RUN_CLIPBOARD_INTEGRATION go test ./...`; та же команда с `-race`; PTY lifecycle/resize с `-race -count=100`; writer/backpressure/Close с `-race -count=10`; `FuzzParserFeed -fuzztime=30s` (93 207 исполнений); `go vet ./...`; staticcheck module v0.8.1; govulncheck module v1.8.0 (`No vulnerabilities found`); `actionlint`; `gofmt -l .`; `git diff --check`.
- Успешно cross-compiled test binaries для Linux и macOS, `amd64` и `arm64`, на локальном Go 1.27.0 darwin/arm64.
- Не запускались clipboard integration tests (они заменяют системный clipboard) и destructive cuetty stress (пересоздаёт существующие artifacts). В более раннем opt-in clipboard run содержимое могло быть заменено на `hello world`; исходное содержимое не было сохранено, текущая проверка clipboard не меняла.
- Нативный Linux PTY/EIO runtime не запускался на macOS; cross-compilation не заменяет Linux runtime CI. GitHub checks для нового diff отсутствуют, потому что ничего не публиковалось.
- Pi/tmux rendering diagnosis заблокирован отсутствием точных шагов воспроизведения и артефактов. GitHub branch protection и repository description не менялись и требуют отдельного production confirmation.

Локальная реализация hardening и проверенная verification matrix завершены. **Релизная готовность не подтверждена:** остаются GitHub CI на опубликованном diff, production-настройки, Linux runtime check и решение пользователя по API-breaking pre-v1 changes. Никакого релиза или коммита без прямого подтверждения.
