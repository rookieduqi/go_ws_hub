package term

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/ssh"
)

type ResizeData struct {
	T string `json:"t"`
	W int    `json:"w"`
	H int    `json:"h"`
}

type WsOut struct {
	Code    int64  `json:"code"`
	Data    any    `json:"data"`
	Message string `json:"msg"`
}

// WsReader 从 WebSocket 读取数据，实现 io.Reader 接口
type WsReader struct {
	Conn    *websocket.Conn
	Session *ssh.Session
}

func (r *WsReader) Read(b []byte) (int, error) {
	for {
		msgType, reader, err := r.Conn.NextReader()
		if err != nil {
			return 0, err
		}
		if msgType != websocket.TextMessage {
			// 只处理文本消息
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return 0, err
		}
		// 尝试将消息解析为 JSON
		var resize ResizeData
		if jsonErr := json.Unmarshal(data, &resize); jsonErr == nil {
			// 如果解析成功，判断是否为 resize 命令
			if resize.T == "resize" {
				if err := r.Session.WindowChange(resize.H, resize.W); err != nil {
					return 0, err
				}
				// 调整窗口后继续等待下一个消息
				continue
			} else {
				// 如果是其它 JSON 数据，可根据需求处理，这里直接返回原始数据
				return copy(b, data), nil
			}
		} else {
			// 非 JSON 消息，直接返回原始数据
			return copy(b, data), nil
		}
	}
}

// WsWriter 将数据写入 WebSocket，实现 io.Writer 接口
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
	return w.Write(b)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func ReleaseSSHResources(client *ssh.Client, session *ssh.Session) {
	if session != nil {
		err := session.Close()
		if err != nil && err.Error() != "EOF" {
		}
	}

	if client != nil {
		err := client.Close()
		if err != nil {
		}
	}
}

// WsSSHHandler 处理 WebSocket 连接，并通过 SSH 与远程服务器交互
func WsSSHHandler(c echo.Context) error {
	// 升级 HTTP 为 WebSocket 连接
	out := &WsOut{}
	// 验证这个 token，然后在响应头中返回
	token := c.Request().Header.Get("Sec-WebSocket-Protocol")
	if token == "" {
		log.Println("token is empty")
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing token"})
	}
	respHeader := http.Header{
		"Sec-WebSocket-Protocol": []string{token},
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), respHeader)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return err
	}

	// 创建 context，用于监听关闭事件
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 设置关闭处理器，WebSocket 关闭时取消 context
	ws.SetCloseHandler(func(code int, text string) error {
		log.Printf("WebSocket close: %d %s", code, text)
		cancel()
		return nil
	})

	// 配置 SSH 客户端参数
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// 建立 SSH 连接
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("SSH dial error: "+err.Error()))
		log.Println("SSH dial error:", err)
		ws.Close()
		return err
	}
	defer sshClient.Close()

	// 创建 SSH 会话
	session, err := sshClient.NewSession()
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("SSH session error: "+err.Error()))
		log.Println("SSH session error:", err)
		ws.Close()
		return err
	}
	defer session.Close()

	// 请求伪终端
	modes := ssh.TerminalModes{
		ssh.ECHO: 1,
	}
	if err := session.RequestPty("xterm", 40, 80, modes); err != nil {
		out.Code = http.StatusBadRequest
		out.Message = "ssh连接获取失败"

		message, _ := json.Marshal(&out)
		_ = ws.WriteMessage(websocket.BinaryMessage, message)
		log.Println("Request pty error:", err)
		ws.Close()
		return err
	}

	// 创建自定义的 WsReader 和 WsWriter，并重定向 SSH I/O
	wsReader := &WsReader{Conn: ws, Session: session}
	wsWriter := &WsWriter{Conn: ws, Session: session}
	session.Stdin = wsReader
	session.Stdout = wsWriter
	session.Stderr = wsWriter

	// 启动交互式 shell
	if err := session.Shell(); err != nil {
		out.Code = http.StatusBadRequest
		out.Message = "shell终端打开失败"
		message, _ := json.Marshal(&out)
		_ = ws.WriteMessage(websocket.BinaryMessage, message)
		log.Println("Shell start error:", err)
		ws.Close()
		return err
	}

	// 使用 done 通道等待 SSH 会话结束
	done := make(chan error, 1)
	go func() {
		slog.Info("session.Wait() session exited")
		done <- session.Wait()
		slog.Info("session.Wait1() session exited")
	}()

	// 选择等待 SSH 会话结束或 WebSocket 关闭
	select {
	case err := <-done:
		slog.Info("session.Wait() session done")
		if err != nil {
			log.Println("SSH session ended with error:", err)
		}
		ws.Close()
		return err
	case <-ctx.Done():
		slog.Info("WS session done")
		// WebSocket 关闭，向 SSH 发送 SIGINT 尝试中断会话
		if err := session.Signal(ssh.SIGINT); err != nil {
			log.Println("Failed to signal SSH session:", err)
			return err
		}
		ws.Close()
		return nil
	}
}

//func main() {
//	e := echo.New()
//	e.GET("/term", wsSSHHandler)
//	log.Println("Server started on :8089")
//	if err := e.Start(":8089"); err != nil {
//		log.Fatal("Echo server error:", err)
//	}
//}
