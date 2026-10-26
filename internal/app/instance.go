package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Instance records the running tray app so a second launch (a blite:// link)
// can hand it the link instead of starting another copy.
type Instance struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func instancePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "blite", "instance.json"), nil
}

// WriteInstance saves the running instance's port and token (mode 0600).
func WriteInstance(in Instance) error {
	p, err := instancePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(in)
	return os.WriteFile(p, b, 0o600)
}

// RemoveInstance deletes the instance file if it is ours.
func RemoveInstance(pid int) {
	p, err := instancePath()
	if err != nil {
		return
	}
	if in, err := readInstance(p); err == nil && in.PID == pid {
		os.Remove(p)
	}
}

func readInstance(p string) (Instance, error) {
	var in Instance
	b, err := os.ReadFile(p)
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal(b, &in)
}

// ErrNoInstance means no running tray app answered.
var ErrNoInstance = errors.New("no running instance")

// ForwardLink hands a join link to an already running instance. With link == ""
// it only asks that instance to show its UI.
func ForwardLink(link string) (Instance, error) {
	p, err := instancePath()
	if err != nil {
		return Instance{}, err
	}
	in, err := readInstance(p)
	if err != nil {
		return in, ErrNoInstance
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", in.Port)
	c := http.Client{Timeout: 25 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, base+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+in.Token)
	res, err := c.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		return in, ErrNoInstance
	}
	res.Body.Close()
	if link == "" {
		return in, nil
	}
	body, _ := json.Marshal(map[string]string{"link": link})
	req, _ = http.NewRequest(http.MethodPost, base+"/api/join", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("X-Blite", "1")
	req.Header.Set("Content-Type", "application/json")
	res, err = c.Do(req)
	if err != nil {
		return in, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		return in, errors.New(e.Error)
	}
	return in, nil
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
