# Final release gate Portalis — перед RC

## Контекст и решения

Аудированный baseline: `4b7005b08745e47d5c02a7f8985f5598e0879fb3`; локальная `main` и `origin/main` совпадали, worktree был чистым. GitHub CI run [36701840337](https://github.com/starframe-dev/portalis/actions/runs/36701840337) для этого baseline успешен; это не проверка локального diff.

Go versions сверены с официальной историей https://go.dev/doc/devel/release: Go 1.26.8 и Go 1.27.1 опубликованы. Минимум проекта — Go 1.26.8; staticcheck и govulncheck используют Go 1.27.1.

DECSTR сверялся с VT510 Programmer Information (https://vt100.net/docs/vt510-rm/DECSTR.html); alternate/cursor modes — с XTerm Control Sequences и Terminal Guide. Локальный контракт:

1. `?1047` переключает primary/alternate buffer и не использует явный DECSC/DECRC save slot. Отдельный screen-state основного/alternate buffer сохраняется.
2. `?1048` сохраняет/восстанавливает cursor position, rendition, cursor modes и активные G0/G1 charset через DECSC slot.
3. `?1049` входит в очищенный alternate buffer и использует независимый cursor/parser snapshot; DECSC/`?1048` slot не перезаписывается.
4. DECSTR (`CSI ! p`) сохраняет видимый текст/scrollback, текущую позицию курсора и tab stops; сбрасывает rendition, поддерживаемые terminal modes, scroll region к полному экрану, active charsets к default и DECSC slot к power-on/home. Экран не очищается; RIS (`ESC c`) остаётся полным reset.
5. Пустой OSC 0/2 очищает текущий title. Callback получает `""` один раз при изменении с непустого значения; повторы не дублируются.
6. `OnExit` выбран как вариант B: callback exactly once для принятого `Update` фактического child exit (`ProcessExited=true`). `Stop`/`Close` suppress callback, включая stale exit messages; host обязан поддерживать `Listen` chain и направлять сообщения в `Update`.
7. GitHub description не заявляет OSC 52. `main` требует PR и актуальные обычные CI checks, блокирует force-push/deletion; signed commits не требуются.

## Изменённые области

- `ansi.go`, `screen.go`, `ansi_test.go` — DEC 1047/1048/1049, DECSTR/RIS, OSC title clearing и regression tests.
- `emulator.go`, `emulator_batch_test.go`, `code-specs/emulator.md`, `MIGRATION.md` — документированный lifecycle `OnExit` и реальные process tests.
- `go.mod`, `.github/workflows/ci.yml`, `README.md`, `CHANGELOG.md`, `CONTEXT.md`, per-file specs и `specs/pre-release-hardening.md` — Go support, terminal/API claims и gate status.
- `docs/en/emulator.html` — исправлены stale public API, resize, TERM, callbacks и process lifecycle.

## Результаты проверок

- [x] Go 1.26.8 toolchain: `go test ./...` и 20× suite прошли.
- [x] Go 1.27.1: полный unit suite, полный `go test -race ./...`, `go vet ./...`, staticcheck v0.8.1, govulncheck v1.8.0 (`No vulnerabilities found`). Версии staticcheck/govulncheck встроенных модулей проверены.
- [x] Обязательные regressions: `TestDEC1047DoesNotUse1049CursorSaveSemantics`, `TestDEC1048SaveRestoreCursorState`, `TestDEC1049SavesAndRestoresCursorAndCharset`, `TestDEC1047And1049Differ`, `TestDECSTRPreservesCurrentCursorPosition`, `TestDECSTRDoesNotResetTabStopsLikeRIS`, `TestDECSTRResetsExpectedModes`, `TestDECSTRAndRISHaveDifferentSemantics`, `TestRISStillPerformsFullReset`, `TestOSCTitleCanBeCleared`.
- [x] OnExit: `TestOnExitNaturalExitExactlyOnce`, `TestOnExitNonZeroExitExactlyOnce`, `TestOnExitSignalExitExactlyOnce`, `TestOnExitStopSemantics`, `TestOnExitCloseSemantics`.
- [x] Точный stress `go test -race -run 'PTY|Resize|Close|Exit|Writer' -count=100 .` прошёл за 175.161s; дополнительно writer/backpressure и OnExit race stress `-count=10` прошли.
- [x] `FuzzParserFeed -fuzztime=60s`: PASS, 96 352 executions (57 new interesting inputs).
- [x] Локально cross-compiled test binaries на Go 1.26.8 для Linux/macOS amd64/arm64; `actionlint`, `gofmt -l .` и `git diff --check` прошли.
- [x] GitHub metadata перечитаны после изменения: description больше не заявляет OSC 52; branch protection включает PR, strict/up-to-date проверки `test (ubuntu-latest)`, `test (macos-latest)`, `static-analysis`, `pty-integration`, `vulnerability-scan`, запрет force push/deletion, approvals `0`, `required_signatures.enabled=false`.
- [x] PR #6 ordinary GitHub CI run [37151764966](https://github.com/starframe-dev/portalis/actions/runs/37151764966): `test (ubuntu-latest)`, `test (macos-latest)`, `static-analysis`, `pty-integration`, `vulnerability-scan` — все прошли. `release-verification` был ожидаемо skipped на `pull_request`.
- [x] Ручной `workflow_dispatch` run [37151877833](https://github.com/starframe-dev/portalis/actions/runs/37151877833) на code commit `033024aa3050c1b196bcce98b15b93be272ddf3f` завершился success; release matrix `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` прошла 4/4. В том же run также прошли ordinary CI jobs, включая Linux PTY runtime.

## Итог и ограничения

Кодовый commit `033024aa3050c1b196bcce98b15b93be272ddf3f` опубликован в ветке `chore/final-release-gate`, PR #6 открыт. Обычный PR CI и ручная release matrix 4/4 прошли; native Linux PTY runtime diff проверен в GitHub. PR ещё не влит в `main`; version tag и GitHub Release не создавались. После отдельного документационного follow-up результаты этого gate относятся к тому же runtime diff.

Clipboard integration tests не запускались (они заменяют системный clipboard); destructive cuTTY stress не запускался (он перегенерирует существующие artifacts). Pi/tmux rendering bug не диагностировался без reproduction steps и artifacts.
