package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Публичные OAuth-креды Android-приложения Яндекс Музыки (используются всеми сторонними клиентами).
const (
	oauthClientID     = "23cabbbdc6cd418abb4b39c32c41195d"
	oauthClientSecret = "53bc75238f0c4d08a118e51fe9203300"
	oauthBase         = "https://oauth.yandex.ru"
)

// ErrAuthPending — пользователь ещё не подтвердил вход.
var ErrAuthPending = errors.New("authorization_pending")

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// TokenURL — ссылка для ручного получения токена: после входа токен будет во фрагменте адреса (#access_token=...).
const TokenURL = oauthBase + "/authorize?response_type=token&client_id=" + oauthClientID

func randomDeviceID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func oauthPost(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "authorization_pending" {
			return ErrAuthPending
		}
		return fmt.Errorf("oauth %d: %s %s", resp.StatusCode, e.Error, e.Description)
	}
	return json.Unmarshal(data, out)
}

// RequestDeviceCode начинает вход по коду: пользователь вводит user_code на странице verification_url.
func RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	var dc DeviceCode
	form := url.Values{
		"client_id":   {oauthClientID},
		"device_id":   {randomDeviceID()},
		"device_name": {"ymusic-tui"},
	}
	if err := oauthPost(ctx, "/device/code", form, &dc); err != nil {
		return nil, err
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	if dc.ExpiresIn <= 0 {
		dc.ExpiresIn = 300
	}
	return &dc, nil
}

// PollDeviceToken ждёт подтверждения входа и возвращает OAuth-токен.
func PollDeviceToken(ctx context.Context, dc *DeviceCode) (string, error) {
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	tick := time.NewTicker(time.Duration(dc.Interval) * time.Second)
	defer tick.Stop()
	for {
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		form := url.Values{
			"grant_type":    {"device_code"},
			"code":          {dc.DeviceCode},
			"client_id":     {oauthClientID},
			"client_secret": {oauthClientSecret},
		}
		err := oauthPost(ctx, "/token", form, &tok)
		switch {
		case err == nil && tok.AccessToken != "":
			return tok.AccessToken, nil
		case err != nil && !errors.Is(err, ErrAuthPending):
			return "", err
		}
		if time.Now().After(deadline) {
			return "", errors.New("код истёк, запустите вход заново")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

// ExtractToken достаёт токен из вставленной строки: голый токен или URL с #access_token=...
func ExtractToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "access_token="); i >= 0 {
		s = s[i+len("access_token="):]
		if j := strings.IndexAny(s, "&# "); j >= 0 {
			s = s[:j]
		}
	}
	return s
}
