package main

import (
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/ssh"
	"log"
	"net/http"
)

// WsReader 从 WebSocket 读取数据，实现 io.Reader 接口
type WsReader struct {
	Conn *websocket.Conn
}

func (r *WsReader) Read(p []byte) (int, error) {
	// 简单实现：每次读取一条消息
	_, msg, err := r.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	// 如果数据最后没有换行符，则追加一个换行符
	//if len(msg) > 0 && msg[len(msg)-1] != '\n' {
	//	msg = append(msg, '\n')
	//}
	n := copy(p, msg)
	return n, nil
}

// WsWriter 将数据写入 WebSocket，实现 io.Writer 接口
type WsWriter struct {
	Conn *websocket.Conn
}

func (w *WsWriter) Write(p []byte) (int, error) {
	err := w.Conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// TerminalSession 封装了 SSH 会话、SSH 客户端与 WebSocket 的交互
type TerminalSession struct {
	Ws       *websocket.Conn // 前端 WebSocket 连接
	SSH      *ssh.Session    // SSH 会话
	Client   *ssh.Client     // SSH 客户端，负责底层 TCP 连接
	WsReader *WsReader       // 自定义的 WebSocket 读器
	WsWriter *WsWriter       // 自定义的 WebSocket 写器
	CloseCh  chan struct{}   // 用于通知退出的通道
}

// Start 启动交互式 shell，并等待 SSH 会话结束
func (ts *TerminalSession) Start() {
	// 初始化关闭通道
	ts.CloseCh = make(chan struct{})

	// 启动 goroutine 等待 SSH 会话结束
	go func() {
		if err := ts.SSH.Wait(); err != nil {
			log.Printf("SSH session ended with error: %v", err)
		}
		close(ts.CloseCh)
		ts.Ws.Close()
	}()

	// 阻塞等待关闭信号
	<-ts.CloseCh
	ts.Close()
}

// Close 清理 TerminalSession 使用的所有资源
func (ts *TerminalSession) Close() {
	ts.Ws.Close()
	if ts.SSH != nil {
		ts.SSH.Close()
	}
	if ts.Client != nil {
		ts.Client.Close()
	}
}

// CreateTerminalSession 建立 SSH 连接、创建 SSH 会话并设置伪终端，重定向 I/O 到自定义读写器
func CreateTerminalSession(ws *websocket.Conn) (*TerminalSession, error) {
	// 配置 SSH 客户端参数（建议生产环境中使用密钥认证，并验证 HostKey）
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// 建立 SSH 连接（替换为实际地址）
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		return nil, err
	}

	// 创建 SSH 会话
	session, err := sshClient.NewSession()
	if err != nil {
		sshClient.Close()
		return nil, err
	}

	// 请求伪终端，获得交互式 shell 体验
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
		session.Close()
		sshClient.Close()
		return nil, err
	}

	// 创建自定义的 WebSocket 读写器
	wsReader := &WsReader{Conn: ws}
	wsWriter := &WsWriter{Conn: ws}

	// 将 SSH 会话的标准输入、输出和错误输出重定向到 WsReader/WsWriter
	session.Stdin = wsReader
	session.Stdout = wsWriter
	session.Stderr = wsWriter

	// 启动交互式 shell
	if err := session.Shell(); err != nil {
		session.Close()
		sshClient.Close()
		return nil, err
	}

	// 构造 TerminalSession 对象，包含 SSH 客户端以便后续释放
	ts := &TerminalSession{
		Ws:       ws,
		SSH:      session,
		Client:   sshClient,
		WsReader: wsReader,
		WsWriter: wsWriter,
	}
	return ts, nil
}

// TerminalHandler 使用 Echo 框架升级为 WebSocket，并建立 TerminalSession
func TerminalHandler(c echo.Context) error {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return err
	}
	// 注意：WebSocket 的关闭在 TerminalSession.Close() 中调用

	terminalSession, err := CreateTerminalSession(ws)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Terminal session error: "+err.Error()))
		log.Printf("CreateTerminalSession error: %v", err)
		return err
	}

	// 启动 TerminalSession，会阻塞直到 SSH 会话结束
	terminalSession.Start()
	return nil
}

func main() {
	e := echo.New()
	e.GET("/terminal", TerminalHandler)
	log.Println("Server started on :8080")
	if err := e.Start(":8080"); err != nil {
		log.Fatalf("Echo server error: %v", err)
	}
}
