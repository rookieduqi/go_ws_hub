package main

import (
	"context"
	"echo_demo/download"
	"echo_demo/term"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// -----------------------
// 消息模型定义
// -----------------------

type WebSocketMessage struct {
	Type      string      `json:"t"`           // "request", "response", "notify", "ping", "pong"
	RequestID string      `json:"r,omitempty"` // 请求ID
	Action    string      `json:"a"`           // 操作，比如 "download"、"local"、"remote"
	Data      interface{} `json:"d,omitempty"` // 消息数据
}

const (
	MessageTypeRequest  = "request"
	MessageTypeResponse = "response"
	MessageTypeNotify   = "notify"
	MessageTypePing     = "ping"
	MessageTypePong     = "pong"
	MessageTypeLocal    = "local"
	MessageTypeRemote   = "remote"
)

// -----------------------
// 配置常量
// -----------------------

const (
	ReadDeadline         = 30 * time.Second
	AgentInitialDeadline = 30 * time.Second
	MaxAgentRetries      = 3
	InitialRetryInterval = 1 * time.Second
)

// -----------------------
// 全局 WS 升级器
// -----------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// -----------------------
// 前端连接（wsClientConn）
// -----------------------

type wsClientConn struct {
	conn *websocket.Conn
	send chan []byte
}

func (c *wsClientConn) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Println("Client write error:", err)
			return
		}
	}
}

// -----------------------
// Agent 连接（wsAgentConn）
// -----------------------

type wsAgentConn struct {
	conn *websocket.Conn
	send chan []byte
}

func (a *wsAgentConn) writePump() {
	defer a.conn.Close()
	for msg := range a.send {
		if err := a.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Println("Agent write error:", err)
			return
		}
	}
}

// -----------------------
// RelaySession：一个 token 对应一对连接
// -----------------------

type RelaySession struct {
	token string
	url   string

	client *wsClientConn
	agent  *wsAgentConn

	ctx    context.Context
	cancel context.CancelFunc

	clientMu sync.Mutex // 保护 client 的读写操作
	agentMu  sync.Mutex // 保护 agent 的读写操作
	stateMu  sync.Mutex // 保护状态更新，比如 agentReconnecting
	// 标识 agent 当前是否正在重连
	agentReconnecting bool

	once sync.Once // 确保 cleanup 只执行一次
}

// 处理本地事件，不转发给远程 agent
func (s *RelaySession) handleLocal(msg WebSocketMessage) {
	log.Println("Processing local event:", msg)
	response := WebSocketMessage{
		Type:      MessageTypeResponse,
		RequestID: msg.RequestID,
		Data:      fmt.Sprintf("Local processing result for data: %v", msg.Data),
	}
	respData, err := json.Marshal(response)
	if err != nil {
		log.Println("Local event marshal error:", err)
		return
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client != nil {
		s.client.send <- respData
	}
}

// clientReadLoop 处理前端发送的消息
func (s *RelaySession) clientReadLoop() {
	defer s.cleanup()
	for {
		// 检测 context 是否取消
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		msgType, data, err := s.client.conn.ReadMessage()
		if err != nil {
			log.Println("Client read error:", err)
			break
		}
		// 只处理文本消息
		if msgType != websocket.TextMessage {
			continue
		}
		// 处理心跳
		if strings.TrimSpace(string(data)) == MessageTypePing {
			s.client.send <- []byte(MessageTypePong)
			_ = s.client.conn.SetReadDeadline(time.Now().Add(ReadDeadline))
			continue
		}
		var msg WebSocketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Println("Client unmarshal error:", err)
			continue
		}
		// 根据 msg.Action 判断是本地还是远程处理
		if msg.Action == MessageTypeLocal {
			s.handleLocal(msg)
		} else {
			// 在转发前先检查 Agent 是否正在重连
			s.stateMu.Lock()
			reconnecting := s.agentReconnecting
			s.stateMu.Unlock()
			if reconnecting {
				notify := WebSocketMessage{
					Type:   MessageTypeNotify,
					Action: "reconnecting",
					Data:   "Agent connection is reconnecting, please wait",
				}
				notifyData, _ := json.Marshal(notify)
				s.client.send <- notifyData
				// 这里选择丢弃消息，也可考虑暂存消息等待 Agent 恢复后再发送
				continue
			}
			s.agentMu.Lock()
			if s.agent != nil {
				s.agent.send <- data
			} else {
				log.Println("Session", s.token, "has no agent connection")
			}
			s.agentMu.Unlock()
		}
	}
}

// agentReadLoop 处理远程 Agent 发来的消息，并实现重连逻辑（指数退避）
func (s *RelaySession) agentReadLoop() {
	retryCount := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		s.agentMu.Lock()
		curAgent := s.agent
		s.agentMu.Unlock()
		if curAgent == nil {
			log.Println("No agent connection present, exiting agentReadLoop")
			return
		}

		msgType, data, err := curAgent.conn.ReadMessage()
		if err != nil {
			log.Println("Agent read error:", err)
			retryCount++
			if retryCount > MaxAgentRetries {
				// 超过重试次数后发送通知给前端并退出
				notify := WebSocketMessage{
					Type:   MessageTypeNotify,
					Action: "exit",
					Data:   "Agent connection lost after maximum retries",
				}
				notifyData, _ := json.Marshal(notify)
				s.clientMu.Lock()
				if s.client != nil {
					s.client.send <- notifyData
				} else {
					log.Println("Session", s.token, "has no client connection")
				}
				s.clientMu.Unlock()
				time.Sleep(1 * time.Second)
				s.cleanup()
				return
			}
			// 标记 Agent 正在重连
			s.stateMu.Lock()
			s.agentReconnecting = true
			s.stateMu.Unlock()
			// 使用指数退避计算重试等待时间
			waitTime := time.Duration(math.Pow(2, float64(retryCount-1))) * InitialRetryInterval
			log.Printf("Attempting to reconnect agent, attempt %d, waiting %v", retryCount, waitTime)
			time.Sleep(waitTime)
			newConn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
			if err != nil {
				log.Println("Reconnect dial remote agent error:", err)
				continue
			}
			_ = newConn.SetReadDeadline(time.Now().Add(AgentInitialDeadline))
			newAgent := &wsAgentConn{
				conn: newConn,
				send: make(chan []byte, 1000),
			}
			go newAgent.writePump()
			s.agentMu.Lock()
			s.agent = newAgent
			s.agentMu.Unlock()
			// 重连成功后清除重连状态，并通知客户端
			s.stateMu.Lock()
			s.agentReconnecting = false
			s.stateMu.Unlock()
			notify := WebSocketMessage{
				Type:   MessageTypeNotify,
				Action: "reconnect_success",
				Data:   "Agent connection re-established",
			}
			notifyData, _ := json.Marshal(notify)
			s.clientMu.Lock()
			if s.client != nil {
				s.client.send <- notifyData
			}
			s.clientMu.Unlock()
			// 重连成功后继续后续逻辑
			continue
		}
		// 成功读取消息时重试计数器归零
		retryCount = 0

		if msgType != websocket.TextMessage {
			continue
		}
		// 处理 Agent 的心跳
		if strings.TrimSpace(string(data)) == "ping" {
			s.agentMu.Lock()
			if s.agent != nil {
				s.agent.send <- []byte(MessageTypePong)
			}
			s.agentMu.Unlock()
			_ = curAgent.conn.SetReadDeadline(time.Now().Add(ReadDeadline))
			continue
		}
		// 转发消息给客户端
		s.clientMu.Lock()
		if s.client != nil {
			s.client.send <- data
		} else {
			log.Println("Session", s.token, "has no client connection")
		}
		s.clientMu.Unlock()
	}
}

// cleanup 关闭整个会话，同时关闭 send 通道避免 goroutine 泄漏
func (s *RelaySession) cleanup() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.clientMu.Lock()
		if s.client != nil {
			s.client.conn.Close()
			close(s.client.send)
			s.client = nil
		}
		s.clientMu.Unlock()
		s.agentMu.Lock()
		if s.agent != nil {
			s.agent.conn.Close()
			close(s.agent.send)
			s.agent = nil
		}
		s.agentMu.Unlock()
		relayHub.removeSession(s.token)
	})
}

// cleanupClient 只清理前端连接
func (s *RelaySession) cleanupClient() {
	s.clientMu.Lock()
	if s.client != nil {
		s.client.conn.Close()
		close(s.client.send)
		s.client = nil
	}
	s.clientMu.Unlock()

	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.client == nil && s.agent == nil {
		relayHub.removeSession(s.token)
	}
}

// cleanupAgent 只清理 Agent 连接
func (s *RelaySession) cleanupAgent() {
	s.agentMu.Lock()
	if s.agent != nil {
		s.agent.conn.Close()
		close(s.agent.send)
		s.agent = nil
	}
	s.agentMu.Unlock()

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client == nil && s.agent == nil {
		relayHub.removeSession(s.token)
	}
}

// -----------------------
// RelayHub：管理所有会话
// -----------------------

type RelayHub struct {
	sessions map[string]*RelaySession
	mu       sync.Mutex
}

func NewRelayHub() *RelayHub {
	return &RelayHub{
		sessions: make(map[string]*RelaySession),
	}
}

func (h *RelayHub) getSession(token string) *RelaySession {
	h.mu.Lock()
	defer h.mu.Unlock()
	sess, exists := h.sessions[token]
	if !exists {
		sess = &RelaySession{token: token}
		h.sessions[token] = sess
	}
	return sess
}

func (h *RelayHub) removeSession(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, token)
}

var relayHub = NewRelayHub()

// -----------------------
// HTTP 入口：建立前端连接并主动拨号建立 Agent 连接
// -----------------------

func HandleConnection(c echo.Context) error {
	// 验证这个 token，然后在响应头中返回
	token := c.Request().Header.Get("Sec-WebSocket-Protocol")
	if token == "" {
		log.Println("token is empty")
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing token"})
	}
	respHeader := http.Header{
		"Sec-WebSocket-Protocol": []string{token},
	}

	// 升级前端 WS 连接
	clientConn, err := upgrader.Upgrade(c.Response(), c.Request(), respHeader)
	if err != nil {
		log.Println("Client upgrade error:", err)
		return err
	}
	client := &wsClientConn{
		conn: clientConn,
		send: make(chan []byte, 1000),
	}

	// 获取或创建 session
	session := relayHub.getSession(token)
	// 检查是否已有客户端连接
	session.clientMu.Lock()
	if session.client != nil {
		session.clientMu.Unlock()
		log.Printf("Session with token %s already has a client connected", token)
		clientConn.WriteMessage(websocket.TextMessage, []byte("Another client is already connected with this token"))
		clientConn.Close()
		return nil
	}
	session.client = client
	session.clientMu.Unlock()

	// 初始化 session 的 context
	if session.ctx == nil {
		ctx, cancel := context.WithCancel(context.Background())
		session.ctx = ctx
		session.cancel = cancel
	}

	// 建立与远程 Agent 的 WS 连接
	remoteAgentURL := fmt.Sprintf("ws://%s:8888/api/ws/stream", "39.98.44.36")
	//remoteAgentURL := "ws://127.0.0.1:8888/ws"
	agentConn, _, err := websocket.DefaultDialer.Dial(remoteAgentURL, nil)
	if err != nil {
		log.Println("Dial remote agent error:", err)
		clientConn.Close()
		return err
	}
	_ = agentConn.SetReadDeadline(time.Now().Add(AgentInitialDeadline))
	agent := &wsAgentConn{
		conn: agentConn,
		send: make(chan []byte, 1000),
	}
	session.agentMu.Lock()
	session.agent = agent
	session.agentMu.Unlock()

	// 设置 Agent 连接的 URL
	session.url = remoteAgentURL

	// 启动前端和 Agent 的写循环
	go client.writePump()
	go agent.writePump()

	// 启动双向中继处理
	go session.clientReadLoop()
	go session.agentReadLoop()

	return nil
}

// -----------------------
// Echo 路由设置
// -----------------------

func main() {
	e := echo.New()
	e.GET("/ws", HandleConnection)
	e.GET("/term", term.WsSSHHandler)

	fileGroup := e.Group("file")
	{
		fileGroup.GET("/download", download.DownloadSftpHandler)
	}

	log.Println("Relay server running on :8089")
	if err := e.Start(":8089"); err != nil {
		log.Fatal("Server run error:", err)
	}
}
