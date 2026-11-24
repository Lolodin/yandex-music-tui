// ymusic — лёгкий терминальный клиент Яндекс Музыки: lossless, нормализация, bit-perfect, gapless/кроссфейд.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/cancelreader"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/desktop"
	"ymusic/internal/likes"
	"ymusic/internal/player"
	"ymusic/internal/stream"
	"ymusic/internal/ui"
)

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "путь к конфигу")
	relogin := flag.Bool("login", false, "войти заново (получить новый токен)")
	token := flag.String("token", "", "OAuth-токен (сохранится в конфиг)")
	offline := flag.Bool("offline", false, "офлайн-режим: только треки из кэша")
	flag.Parse()

	if err := run(*cfgPath, *relogin, *token, *offline); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func openLog(dir string) (io.WriteCloser, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "ymusic.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(p, p+".old")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func run(cfgPath string, relogin bool, token string, offline bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	logf, err := openLog(cfg.Cache.Dir)
	if err != nil {
		return err
	}
	defer logf.Close()
	log.SetOutput(logf)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("старт, конфиг %s", cfg.Path())

	if t := api.ExtractToken(token); t != "" {
		cfg.Token = t
	} else if env := os.Getenv("YM_TOKEN"); env != "" && cfg.Token == "" {
		cfg.Token = api.ExtractToken(env)
	}

	var account *api.AccountStatus
	client := api.New(cfg.Token)
	if !offline {
		for attempt := 0; ; attempt++ {
			if cfg.Token == "" || relogin {
				relogin = false
				t, err := login()
				if err != nil {
					return err
				}
				cfg.Token = t
				client = api.New(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			account, err = client.AccountStatus(ctx)
			cancel()
			if err == nil {
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("сохранение конфига: %w", err)
				}
				break
			}
			if api.IsUnauthorized(err) && attempt == 0 {
				fmt.Println("Токен не подошёл или истёк:", err)
				cfg.Token = ""
				continue
			}
			if api.IsNetwork(err) {
				fmt.Println("Нет связи с Яндекс Музыкой — запускаюсь в офлайн-режиме (только кэш).")
				log.Printf("офлайн: %v", err)
				account = nil
				time.Sleep(1200 * time.Millisecond)
				break
			}
			return err
		}
		if account != nil && !account.Plus.HasPlus {
			fmt.Println("Внимание: у аккаунта нет подписки Плюс — полные треки и lossless будут недоступны.")
			time.Sleep(1500 * time.Millisecond)
		}
	}

	cache, err := stream.NewCache(cfg.Cache.Dir, cfg.Cache.Enabled, cfg.Cache.MaxGB)
	if err != nil {
		return fmt.Errorf("кэш: %w", err)
	}
	srv, err := stream.NewServer(client, cache)
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.SetOffline(account == nil)

	eng, err := player.New(cfg.Audio, cfg.Quality, client, srv, logf)
	if err != nil {
		return err
	}
	defer eng.Close()

	likesStore := likes.New(client)
	model := ui.New(cfg, client, eng, cache, account, likesStore)
	p := tea.NewProgram(model, tea.WithAltScreen())

	// Трей и MPRIS: живут, пока открыт терминал с плеером.
	desktop.Start(eng, likesStore, p.Quit, desktop.Options{
		Tray:   cfg.Desktop.Tray,
		MPRIS:  cfg.Desktop.MPRIS,
		Online: account != nil,
	})

	// SIGHUP — закрыли терминал: выходим штатно, чтобы погасить mpv и сохранить настройки.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, sigs...)
	go func() {
		<-sig
		p.Quit()
	}()

	// Паника в фоновой горутине плеера не должна оставлять терминал в alt-screen и raw-режиме:
	// останавливаем TUI штатно (Kill восстанавливает терминал) и выходим с ошибкой.
	var crash atomic.Value
	eng.SetPanicHandler(func(r any) {
		crash.Store(fmt.Sprint(r))
		p.Kill()
	})

	_, err = p.Run()
	model.SaveConfig()
	if r := crash.Load(); r != nil {
		return fmt.Errorf("внутренняя ошибка плеера: %v (подробности в %s)", r, filepath.Join(cfg.Cache.Dir, "ymusic.log"))
	}
	return err
}

var errInputClosed = errors.New("ввод закрыт")

// login — вход по коду с другого устройства или вставкой токена.
func login() (string, error) {
	fmt.Println()
	fmt.Println("Вход в Яндекс Музыку")
	fmt.Println("────────────────────")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		token string
		err   error
	}
	results := make(chan result, 2)

	dc, err := api.RequestDeviceCode(ctx)
	if err == nil {
		fmt.Printf("1) Откройте %s и введите код: %s\n", dc.VerificationURL, dc.UserCode)
		fmt.Println("   (жду подтверждения…)")
		go func() {
			t, err := api.PollDeviceToken(ctx, dc)
			results <- result{t, err}
		}()
		fmt.Println()
		fmt.Println("2) Или вставьте OAuth-токен (можно целиком ссылку с #access_token=…) и нажмите Enter.")
	} else {
		log.Printf("device code: %v", err)
		fmt.Println("Откройте ссылку, войдите и скопируйте адрес страницы, куда вас перенаправит (в нём будет #access_token=…):")
		fmt.Println("  " + api.TokenURL)
		fmt.Println("Вставьте токен или эту ссылку и нажмите Enter:")
	}
	fmt.Print("> ")

	// Чтение stdin должно закончиться вместе с login(): иначе забытая горутина продолжит читать терминал
	// параллельно с TUI и будет забирать нажатия клавиш. Поэтому ридер отменяемый.
	var in io.Reader = os.Stdin
	cr, err := cancelreader.NewReader(os.Stdin)
	if err == nil {
		in = cr
	}
	done := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		send := func(r result) {
			select {
			case results <- r:
			case <-done:
			}
		}
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			if t := api.ExtractToken(sc.Text()); t != "" {
				send(result{token: t})
				return
			}
			fmt.Print("> ")
		}
		send(result{err: errInputClosed})
	}()
	defer func() {
		close(done)
		if cr != nil {
			cr.Cancel()
			<-readerDone
			_ = cr.Close()
		}
	}()

	for {
		r := <-results
		if r.err != nil {
			if dc != nil && !errors.Is(r.err, errInputClosed) {
				fmt.Println("\nВход по коду не удался:", r.err)
				fmt.Print("Вставьте токен вручную > ")
				dc = nil
				continue
			}
			return "", r.err
		}
		fmt.Println("\nГотово, токен получен.")
		return r.token, nil
	}
}
