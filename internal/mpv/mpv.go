// Package mpv запускает mpv и управляет им через JSON IPC по socketpair (работает и со snap-версией mpv).
package mpv

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Event — событие mpv. Для property-change заполнены ID, Name и Data.
type Event struct {
	Event   string          `json:"event"`
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Data    json.RawMessage `json:"data"`
	Reason  string          `json:"reason"`
	EntryID int64           `json:"playlist_entry_id"`
	FileErr string          `json:"file_error"`
}

type reply struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	ReqID int64           `json:"request_id"`
}

var ErrClosed = errors.New("mpv завершился")

type Client struct {
	cmd  *exec.Cmd
	conn *os.File

	wmu     sync.Mutex
	reqID   atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan reply

	events chan Event
	done   chan struct{}
}

// Start запускает mpv с указанными аргументами; IPC подключается через унаследованный fd 3.
func Start(path string, args []string, stderr io.Writer) (*Client, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "mpv-ipc")
	child := os.NewFile(uintptr(fds[1]), "mpv-ipc-child")

	full := append([]string{
		"--no-config",
		"--idle=yes",
		"--no-video",
		"--no-terminal",
		"--input-ipc-client=fd://3",
		"--input-default-bindings=no",
		"--audio-display=no",
		"--audio-client-name=ymusic",
		"--msg-level=all=warn",
	}, args...)
	cmd := exec.Command(path, full...)
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		return nil, fmt.Errorf("запуск %s: %w", path, err)
	}
	child.Close()

	c := &Client{
		cmd:     cmd,
		conn:    parent,
		pending: map[int64]chan reply{},
		events:  make(chan Event, 512),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	go func() { _ = cmd.Wait(); parent.Close() }()

	// Проверим, что IPC живой.
	if _, err := c.Command("get_property", "mpv-version"); err != nil {
		c.Close()
		return nil, fmt.Errorf("mpv не отвечает по IPC (нужен mpv ≥ 0.35): %w", err)
	}
	return c, nil
}

func (c *Client) readLoop() {
	defer func() {
		close(c.done)
		c.pmu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pmu.Unlock()
		close(c.events)
	}()
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Event string `json:"event"`
			ReqID *int64 `json:"request_id"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Event != "" {
			var ev Event
			if json.Unmarshal(line, &ev) == nil {
				select {
				case c.events <- ev:
				default: // не блокируем чтение ответов, если потребитель отстал
				}
			}
			continue
		}
		if probe.ReqID != nil {
			var r reply
			if json.Unmarshal(line, &r) != nil {
				continue
			}
			c.pmu.Lock()
			ch := c.pending[r.ReqID]
			delete(c.pending, r.ReqID)
			c.pmu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
	}
}

// Events — поток событий; закрывается, когда mpv завершается.
func (c *Client) Events() <-chan Event { return c.events }

// Done закрывается при завершении mpv.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) send(cmd any) (json.RawMessage, error) {
	id := c.reqID.Add(1)
	ch := make(chan reply, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()

	msg, err := json.Marshal(map[string]any{"command": cmd, "request_id": id})
	if err != nil {
		return nil, err
	}
	msg = append(msg, '\n')
	c.wmu.Lock()
	_, err = c.conn.Write(msg)
	c.wmu.Unlock()
	if err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return nil, ErrClosed
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if r.Error != "success" {
			return nil, fmt.Errorf("mpv: %s", r.Error)
		}
		return r.Data, nil
	case <-time.After(5 * time.Second):
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return nil, errors.New("mpv: таймаут ответа")
	case <-c.done:
		return nil, ErrClosed
	}
}

// Command — команда с позиционными аргументами.
func (c *Client) Command(args ...any) (json.RawMessage, error) {
	return c.send(args)
}

// CommandNamed — команда с именованными аргументами, например loadfile с options.
func (c *Client) CommandNamed(name string, args map[string]any) (json.RawMessage, error) {
	m := map[string]any{"name": name}
	for k, v := range args {
		m[k] = v
	}
	return c.send(m)
}

func (c *Client) Set(prop string, v any) error {
	_, err := c.Command("set_property", prop, v)
	return err
}

func (c *Client) Get(prop string, out any) error {
	data, err := c.Command("get_property", prop)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Observe(id int64, prop string) error {
	_, err := c.Command("observe_property", id, prop)
	return err
}

// LoadFile добавляет файл; flags: replace | append | append-play. Возвращает playlist_entry_id.
func (c *Client) LoadFile(url, flags string, options map[string]string) (int64, error) {
	args := map[string]any{"url": url, "flags": flags}
	if len(options) > 0 {
		args["options"] = options
	}
	data, err := c.CommandNamed("loadfile", args)
	if err != nil {
		return 0, err
	}
	var r struct {
		EntryID int64 `json:"playlist_entry_id"`
	}
	_ = json.Unmarshal(data, &r)
	return r.EntryID, nil
}

func (c *Client) Close() {
	_, _ = c.Command("quit")
	select {
	case <-c.done:
	case <-time.After(time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
}

// AudioOutputs — список доступных AO (pulse, pipewire, alsa…) у данного mpv.
func AudioOutputs(path string) ([]string, error) {
	out, err := exec.Command(path, "--no-config", "--ao=help").Output()
	if err != nil {
		return nil, err
	}
	var aos []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(line, "  ") {
			aos = append(aos, f[0])
		}
	}
	return aos, nil
}

// Device — устройство вывода из audio-device-list.
type Device struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
