package main

import (
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/ssh"
)

type WsReader struct {
	Conn    *websocket.Conn
	Session *ssh.Session
}

type ResizeData struct {
	T string `json:"t"`
	W int    `json:"w"`
	H int    `json:"h"`
}

func (p *WsReader) Read(b []byte) (n int, err error) {
	for {
		msgType, reader, rErr := p.Conn.NextReader()
		if rErr != nil {
			return 0, rErr
		}
		if msgType != websocket.TextMessage {
			continue
		}

		// 解析消息
		currentMsg, readErr := io.ReadAll(reader)
		if readErr != nil {
			return 0, readErr
		}
		var msg ResizeData
		if jsonErr := json.Unmarshal(currentMsg, &msg); jsonErr != nil {
			return copy(b, currentMsg), nil
		}

		if msg.T != "resize" {
			continue
		}

		if chErr := p.Session.WindowChange(msg.H, msg.W); chErr != nil {
			return 0, chErr
		}
	}
}

type WsWriter struct {
	Conn    *websocket.Conn
	Session *ssh.Session
}

func (p *WsWriter) Write(b []byte) (n int, err error) {
	w, wErr := p.Conn.NextWriter(websocket.BinaryMessage)
	if wErr != nil {
		slog.Info("websocket write fail: " + wErr.Error())
		return 0, wErr
	}
	defer func(w io.WriteCloser) {
		cErr := w.Close()
		if cErr != nil && cErr.Error() != "EOF" {
			slog.Warn("websocket write close fail: " + cErr.Error())
		}
	}(w)
	slog.Info("write: ", "b", b, "n", n)
	return w.Write(b)
}

// 定义 WebSocket 升级器，允许跨域
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsSSHHandler 用于处理 WebSocket 连接，并通过 SSH 与远程服务器交互
func wsSSHHandler(c echo.Context) error {
	// 升级 HTTP 为 WebSocket 连接
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return err
	}
	defer ws.Close()

	// 配置 SSH 客户端参数
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// 建立 SSH 连接（这里示例连接到 remote.server.com:22）
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("SSH dial error: "+err.Error()))
		log.Println("SSH dial error:", err)
		return err
	}
	defer sshClient.Close()

	// 创建 SSH 会话
	session, err := sshClient.NewSession()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("SSH session error: "+err.Error()))
		log.Println("SSH session error:", err)
		return err
	}
	defer session.Close()

	// 请求伪终端，这样可以获得更真实的终端环境（比如 ls、top 等命令的执行）
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,     // 回显
		ssh.TTY_OP_ISPEED: 14400, // 输入速度
		ssh.TTY_OP_OSPEED: 14400, // 输出速度
	}
	if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Request pty error: "+err.Error()))
		log.Println("Request pty error:", err)
		return err
	}

	// 获取 SSH 会话的标准输出、标准错误以及标准输入
	stdout, err := session.StdoutPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Stdout pipe error: "+err.Error()))
		return err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Stderr pipe error: "+err.Error()))
		return err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Stdin pipe error: "+err.Error()))
		return err
	}

	// 启动一个交互式 shell
	if err := session.Shell(); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Shell start error: "+err.Error()))
		return err
	}

	// 开启一个 goroutine，将 SSH 的输出（stdout 与 stderr）读取后转发给前端 WebSocket
	go func() {
		// 合并 stdout 和 stderr
		reader := io.MultiReader(stdout, stderr)
		buf := make([]byte, 1024)
		for {
			n, err := reader.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Println("Error reading SSH output:", err)
				}
				break
			}
			if n > 0 {
				if err := ws.WriteMessage(websocket.TextMessage, buf[:n]); err != nil {
					log.Println("Error writing to WebSocket:", err)
					break
				}
			}
		}
	}()

	// 循环读取前端发送的命令，并写入 SSH 的标准输入
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Println("Error reading WebSocket message:", err)
			break
		}
		// 写入 SSH 会话，并附加换行符触发命令执行
		if _, err := stdin.Write(append(msg, '\n')); err != nil {
			log.Println("Error writing to SSH stdin:", err)
			break
		}
	}

	return nil
}

func main() {
	e := echo.New()
	// 注册 WebSocket 接口
	e.GET("/term", wsSSHHandler)
	log.Println("Server started on :8080")
	if err := e.Start(":8080"); err != nil {
		log.Fatal("Echo server error:", err)
	}
}
