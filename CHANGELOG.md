# Журнал изменений

## Не выпущено

### Изменено

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

Это changelog незавершённой pre-v1 ветки; он не является объявлением готовности к релизу.
