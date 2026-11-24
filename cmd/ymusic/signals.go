package main

import (
	"os"
	"syscall"
)

// SIGINT и SIGTERM обрабатывает сам bubbletea (QuitMsg); свой обработчик на них ставить нельзя —
// иначе bubbletea блокируется на отправке QuitMsg в уже остановленный цикл и процесс не завершается.
// SIGHUP (закрыли терминал) bubbletea не ловит — его обрабатываем сами.
var sigs = []os.Signal{syscall.SIGHUP}
