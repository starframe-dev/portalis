# Журнал изменений

## Не выпущено

### Изменено

- Минимальная версия Go повышена до 1.26.8; текущие staticcheck и govulncheck запускаются на Go 1.27.1.
- Разведены семантики DECSET `?1047`, `?1048` и `?1049`; добавлены независимый cursor/parser save для 1049 и regression tests.
- DECSTR сохраняет экран, позицию курсора и tab stops, сбрасывая поддерживаемые режимы; RIS остаётся полным reset.
- Пустой OSC 0/2 очищает текущий title и уведомляет callback один раз при фактическом изменении.
- Зафиксирован `OnExit`: callback ровно один раз при доставленном child-exit событии; Stop/Close его подавляют.

- Скрыты изменяемые поля `Screen`, внутренние каналы и generation API PTY, callback/session storage `Emulator`; вместо сырого `Emulator.Pty()` предоставлен snapshot состояния.
- OSC 7 теперь возвращает `WorkingDirectory{Host, Path, Local}`; добавлена поддержка OSC 0/2 title callback.
- Значение `TERM` по умолчанию изменено с `xterm-256color` на `ansi`; DA2 больше не заявляет версию конкретного xterm.
- Клавиатурные и мышиные записи ставятся в ограниченную интерактивную очередь без ожидания завершения PTY write; общая FIFO-последовательность сохраняется.
- Добавлены tab-stop controls, insert mode, RIS, DECSTR и режимы alternate buffer/cursor save.
- Документировано, что command capture эвристически считывает видимую строку и не является shell history.

### Безопасность и надёжность

- Для clipboard PNG ограничена worst-case память декодирования; Wayland проверяет объявленный MIME type. Clipboard временные файлы хранятся в приватной директории с ограничениями числа, размера и TTL.
- PTY raw trace получил приватное создание, квоты размера, ограниченную ротацию и non-fatal предупреждения.
- Fuzz-проверка сравнивает semantic state при произвольных границах входных чанков.
- GitHub Actions закреплены полными commit SHA; добавлена ручная/tag-triggered cross-compilation матрица Linux/macOS amd64/arm64.

Эта секция фиксирует unreleased изменения и не объявляет о создании тега или выпуске релиза.
