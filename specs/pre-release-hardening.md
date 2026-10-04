# Pre-release hardening — baseline verification record

## Цель

Этот документ хранит исторический статус базового hardening, а не выдаёт прошлые проверки за проверку текущей рабочей копии. Текущий финальный gate и acceptance criteria находятся в [`final-release-gate.md`](final-release-gate.md).

## Проверенная база

- Baseline: `4b7005b08745e47d5c02a7f8985f5598e0879fb3`; на момент аудита локальная `main` совпадала с `origin/main`, worktree был чистым.
- Обычный GitHub CI для этого commit завершился успешно. Проверка относится только к commit `4b7005b`; она не включает локальные изменения финального release gate.
- Этот baseline включает предшествующую работу по API encapsulation, PTY write/resize/lifecycle safety, OSC 7 structured paths и title callback, bounded trace/clipboard resources, parser fuzzing и терминальным reset/mode support.

## Базовые safety invariants

- Не обходить FIFO PTY writer; interactive writes должны оставаться ограниченными и не блокировать UI update.
- Resize меняет screen только после успешного `TIOCSWINSZ`; при ошибке сохраняются последние applied dimensions.
- `Pty.Close` дожидается worker goroutines; Linux `EIO` считается нормальным только при завершении процесса.
- Clipboard integration tests запускать только с `PORTALIS_RUN_CLIPBOARD_INTEGRATION=1`: они заменяют системный clipboard.
- Не запускать destructive cuetty ANSI stress над существующим `cuetty-artifacts/ansi-stress/` без разрешения на перегенерацию.
- Raw trace и clipboard temporary storage должны оставаться ограниченными, приватными и удаляться при закрытии сессии.

## Известные ограничения и открытые проверки

- Реализуется поднабор ANSI/VT, а не полный xterm; `TERM=ansi` не означает поддержку всех ANSI controls. В частности, `mc4/mc5` и G2/G3 designators не обещаются.
- На момент проверки baseline native Linux PTY/EIO и GitHub CI для последующего release-gate diff ещё не были подтверждены; позднее Linux PTY runtime и ordinary GitHub CI для release-gate commit прошли.
- На момент этой baseline записи release matrix для будущего diff не запускалась; позже ручной workflow_dispatch прошёл для Linux/macOS amd64/arm64 4/4. Подробности и run links приведены в [`final-release-gate.md`](final-release-gate.md).
- Pi/tmux rendering issue не диагностируется без воспроизводимых шагов и соответствующих artifacts.
- GitHub description обновлён и больше не заявляет OSC 52. `main` защищён PR, up-to-date обязательными checks `test (ubuntu-latest)`, `test (macos-latest)`, `static-analysis`, `pty-integration`, `vulnerability-scan`; force-push/deletion запрещены, signed commits не требуются. Настройки подтверждены read-back.

## Вывод

Успешный CI на baseline `4b7005b` подтверждает только состояние того опубликованного commit. Позже code commit `033024aa3050c1b196bcce98b15b93be272ddf3f` был опубликован в PR #6; ordinary GitHub CI, native Linux PTY runtime и ручная release matrix 4/4 прошли. Актуальные ссылки и результаты приведены в [`final-release-gate.md`](final-release-gate.md). PR остаётся открытым, version tag и GitHub Release не создавались.
