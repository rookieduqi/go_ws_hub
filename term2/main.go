package term2

import (
	"bytes"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/ssh"
	"log"
	"net/http"
	"sync"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WsReader 从 WebSocket 读取数据，并使用内部缓冲区确保数据完整传递
type WsReader struct {
	Conn   *websocket.Conn
	buffer bytes.Buffer
}

func (r *WsReader) Read(p []byte) (int, error) {
	// 如果缓冲区已有数据，先读取缓冲区内容
	if r.buffer.Len() > 0 {
		return r.buffer.Read(p)
	}

	// 读取一条完整消息
	_, msg, err := r.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	// 将消息写入内部缓冲区
	r.buffer.Write(msg)
	return r.buffer.Read(p)
}

// WsWriter 将数据写入 WebSocket，并使用互斥锁保护写入操作
type WsWriter struct {
	Conn *websocket.Conn
	mu   sync.Mutex
}

func (w *WsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	if ts.Ws != nil {
		ts.Ws.Close()
	}
	if ts.SSH != nil {
		ts.SSH.Close()
	}
	if ts.Client != nil {
		ts.Client.Close()
	}
}

// CreateTerminalSession 建立 SSH 连接、创建 SSH 会话并设置伪终端，重定向 I/O 到自定义读写器
func CreateTerminalSession(ws *websocket.Conn) (*TerminalSession, error) {
	// 配置 SSH 客户端参数
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// 建立 SSH 连接
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

// TerminalHandler 升级为 WebSocket，并建立 TerminalSession
func TerminalHandler(c echo.Context) error {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return err
	}

	terminalSession, err := CreateTerminalSession(ws)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("Terminal session error: "+err.Error()))
		log.Printf("CreateTerminalSession error: %v", err)
		terminalSession.Close()
		return err
	}
	// 设置关闭回调，当 ws 主动关闭时，释放资源
	ws.SetCloseHandler(func(code int, text string) error {
		log.Printf("WebSocket closed with code: %d, text: %s", code, text)
		terminalSession.Close()
		return nil
	})

	// 启动 TerminalSession，会阻塞直到 SSH 会话结束
	terminalSession.Start()
	return nil
}

// ---------------------
// 自定义 token 验证函数
// ---------------------
func validateToken(token string) bool {
	return true
}

// ---------------------
// Token 验证中间件
// ---------------------
func tokenMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// 从请求 Header 中获取 token
		//token := c.Request().Header.Get("Sec-WebSocket-Protocol")
		token := c.Request().Header.Get("token")
		if token == "" || !validateToken(token) {
			// 如果 token 不合法，直接返回错误响应
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "Invalid or missing token",
			})
		}
		// Token 合法，继续后续处理
		return next(c)
	}
}

//func main() {
//	e := echo.New()
//	e.Use(middleware.Logger())
//	e.Use(middleware.Recover())
//
//	termGroup := e.Group("/term")
//	termGroup.Use(tokenMiddleware)
//	{
//		termGroup.GET("", TerminalHandler)
//	}
//	log.Println("Server started on :8080")
//	if err := e.Start(":8080"); err != nil {
//		log.Fatalf("Echo server error: %v", err)
//	}
//}
