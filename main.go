package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"log"
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
	Action    string      `json:"a"`           // 操作，比如 "download"
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
	for {
		msg, ok := <-c.send
		if !ok {
			_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}

		//_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
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
	for {
		msg, ok := <-a.send
		if !ok {
			_ = a.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}
		//_ = a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
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
	token  string
	client *wsClientConn
	agent  *wsAgentConn
	mu     sync.Mutex // 保护 client 与 agent 的设置
}

// 前端读循环，将消息转发给 agent
func (s *RelaySession) clientReadLoop() {
	defer s.cleanup()
	for {
		msgType, data, err := s.client.conn.ReadMessage()
		if err != nil {
			log.Println("Client read error:", err)
			break
		}
		// 只处理文本消息
		if msgType != websocket.TextMessage {
			continue
		}

		if strings.TrimSpace(string(data)) == "ping" {
			s.client.send <- []byte(MessageTypePong)
			_ = s.client.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			continue
		}

		var msg WebSocketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Println("Client unmarshal error:", err)
			continue
		}
		// 转发其他消息给 agent
		s.mu.Lock()
		if s.agent != nil {
			s.agent.send <- data
		} else {
			log.Println("Session", s.token, "has no agent connection")
		}
		s.mu.Unlock()
	}
}

// Agent 读循环，将消息转发给前端
func (s *RelaySession) agentReadLoop() {
	defer s.cleanup()
	for {
		msgType, data, err := s.agent.conn.ReadMessage()
		if err != nil {
			log.Println("Agent read error:", err)
			break
		}
		// 只处理文本消息
		if msgType != websocket.TextMessage {
			continue
		}

		// 如果接收到的是纯字符串 "ping"
		if strings.TrimSpace(string(data)) == "ping" {
			s.agent.send <- []byte(MessageTypePong)
			_ = s.agent.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			continue
		}

		s.mu.Lock()
		if s.client != nil {
			s.client.send <- data
		} else {
			log.Println("Session", s.token, "has no client connection")
		}
		s.mu.Unlock()
	}
}

// cleanup 关闭本会话所有连接，并从 Hub 中移除该会话
func (s *RelaySession) cleanup() {
	s.mu.Lock()
	if s.client != nil {
		s.client.conn.Close()
	}
	if s.agent != nil {
		s.agent.conn.Close()
	}
	s.mu.Unlock()
	relayHub.removeSession(s.token)
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
// HTTP 入口：单一接口建立前端连接及主动拨号建立 Agent 连接
// -----------------------

func HandleConnection(c echo.Context) error {
	// 获取 token 参数
	token := c.Request().Header.Get("token")
	if token == "" {
		fmt.Println("token is empty")
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing token"})
	}

	// 升级前端 WS 连接
	clientConn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		log.Println("Client upgrade error:", err)
		return err
	}

	// 设置读取超时为 30 秒
	_ = clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	client := &wsClientConn{
		conn: clientConn,
		send: make(chan []byte, 1000),
	}

	// 根据 token 主动拨号建立与远程 Agent 的 WS 连接
	remoteAgentURL := "ws://172.24.65.130:8888/api/v1/ws/stream"
	agentConn, _, err := websocket.DefaultDialer.Dial(remoteAgentURL, nil)
	if err != nil {
		log.Println("Dial remote agent error:", err)
		clientConn.Close()
		return err
	}

	_ = agentConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	agent := &wsAgentConn{
		conn: agentConn,
		send: make(chan []byte, 1000),
	}

	// 将前端和 Agent 连接保存到同一 RelaySession 中
	session := relayHub.getSession(token)
	session.mu.Lock()
	session.client = client
	session.agent = agent
	session.mu.Unlock()

	// 启动前端和 Agent 的写循环
	go client.writePump()
	go agent.writePump()

	// 启动双向中继：分别读取前端和 Agent 消息
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
	log.Println("Relay server running on :8080")
	if err := e.Start(":8080"); err != nil {
		log.Fatal("Server run error:", err)
	}
}
