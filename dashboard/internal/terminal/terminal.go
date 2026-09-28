// Package terminal bridges a browser websocket to a login shell on a PTY. The shell runs
// as the person who logged in to the dashboard (runuser -l), never as root unless they
// log in as root.
package terminal

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// Messages from the browser: {"type":"input","data":"..."} or {"type":"resize","cols":80,"rows":24}.
// Output goes back as binary frames.
type message struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Serve upgrades the request and runs the shell until either side closes.
// When dev is true the shell is the current user's own $SHELL.
func Serve(w http.ResponseWriter, r *http.Request, up *websocket.Upgrader, username string, dev bool) {
	var cmd *exec.Cmd
	if dev {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		cmd = exec.Command(sh, "-l")
	} else {
		cmd = exec.Command("runuser", "-l", username)
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("could not start a shell: "+err.Error()+"\r\n"))
		return
	}
	log.Printf("terminal opened for %s", username)
	defer func() {
		f.Close()
		cmd.Process.Kill()
		cmd.Wait()
		log.Printf("terminal closed for %s", username)
	}()

	var wmu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				wmu.Lock()
				werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n])
				wmu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					wmu.Lock()
					conn.WriteMessage(websocket.TextMessage, []byte("\r\n[session ended]\r\n"))
					wmu.Unlock()
				}
				return
			}
		}
	}()

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				f.Close()
				return
			}
			var m message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "input":
				f.Write([]byte(m.Data))
			case "resize":
				if m.Cols > 0 && m.Rows > 0 {
					pty.Setsize(f, &pty.Winsize{Cols: m.Cols, Rows: m.Rows})
				}
			}
		}
	}()
	<-done
}
